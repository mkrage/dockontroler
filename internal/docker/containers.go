package docker

import (
	"context"
	"fmt"
	"math"
	"net/http"
	"net/url"
	"strconv"
	"time"
)

// containerPath builds /containers/<id><suffix> with the id escaped. Callers may
// pass a full id, a 12-character short id or a name — the Engine resolves all
// three.
func containerPath(id, suffix string) string {
	return "/containers/" + url.PathEscape(id) + suffix
}

// seconds renders a duration for the Engine's "t" parameter, which counts whole
// seconds and treats 0 as "kill immediately".
func seconds(d time.Duration) string {
	if d <= 0 {
		return "0"
	}
	return strconv.Itoa(int(math.Ceil(d.Seconds())))
}

// ListContainers returns every container, running or not.
func (c *Client) ListContainers(ctx context.Context) ([]ContainerSummary, error) {
	query := url.Values{"all": {"true"}}
	var containers []ContainerSummary
	if err := c.do(ctx, http.MethodGet, "/containers/json", query, nil, &containers); err != nil {
		return nil, err
	}
	return containers, nil
}

// InspectContainer returns the full configuration of one container.
func (c *Client) InspectContainer(ctx context.Context, id string) (*ContainerInspect, error) {
	var inspected ContainerInspect
	if err := c.do(ctx, http.MethodGet, containerPath(id, "/json"), nil, nil, &inspected); err != nil {
		return nil, err
	}
	return &inspected, nil
}

// StartContainer starts a container. Starting an already running container is
// not an error; the Engine answers 304 and the call succeeds.
func (c *Client) StartContainer(ctx context.Context, id string) error {
	return c.do(ctx, http.MethodPost, containerPath(id, "/start"), nil, nil, nil)
}

// StopContainer asks a container to stop, giving it timeout to exit on its own
// before it is killed. Stopping an already stopped container is not an error.
func (c *Client) StopContainer(ctx context.Context, id string, timeout time.Duration) error {
	query := url.Values{"t": {seconds(timeout)}}
	return c.do(ctx, http.MethodPost, containerPath(id, "/stop"), query, nil, nil)
}

// RestartContainer stops and starts a container.
//
// Note this reuses the existing container, so it does *not* pick up a rebuilt
// image — the container is bound to the image id it was created from. Use
// manager.Recreate for that.
func (c *Client) RestartContainer(ctx context.Context, id string, timeout time.Duration) error {
	query := url.Values{"t": {seconds(timeout)}}
	return c.do(ctx, http.MethodPost, containerPath(id, "/restart"), query, nil, nil)
}

// SetRestartPolicy changes a container's restart policy. It works on running and
// stopped containers alike and takes effect without a restart.
//
// Any warnings the Engine returns are passed back so callers can surface them.
func (c *Client) SetRestartPolicy(ctx context.Context, id, policy string) ([]string, error) {
	switch policy {
	case PolicyNo, PolicyAlways, PolicyUnlessStopped:
	default:
		return nil, fmt.Errorf("unsupported restart policy %q", policy)
	}

	body := struct {
		RestartPolicy RestartPolicy `json:"RestartPolicy"`
	}{RestartPolicy: RestartPolicy{Name: policy}}

	var result UpdateResponse
	if err := c.do(ctx, http.MethodPost, containerPath(id, "/update"), nil, body, &result); err != nil {
		return nil, err
	}
	return result.Warnings, nil
}

// CreateContainer creates a container under the given name.
func (c *Client) CreateContainer(ctx context.Context, name string, body CreateBody) (CreateResponse, error) {
	query := url.Values{"name": {name}}
	var result CreateResponse
	if err := c.do(ctx, http.MethodPost, "/containers/create", query, body, &result); err != nil {
		return CreateResponse{}, err
	}
	return result, nil
}

// RenameContainer changes a container's name.
func (c *Client) RenameContainer(ctx context.Context, id, newName string) error {
	query := url.Values{"name": {newName}}
	return c.do(ctx, http.MethodPost, containerPath(id, "/rename"), query, nil, nil)
}

// RemoveContainer deletes a container, stopping it first if needed.
//
// removeVolumes must stay false when replacing a container during a recreate:
// passing true deletes the anonymous volumes attached to it, which is how a
// naive recreate destroys a database.
func (c *Client) RemoveContainer(ctx context.Context, id string, removeVolumes bool) error {
	query := url.Values{
		"force": {"true"},
		"v":     {strconv.FormatBool(removeVolumes)},
	}
	return c.do(ctx, http.MethodDelete, containerPath(id, ""), query, nil, nil)
}
