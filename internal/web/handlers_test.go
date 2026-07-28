package web

import (
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/mkrage/dockontroler/internal/docker"
	"github.com/mkrage/dockontroler/internal/manager"
)

const testContainerID = "aaaaaaaaaaaabbbbbbbbbbbbccccccccccccdddddddddddd0000000011111111"

// stubEngine answers the handful of Engine endpoints the web layer reaches.
//
// The manager's own tests cover Docker semantics properly; these tests are about
// routing, response shapes and template rendering, so a purpose-built stub keeps
// them readable and independent of that package's test helpers.
type stubEngine struct {
	mu      sync.Mutex
	running bool
	policy  string
}

func (s *stubEngine) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	s.mu.Lock()
	defer s.mu.Unlock()

	path := strings.TrimPrefix(r.URL.Path, "/v"+docker.APIVersion)

	switch {
	case path == "/containers/json":
		writeJSON(w, http.StatusOK, []docker.ContainerSummary{s.summary()})

	case path == "/containers/"+testContainerID+"/json":
		writeJSON(w, http.StatusOK, s.inspect())

	case path == "/containers/"+testContainerID+"/stop":
		s.running = false
		w.WriteHeader(http.StatusNoContent)

	case path == "/containers/"+testContainerID+"/start":
		s.running = true
		w.WriteHeader(http.StatusNoContent)

	case path == "/containers/"+testContainerID+"/update":
		var body struct {
			RestartPolicy docker.RestartPolicy `json:"RestartPolicy"`
		}
		_ = json.NewDecoder(r.Body).Decode(&body)
		s.policy = body.RestartPolicy.Name
		writeJSON(w, http.StatusOK, docker.UpdateResponse{})

	default:
		writeJSON(w, http.StatusNotFound, map[string]string{"message": "No such container"})
	}
}

func (s *stubEngine) summary() docker.ContainerSummary {
	state, status := docker.StateExited, "Exited (0) 1 minute ago"
	if s.running {
		state, status = docker.StateRunning, "Up 3 hours"
	}
	return docker.ContainerSummary{
		ID:      testContainerID,
		Names:   []string{"/blog-web-1"},
		Image:   "ghcr.io/me/blog:latest",
		State:   state,
		Status:  status,
		Created: time.Now().Add(-time.Hour).Unix(),
		Labels: map[string]string{
			docker.LabelComposeProject: "blog",
			docker.LabelComposeService: "web",
		},
	}
}

func (s *stubEngine) inspect() map[string]any {
	policy := s.policy
	if policy == "" {
		policy = docker.PolicyUnlessStopped
	}
	return map[string]any{
		"Id":    testContainerID,
		"Name":  "/blog-web-1",
		"Image": "sha256:" + strings.Repeat("f", 64),
		"State": map[string]any{"Running": s.running},
		"Config": map[string]any{
			"Image": "ghcr.io/me/blog:latest",
			"Labels": map[string]any{
				docker.LabelComposeProject: "blog",
				docker.LabelComposeService: "web",
			},
		},
		"HostConfig": map[string]any{
			"NetworkMode":   "blog_default",
			"RestartPolicy": map[string]any{"Name": policy},
		},
		"NetworkSettings": map[string]any{"Networks": map[string]any{}},
	}
}

func newTestServer(t *testing.T) (http.Handler, *stubEngine) {
	t.Helper()

	engine := &stubEngine{running: true}
	daemon := httptest.NewServer(engine)
	t.Cleanup(daemon.Close)

	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	containers := manager.New(docker.NewWithBaseURL(daemon.URL), logger, 5*time.Second, "")

	server, err := New(containers, logger, 5*time.Second)
	if err != nil {
		t.Fatalf("web.New: %v", err)
	}
	return server.Handler(), engine
}

