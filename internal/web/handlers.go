package web

import (
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"strings"

	"github.com/mkrage/dockontroler/internal/manager"
)

// pageData is the model for the full page template.
type pageData struct {
	Overview      manager.Overview
	RefreshMillis int64
	AssetVersion  string
	// SelfProtected is false when Dockontroler could not identify its own
	// container, meaning it cannot stop the UI from shutting itself down. Worth a
	// visible banner rather than a silent gap.
	SelfProtected bool
	Flash         *flashMessage
}

type flashMessage struct {
	Level string // "ok" or "error"
	Text  string
}

func (s *Server) handleIndex(w http.ResponseWriter, r *http.Request) {
	overview, err := s.manager.List(r.Context())
	if err != nil {
		s.log.Error("could not list containers", "error", err)
		http.Error(w, "Cannot reach the Docker daemon: "+manager.UserMessage(err), http.StatusBadGateway)
		return
	}

	data := pageData{
		Overview:      overview,
		RefreshMillis: s.refreshInterval.Milliseconds(),
		AssetVersion:  s.assetVersion,
		SelfProtected: s.manager.SelfID() != "",
		Flash:         flashFromQuery(r.URL.Query()),
	}

	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	// The overview reflects live daemon state, so it must never be cached.
	w.Header().Set("Cache-Control", "no-store")
	if err := s.templates.ExecuteTemplate(w, "page", data); err != nil {
		// Too late for a status code — the response is already streaming.
		s.log.Error("could not render the page", "error", err)
	}
}

// handleFragment serves just the container list, which is what the auto-refresh
// fetches. Rendering it from the same template as the full page keeps the markup
// in one place instead of rebuilding rows in JavaScript.
func (s *Server) handleFragment(w http.ResponseWriter, r *http.Request) {
	overview, err := s.manager.List(r.Context())
	if err != nil {
		s.log.Error("could not list containers", "error", err)
		http.Error(w, manager.UserMessage(err), http.StatusBadGateway)
		return
	}

	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.Header().Set("Cache-Control", "no-store")
	if err := s.templates.ExecuteTemplate(w, "containers", overview); err != nil {
		s.log.Error("could not render the container list", "error", err)
	}
}

// handleAPIContainers exposes the overview as JSON. Nothing in the UI needs it —
// it is there so users can script against Dockontroler.
func (s *Server) handleAPIContainers(w http.ResponseWriter, r *http.Request) {
	overview, err := s.manager.List(r.Context())
	if err != nil {
		writeJSON(w, http.StatusBadGateway, actionResponse{Message: manager.UserMessage(err)})
		return
	}
	writeJSON(w, http.StatusOK, overview)
}

func (s *Server) handleHealth(w http.ResponseWriter, _ *http.Request) {
	w.Header().Set("Content-Type", "text/plain; charset=utf-8")
	_, _ = w.Write([]byte("ok\n"))
}

// handleAction builds the handler for start, stop, restart and recreate.
func (s *Server) handleAction(action string) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		id := r.PathValue("id")

		var (
			message string
			err     error
		)
		switch action {
		case "start":
			err = s.manager.Start(r.Context(), id)
			message = "Container started."
		case "stop":
			err = s.manager.Stop(r.Context(), id)
			message = "Container stopped."
		case "restart":
			err = s.manager.Restart(r.Context(), id)
			message = "Container restarted."
		case "recreate":
			var result manager.RecreateResult
			result, err = s.manager.Recreate(r.Context(), id)
			message = joinMessage("Container recreated.", result.Notes)
		default:
			// Unreachable: actions come from the route table, not from input.
			http.Error(w, "unknown action", http.StatusNotFound)
			return
		}

		s.finishAction(w, r, action, id, message, err)
	}
}

func (s *Server) handlePolicy(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")

	if err := r.ParseForm(); err != nil {
		s.finishAction(w, r, "policy", id, "", fmt.Errorf("%w: malformed form data", manager.ErrUnsupported))
		return
	}
	policy := r.PostFormValue("policy")

	err := s.manager.SetPolicy(r.Context(), id, policy)
	s.finishAction(w, r, "policy", id, "Restart policy set to "+policy+".", err)
}

// finishAction reports the outcome of an action in whichever form the caller
// expects, and logs failures.
func (s *Server) finishAction(w http.ResponseWriter, r *http.Request, action, id, successMessage string, err error) {
	if err != nil {
		message := manager.UserMessage(err)
		status := statusFor(err)
		// Refusals and races are expected traffic; only genuine faults are
		// logged at error level.
		if status >= http.StatusInternalServerError {
			s.log.Error("action failed", "action", action, "container", id, "error", err)
		} else {
			s.log.Info("action refused", "action", action, "container", id, "reason", message)
		}
		s.respond(w, r, status, "error", message)
		return
	}
	s.respond(w, r, http.StatusOK, "ok", successMessage)
}

// respond answers an action request.
//
// A fetch from the page sends Accept: application/json and gets JSON back, so the
// list can be refreshed in place. A plain form submission — the path taken when
// JavaScript is unavailable — gets a redirect to the overview with the message in
// the query string.
func (s *Server) respond(w http.ResponseWriter, r *http.Request, status int, level, message string) {
	if wantsJSON(r) {
		writeJSON(w, status, actionResponse{OK: level == "ok", Message: message})
		return
	}

	target := url.URL{Path: "/"}
	if message != "" {
		target.RawQuery = url.Values{"level": {level}, "msg": {message}}.Encode()
	}
	http.Redirect(w, r, target.String(), http.StatusSeeOther)
}

type actionResponse struct {
	OK      bool   `json:"ok"`
	Message string `json:"message,omitempty"`
}

func wantsJSON(r *http.Request) bool {
	return strings.Contains(r.Header.Get("Accept"), "application/json")
}

func writeJSON(w http.ResponseWriter, status int, payload any) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.Header().Set("Cache-Control", "no-store")
	w.WriteHeader(status)
	if err := json.NewEncoder(w).Encode(payload); err != nil {
		// Nothing useful left to do; the status line is already sent.
		return
	}
}

// statusFor maps the manager's sentinel errors onto HTTP status codes.
func statusFor(err error) int {
	switch {
	case errors.Is(err, manager.ErrNotFound):
		return http.StatusNotFound
	case errors.Is(err, manager.ErrBusy):
		return http.StatusConflict
	case errors.Is(err, manager.ErrProtected):
		return http.StatusForbidden
	case errors.Is(err, manager.ErrUnsupported):
		return http.StatusUnprocessableEntity
	default:
		return http.StatusInternalServerError
	}
}

// flashFromQuery reads a message left by a redirect. The level is restricted to
// known values so a crafted link cannot style itself as a success.
func flashFromQuery(query url.Values) *flashMessage {
	text := strings.TrimSpace(query.Get("msg"))
	if text == "" {
		return nil
	}
	level := "error"
	if query.Get("level") == "ok" {
		level = "ok"
	}
	// Keep a hostile link from filling the page with text. The templates escape
	// the content, so this is about layout rather than safety.
	if len(text) > 500 {
		text = text[:500] + "…"
	}
	return &flashMessage{Level: level, Text: text}
}

func joinMessage(headline string, notes []string) string {
	if len(notes) == 0 {
		return headline
	}
	return headline + " " + strings.Join(notes, "; ") + "."
}
