// Package web serves docKontroler's browser interface.
//
// Pages are rendered server-side with html/template and enhanced by a small
// amount of hand-written JavaScript. There is no bundler and no node_modules:
// templates and assets are compiled into the binary with embed, so the runtime
// image contains a single file.
package web

import (
	"crypto/sha256"
	"embed"
	"encoding/hex"
	"fmt"
	"html/template"
	"io"
	"io/fs"
	"log/slog"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/mkrage/dockontroler/internal/manager"
)

//go:embed templates/*.html
var templateFS embed.FS

//go:embed static
var staticFS embed.FS

// Server holds everything the handlers need.
type Server struct {
	manager   *manager.Manager
	log       *slog.Logger
	templates *template.Template

	refreshInterval time.Duration
	// assetVersion is a hash of the embedded assets, appended to their URLs so a
	// new binary never serves stale CSS from a browser cache.
	assetVersion string
}

// New parses the embedded templates and returns a ready Server.
func New(containers *manager.Manager, log *slog.Logger, refreshInterval time.Duration) (*Server, error) {
	templates, err := template.ParseFS(templateFS, "templates/*.html")
	if err != nil {
		return nil, fmt.Errorf("parse templates: %w", err)
	}

	version, err := hashAssets()
	if err != nil {
		return nil, fmt.Errorf("hash static assets: %w", err)
	}

	return &Server{
		manager:         containers,
		log:             log,
		templates:       templates,
		refreshInterval: refreshInterval,
		assetVersion:    version,
	}, nil
}

// Handler returns the router for the whole interface.
//
// The route patterns use the method and wildcard syntax added to net/http in Go
// 1.22, which is why no third-party router is needed.
func (s *Server) Handler() http.Handler {
	mux := http.NewServeMux()

	// "/{$}" matches only the root path; a bare "/" would swallow every unknown
	// URL and answer it with the container list.
	mux.HandleFunc("GET /{$}", s.handleIndex)
	mux.HandleFunc("GET /partials/containers", s.handleFragment)
	mux.HandleFunc("GET /api/containers", s.handleAPIContainers)
	mux.HandleFunc("GET /healthz", s.handleHealth)

	// Every state-changing route is wrapped, because none of them may be driven by
	// another site; see requireSameOrigin.
	action := func(pattern string, handler http.HandlerFunc) {
		mux.Handle("POST "+pattern, s.requireSameOrigin(handler))
	}
	action("/containers/{id}/start", s.handleAction("start"))
	action("/containers/{id}/stop", s.handleAction("stop"))
	action("/containers/{id}/restart", s.handleAction("restart"))
	action("/containers/{id}/recreate", s.handleAction("recreate"))
	action("/containers/{id}/policy", s.handlePolicy)
	action("/stacks/{project}/start", s.handleStack("start"))
	action("/stacks/{project}/stop", s.handleStack("stop"))
	action("/stacks/{project}/restart", s.handleStack("restart"))

	mux.Handle("GET /static/", s.staticHandler())

	return s.recoverPanics(mux)
}

// staticHandler serves the embedded assets. Their URLs carry an asset-version
// query parameter, so they are safe to cache aggressively.
func (s *Server) staticHandler() http.Handler {
	fileServer := http.FileServer(http.FS(staticFS))
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Cache-Control", "public, max-age=31536000, immutable")
		fileServer.ServeHTTP(w, r)
	})
}

// requireSameOrigin refuses state-changing requests that a browser marks as
// coming from somewhere else.
//
// docKontroler has no login by design, so a POST is authorised by nothing but
// being reachable — which makes the browser of anyone on the network a usable
// deputy. A form on any other page can post here, cross-origin form posts need no
// CORS preflight, and the manager resolves container names as well as ids, so an
// attacker does not even need to know one. Binding to the LAN does not help
// against that; this does.
//
// It is deliberately not a login: the page's own forms and fetches are
// same-origin, and curl or a script sends neither header, so both keep working
// unchanged.
//
// What it does not stop is DNS rebinding: an attacker who points a hostname of
// their own at this host's address is same-origin as far as the browser is
// concerned, and their page can then read and post freely. Catching that means
// checking the Host header against the name this instance is supposed to answer
// to, which needs somebody to configure that name — filter it in a reverse proxy
// if it matters to you.
func (s *Server) requireSameOrigin(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch site := r.Header.Get("Sec-Fetch-Site"); {
		case site == "same-origin":
			// The page itself.
		case site != "":
			s.forbidCrossOrigin(w, r, "Sec-Fetch-Site: "+site)
			return
		case r.Header.Get("Origin") != "":
			// Not a legacy fallback: this is the branch that does the work in the
			// deployment this tool is built for. Browsers attach Sec-Fetch-* only
			// to potentially trustworthy URLs, so a plain http:// LAN address
			// never receives them, and Origin on the form post is all there is to
			// go on. Hosts are compared rather than whole origins, so a
			// TLS-terminating proxy — https outside, http here — does not trip
			// over its own scheme.
			if origin, err := url.Parse(r.Header.Get("Origin")); err != nil ||
				!strings.EqualFold(origin.Host, r.Host) {
				s.forbidCrossOrigin(w, r, "Origin: "+r.Header.Get("Origin"))
				return
			}
		}
		next.ServeHTTP(w, r)
	})
}

func (s *Server) forbidCrossOrigin(w http.ResponseWriter, r *http.Request, reason string) {
	s.log.Warn("refused a cross-origin action",
		"method", r.Method, "path", r.URL.Path, "reason", reason)
	http.Error(w, "cross-origin requests are refused", http.StatusForbidden)
}

// recoverPanics turns a panicking handler into a 500 instead of a dropped
// connection, and logs it with the route that caused it.
func (s *Server) recoverPanics(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		defer func() {
			if recovered := recover(); recovered != nil {
				s.log.Error("handler panicked", "method", r.Method, "path", r.URL.Path, "panic", recovered)
				http.Error(w, "internal error", http.StatusInternalServerError)
			}
		}()
		next.ServeHTTP(w, r)
	})
}

// hashAssets fingerprints the embedded static files.
func hashAssets() (string, error) {
	digest := sha256.New()
	err := fs.WalkDir(staticFS, "static", func(path string, entry fs.DirEntry, err error) error {
		if err != nil || entry.IsDir() {
			return err
		}
		file, err := staticFS.Open(path)
		if err != nil {
			return err
		}
		defer file.Close()

		if _, err := io.WriteString(digest, path); err != nil {
			return err
		}
		_, err = io.Copy(digest, file)
		return err
	})
	if err != nil {
		return "", err
	}
	return hex.EncodeToString(digest.Sum(nil))[:12], nil
}