func TestIndexRendersTheContainerList(t *testing.T) {
	handler, _ := newTestServer(t)

	recorder := httptest.NewRecorder()
	handler.ServeHTTP(recorder, httptest.NewRequest(http.MethodGet, "/", nil))

	if recorder.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200. body:\n%s", recorder.Code, recorder.Body)
	}
	body := recorder.Body.String()

	for _, want := range []string{
		"<!DOCTYPE html>",
		"blog",                    // the Compose project heading
		"web",                     // the service name
		"ghcr.io/me/blog:latest",  // the image the recreate would use
		"Up 3 hours",
		`value="unless-stopped"`,  // the policy control
		"Recreate",
	} {
		if !strings.Contains(body, want) {
			t.Errorf("page does not contain %q", want)
		}
	}

	// The active policy must be visibly marked, or the control tells the user
	// nothing about the current state.
	if !strings.Contains(body, "is-active") {
		t.Error("no policy button is marked active")
	}
}

func TestFragmentIsJustTheList(t *testing.T) {
	handler, _ := newTestServer(t)

	recorder := httptest.NewRecorder()
	handler.ServeHTTP(recorder, httptest.NewRequest(http.MethodGet, "/partials/containers", nil))

	if recorder.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", recorder.Code)
	}
	body := recorder.Body.String()
	if strings.Contains(body, "<!DOCTYPE html>") {
		t.Error("the fragment returned a whole page; the auto-refresh would nest documents")
	}
	if !strings.Contains(body, "blog-web-1") {
		t.Errorf("the fragment does not list the container:\n%s", body)
	}
	// app.js pulls the timestamp out of this element, so its class is part of the
	// contract between template and script.
	if !strings.Contains(body, "overview__stamp") {
		t.Error("the fragment is missing the timestamp element app.js looks for")
	}
}

func TestAPIContainersReturnsJSON(t *testing.T) {
	handler, _ := newTestServer(t)

	recorder := httptest.NewRecorder()
	handler.ServeHTTP(recorder, httptest.NewRequest(http.MethodGet, "/api/containers", nil))

	if recorder.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", recorder.Code)
	}
	var overview manager.Overview
	if err := json.Unmarshal(recorder.Body.Bytes(), &overview); err != nil {
		t.Fatalf("decode response: %v\nbody: %s", err, recorder.Body)
	}
	if overview.Total != 1 || overview.Running != 1 {
		t.Errorf("Total=%d Running=%d, want 1 and 1", overview.Total, overview.Running)
	}
}

func TestActionAsFetchReturnsJSON(t *testing.T) {
	handler, engine := newTestServer(t)

	request := httptest.NewRequest(http.MethodPost, "/containers/"+testContainerID+"/stop", nil)
	request.Header.Set("Accept", "application/json")
	recorder := httptest.NewRecorder()
	handler.ServeHTTP(recorder, request)

	if recorder.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200. body: %s", recorder.Code, recorder.Body)
	}
	var payload struct {
		OK      bool   `json:"ok"`
		Message string `json:"message"`
	}
	if err := json.Unmarshal(recorder.Body.Bytes(), &payload); err != nil {
		t.Fatalf("decode response: %v", err)
	}
	if !payload.OK || payload.Message == "" {
		t.Errorf("payload = %+v, want ok with a message", payload)
	}

	engine.mu.Lock()
	defer engine.mu.Unlock()
	if engine.running {
		t.Error("the container was not stopped")
	}
}

// TestActionWithoutJavaScriptRedirects covers the progressive-enhancement path: a
// plain form submission must still work, with the outcome carried in the URL.
func TestActionWithoutJavaScriptRedirects(t *testing.T) {
	handler, _ := newTestServer(t)

	request := httptest.NewRequest(http.MethodPost, "/containers/"+testContainerID+"/stop", nil)
	recorder := httptest.NewRecorder()
	handler.ServeHTTP(recorder, request)

	if recorder.Code != http.StatusSeeOther {
		t.Fatalf("status = %d, want 303", recorder.Code)
	}
	location, err := url.Parse(recorder.Header().Get("Location"))
	if err != nil {
		t.Fatalf("parse Location: %v", err)
	}
	if location.Path != "/" {
		t.Errorf("redirect path = %q, want /", location.Path)
	}
	if location.Query().Get("level") != "ok" || location.Query().Get("msg") == "" {
		t.Errorf("redirect query = %q, want a level and a message", location.RawQuery)
	}
}

