// Package docker is a minimal client for the Docker Engine API.
//
// It speaks plain HTTP over the Engine's unix socket rather than using the
// official SDK. The SDK pulls in a large part of the Moby source tree, and
// docKontroler needs about a dozen endpoints — so the whole client fits in a few
// hundred lines and the project keeps zero external dependencies.
package docker

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"
)

// APIVersion is the Engine API version docKontroler negotiates. 1.43 ships with
// Docker 24 (mid-2023) and covers everything used here, so it is old enough to
// work on the kind of engine a home server actually runs.
const APIVersion = "1.43"

// minAPIVersion is the oldest engine we accept. Below this, container update
// and the network fields used by recreate behave differently enough that
// failing loudly at startup beats misbehaving later.
const minAPIVersion = "1.41"

// Client talks to a Docker Engine over a unix socket.
type Client struct {
	http *http.Client
	// baseURL carries a placeholder host: the transport dials the socket and
	// ignores it, but net/http still requires a syntactically valid URL.
	baseURL string
}

// New returns a Client bound to the unix socket at socketPath.
//
// No connection is made here; call Ping to verify the socket is reachable.
func New(socketPath string) *Client {
	transport := &http.Transport{
		// Every request, whatever host it names, is dialled as this socket.
		DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
			var d net.Dialer
			return d.DialContext(ctx, "unix", socketPath)
		},
		// A local socket does not need a large connection pool.
		MaxIdleConns:          4,
		IdleConnTimeout:       30 * time.Second,
		ResponseHeaderTimeout: 30 * time.Second,
	}
	return &Client{
		// No Timeout on the client itself: per-call deadlines come from the
		// context, which lets slow operations (stop, recreate) set their own.
		http:    &http.Client{Transport: transport},
		baseURL: "http://docker/v" + APIVersion,
	}
}

// NewWithBaseURL returns a Client that talks to an ordinary HTTP base URL instead
// of a unix socket, e.g. "http://127.0.0.1:2375".
//
// This is the seam the tests use: it lets the whole manager be exercised against
// an httptest server, so the recreate logic can be checked without a Docker
// daemon. baseURL must not already carry the API version prefix.
func NewWithBaseURL(baseURL string) *Client {
	return &Client{
		http:    &http.Client{},
		baseURL: strings.TrimSuffix(baseURL, "/") + "/v" + APIVersion,
	}
}

// APIError is a non-success reply from the Engine. The Message field is the
// Engine's own wording ("port is already allocated", "no such image"), which is
// usually specific enough to show to the user unchanged.
type APIError struct {
	Status  int
	Method  string
	Path    string
	Message string
}

func (e *APIError) Error() string {
	if e.Message != "" {
		return fmt.Sprintf("docker: %s (HTTP %d on %s %s)", e.Message, e.Status, e.Method, e.Path)
	}
	return fmt.Sprintf("docker: HTTP %d on %s %s", e.Status, e.Method, e.Path)
}

// IsNotFound reports whether err is an APIError with status 404, i.e. the
// container, image or network is gone.
func IsNotFound(err error) bool {
	var apiErr *APIError
	return errors.As(err, &apiErr) && apiErr.Status == http.StatusNotFound
}

// IsConflict reports whether err is an APIError with status 409. The Engine uses
// it for "name already in use" and for operations invalid in the current state.
func IsConflict(err error) bool {
	var apiErr *APIError
	return errors.As(err, &apiErr) && apiErr.Status == http.StatusConflict
}

