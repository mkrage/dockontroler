// Package web serves Dockontroler's browser interface.
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

	mux.HandleFunc("POST /containers/{id}/start", s.handleAction("start"))
	mux.HandleFunc("POST /containers/{id}/stop", s.handleAction("stop"))
	mux.HandleFunc("POST /containers/{id}/restart", s.handleAction("restart"))
	mux.HandleFunc("POST /containers/{id}/recreate", s.handleAction("recreate"))
	mux.HandleFunc("POST /containers/{id}/policy", s.handlePolicy)

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