func TestPolicyAction(t *testing.T) {
	handler, engine := newTestServer(t)

	body := strings.NewReader("policy=always")
	request := httptest.NewRequest(http.MethodPost, "/containers/"+testContainerID+"/policy", body)
	request.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	request.Header.Set("Accept", "application/json")
	recorder := httptest.NewRecorder()
	handler.ServeHTTP(recorder, request)

	if recorder.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200. body: %s", recorder.Code, recorder.Body)
	}
	engine.mu.Lock()
	defer engine.mu.Unlock()
	if engine.policy != docker.PolicyAlways {
		t.Errorf("daemon policy = %q, want always", engine.policy)
	}
}

func TestPolicyRejectsUnsupportedValue(t *testing.T) {
	handler, _ := newTestServer(t)

	body := strings.NewReader("policy=on-failure")
	request := httptest.NewRequest(http.MethodPost, "/containers/"+testContainerID+"/policy", body)
	request.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	request.Header.Set("Accept", "application/json")
	recorder := httptest.NewRecorder()
	handler.ServeHTTP(recorder, request)

	if recorder.Code != http.StatusUnprocessableEntity {
		t.Errorf("status = %d, want 422", recorder.Code)
	}
}

func TestUnknownContainerIs404(t *testing.T) {
	handler, _ := newTestServer(t)

	request := httptest.NewRequest(http.MethodPost, "/containers/nosuchcontainer/start", nil)
	request.Header.Set("Accept", "application/json")
	recorder := httptest.NewRecorder()
	handler.ServeHTTP(recorder, request)

	if recorder.Code != http.StatusNotFound {
		t.Errorf("status = %d, want 404", recorder.Code)
	}
}

func TestRoutingBoundaries(t *testing.T) {
	handler, _ := newTestServer(t)

	cases := []struct {
		method, path string
		wantStatus   int
	}{
		// "/" is registered as "/{$}" so it cannot swallow unknown paths.
		{http.MethodGet, "/nope", http.StatusNotFound},
		{http.MethodGet, "/healthz", http.StatusOK},
		{http.MethodGet, "/static/app.css", http.StatusOK},
		{http.MethodGet, "/static/app.js", http.StatusOK},
		{http.MethodGet, "/static/favicon.svg", http.StatusOK},
		// Actions must not be reachable by GET, which browsers prefetch.
		{http.MethodGet, "/containers/" + testContainerID + "/stop", http.StatusMethodNotAllowed},
	}
	for _, testCase := range cases {
		recorder := httptest.NewRecorder()
		handler.ServeHTTP(recorder, httptest.NewRequest(testCase.method, testCase.path, nil))
		if recorder.Code != testCase.wantStatus {
			t.Errorf("%s %s = %d, want %d", testCase.method, testCase.path,
				recorder.Code, testCase.wantStatus)
		}
	}
}

func TestStatusForSentinels(t *testing.T) {
	cases := map[error]int{
		manager.ErrNotFound:    http.StatusNotFound,
		manager.ErrBusy:        http.StatusConflict,
		manager.ErrProtected:   http.StatusForbidden,
		manager.ErrUnsupported: http.StatusUnprocessableEntity,
	}
	for err, want := range cases {
		if got := statusFor(err); got != want {
			t.Errorf("statusFor(%v) = %d, want %d", err, got, want)
		}
	}
}

// TestFlashLevelCannotBeForged: the level decides the styling, so a crafted link
// must not be able to present a failure as a success.
func TestFlashLevelCannotBeForged(t *testing.T) {
	flash := flashFromQuery(url.Values{"msg": {"anything"}, "level": {"ok'; --"}})
	if flash == nil || flash.Level != "error" {
		t.Errorf("flash = %+v, want an unknown level to fall back to error", flash)
	}

	if flashFromQuery(url.Values{}) != nil {
		t.Error("an empty query should produce no flash")
	}

	long := flashFromQuery(url.Values{"msg": {strings.Repeat("x", 5000)}})
	if len([]rune(long.Text)) > 501 {
		t.Errorf("flash text is %d characters, want it clamped", len([]rune(long.Text)))
	}
}