// do performs one Engine call.
//
// body, if non-nil, is sent as JSON. out, if non-nil, receives the decoded JSON
// reply. HTTP 304 is treated as success: the Engine uses it for "the container
// was already in the requested state", which is not an error for our purposes.
func (c *Client) do(ctx context.Context, method, path string, query url.Values, body, out any) error {
	var reqBody io.Reader
	if body != nil {
		encoded, err := json.Marshal(body)
		if err != nil {
			return fmt.Errorf("encode request body for %s %s: %w", method, path, err)
		}
		reqBody = bytes.NewReader(encoded)
	}

	fullURL := c.baseURL + path
	if len(query) > 0 {
		fullURL += "?" + query.Encode()
	}

	req, err := http.NewRequestWithContext(ctx, method, fullURL, reqBody)
	if err != nil {
		return fmt.Errorf("build request %s %s: %w", method, path, err)
	}
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}

	resp, err := c.http.Do(req)
	if err != nil {
		return fmt.Errorf("%s %s: %w", method, path, err)
	}
	defer resp.Body.Close()

	if resp.StatusCode >= 300 && resp.StatusCode != http.StatusNotModified {
		return newAPIError(resp, method, path)
	}

	if out == nil || resp.StatusCode == http.StatusNoContent || resp.StatusCode == http.StatusNotModified {
		// Drain so the connection can be reused.
		_, _ = io.Copy(io.Discard, resp.Body)
		return nil
	}
	if err := json.NewDecoder(resp.Body).Decode(out); err != nil {
		return fmt.Errorf("decode response of %s %s: %w", method, path, err)
	}
	return nil
}

// newAPIError reads the Engine's error body, which is {"message": "..."}.
func newAPIError(resp *http.Response, method, path string) *APIError {
	apiErr := &APIError{Status: resp.StatusCode, Method: method, Path: path}

	raw, err := io.ReadAll(io.LimitReader(resp.Body, 8<<10))
	if err != nil || len(raw) == 0 {
		return apiErr
	}
	var payload struct {
		Message string `json:"message"`
	}
	if json.Unmarshal(raw, &payload) == nil && payload.Message != "" {
		apiErr.Message = payload.Message
	} else {
		apiErr.Message = strings.TrimSpace(string(raw))
	}
	return apiErr
}

// PingResult describes the engine we are talking to.
type PingResult struct {
	APIVersion string // e.g. "1.45"
	OSType     string // e.g. "linux"
}

// Ping verifies the socket is reachable and reports the Engine's own API
// version.
//
// The request deliberately skips the /v1.43 prefix: a versioned request against
// an engine older than that prefix fails with "client version is too new",
// which would hide the far more useful "your Docker is too old" message.
func (c *Client) Ping(ctx context.Context) (PingResult, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, "http://docker/_ping", nil)
	if err != nil {
		return PingResult{}, fmt.Errorf("build ping request: %w", err)
	}

	resp, err := c.http.Do(req)
	if err != nil {
		return PingResult{}, fmt.Errorf("cannot reach the Docker socket: %w", err)
	}
	defer resp.Body.Close()
	_, _ = io.Copy(io.Discard, resp.Body)

	if resp.StatusCode >= 300 {
		return PingResult{}, newAPIError(resp, http.MethodGet, "/_ping")
	}

	result := PingResult{
		APIVersion: resp.Header.Get("Api-Version"),
		OSType:     resp.Header.Get("Ostype"),
	}
	if result.APIVersion == "" {
		// Very old or proxied engines may omit the header. Continuing is more
		// useful than refusing to start over a missing diagnostic.
		return result, nil
	}
	older, err := versionLess(result.APIVersion, minAPIVersion)
	if err != nil {
		return result, nil
	}
	if older {
		return result, fmt.Errorf(
			"docker engine API %s is too old, dockontroler needs at least %s",
			result.APIVersion, minAPIVersion)
	}
	return result, nil
}

// versionLess compares two "major.minor" Engine API versions.
func versionLess(a, b string) (bool, error) {
	aMajor, aMinor, err := parseVersion(a)
	if err != nil {
		return false, err
	}
	bMajor, bMinor, err := parseVersion(b)
	if err != nil {
		return false, err
	}
	if aMajor != bMajor {
		return aMajor < bMajor, nil
	}
	return aMinor < bMinor, nil
}

func parseVersion(v string) (major, minor int, err error) {
	parts := strings.SplitN(strings.TrimPrefix(v, "v"), ".", 2)
	if len(parts) != 2 {
		return 0, 0, fmt.Errorf("malformed API version %q", v)
	}
	if major, err = strconv.Atoi(parts[0]); err != nil {
		return 0, 0, fmt.Errorf("malformed API version %q: %w", v, err)
	}
	if minor, err = strconv.Atoi(parts[1]); err != nil {
		return 0, 0, fmt.Errorf("malformed API version %q: %w", v, err)
	}
	return major, minor, nil
}
