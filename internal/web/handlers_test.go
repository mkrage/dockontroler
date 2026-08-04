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
	var ports []docker.Port
	if s.running {
		state, status = docker.StateRunning, "Up 3 hours"
		// As the Engine reports it: one published port per address family, plus a
		// port the image only exposes. Ports are absent entirely while stopped,
		// which is what makes the fallback to HostConfig.PortBindings matter.
		ports = []docker.Port{
			{IP: "0.0.0.0", PrivatePort: 80, PublicPort: 8080, Type: "tcp"},
			{IP: "::", PrivatePort: 80, PublicPort: 8080, Type: "tcp"},
			{PrivatePort: 9000, Type: "tcp"},
		}
	}
	return docker.ContainerSummary{
		ID:      testContainerID,
		Names:   []string{"/blog-web-1"},
		Image:   "ghcr.io/me/blog:latest",
		State:   state,
		Status:  status,
		Created: time.Now().Add(-time.Hour).Unix(),
		Ports:   ports,
		Labels: map[string]string{
			docker.LabelComposeProject:     "blog",
			docker.LabelComposeService:     "web",
			docker.LabelComposeConfigFiles: "/data/compose/7/docker-compose.yml",
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
			// The inspect labels win over the ones in the list entry, so the compose
			// file has to be here as well -- which is where Docker keeps it too.
			"Labels": map[string]any{
				docker.LabelComposeProject:     "blog",
				docker.LabelComposeService:     "web",
				docker.LabelComposeConfigFiles: "/data/compose/7/docker-compose.yml",
			},
		},
		"HostConfig": map[string]any{
			"NetworkMode":   "blog_default",
			"RestartPolicy": map[string]any{"Name": policy},
			"PortBindings": map[string]any{
				"80/tcp": []any{map[string]any{"HostIp": "", "HostPort": "8080"}},
			},
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
		"blog",                   // the Compose project heading
		"web",                    // the service name
		"ghcr.io/me/blog:latest", // the image the recreate would use
		"Up 3 hours",
		`value="unless-stopped"`, // the policy control
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

	// A policy set here is overwritten the next time the container is rebuilt from
	// its yaml, so the row has to say which file to write it into.
	if !strings.Contains(body, "/data/compose/7/docker-compose.yml") {
		t.Error("the row does not name the compose file the policy has to be written into")
	}
}

// TestIndexShowsPortsAndLinksThem covers the point of showing ports at all: being
// able to go from "which port was that again" to the service in one click. The
// link has to be built from the address the browser used, because a wildcard
// binding is not something anyone can open.
func TestIndexShowsPortsAndLinksThem(t *testing.T) {
	handler, engine := newTestServer(t)

	request := httptest.NewRequest(http.MethodGet, "/", nil)
	request.Host = "192.168.1.10:3625"
	recorder := httptest.NewRecorder()
	handler.ServeHTTP(recorder, request)
	body := recorder.Body.String()

	if !strings.Contains(body, `href="http://192.168.1.10:8080"`) {
		t.Errorf("no link to the published port built from the browser's own address:\n%s", body)
	}
	if !strings.Contains(body, "8080 → 80") {
		t.Error("the port mapping is not shown")
	}
	// Reported twice by the Engine, once per address family, but it is one port.
	if count := strings.Count(body, "8080 → 80"); count != 1 {
		t.Errorf("the port is shown %d times, want once", count)
	}
	// Exposed by the image but not published: unreachable, so naming it would
	// only offer a link that cannot work.
	if strings.Contains(body, ">9000<") {
		t.Error("an exposed-but-unpublished port is shown as if it were reachable")
	}

	// Stopped: the mapping is still worth showing, a link is not — nothing is
	// listening on it.
	engine.mu.Lock()
	engine.running = false
	engine.mu.Unlock()

	recorder = httptest.NewRecorder()
	handler.ServeHTTP(recorder, request)
	body = recorder.Body.String()

	if !strings.Contains(body, "8080 → 80") {
		t.Error("a stopped container does not show its configured port")
	}
	if strings.Contains(body, `href="http://192.168.1.10:8080"`) {
		t.Error("a stopped container's port is offered as a link")
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

// TestActionsRefuseCrossOriginRequests covers the only thing standing between an
// unauthenticated control panel and any web page a user on the network happens to
// open: without this, a form on another site could post here, and it would not even
// need a container id, since names resolve too.
//
// The flip side matters just as much — this must not turn into a login. A script
// with no browser headers, and the page's own forms, have to keep working.
func TestActionsRefuseCrossOriginRequests(t *testing.T) {
	handler, engine := newTestServer(t)

	cases := []struct {
		name       string
		headers    map[string]string
		wantStatus int
	}{
		{
			name:       "a form on another site",
			headers:    map[string]string{"Sec-Fetch-Site": "cross-site"},
			wantStatus: http.StatusForbidden,
		},
		{
			name:       "a sibling host on the same site",
			headers:    map[string]string{"Sec-Fetch-Site": "same-site"},
			wantStatus: http.StatusForbidden,
		},
		{
			name: "an old browser posting from elsewhere",
			// No fetch metadata, so the Origin is all there is to go on.
			headers:    map[string]string{"Origin": "http://evil.example"},
			wantStatus: http.StatusForbidden,
		},
		{
			name:       "the page itself",
			headers:    map[string]string{"Sec-Fetch-Site": "same-origin"},
			wantStatus: http.StatusOK,
		},
		{
			name:       "an old browser posting from the page",
			headers:    map[string]string{"Origin": "http://example.com"},
			wantStatus: http.StatusOK,
		},
		{
			// curl, a cron job, the /api consumer: no browser, no headers.
			name:       "a script",
			headers:    nil,
			wantStatus: http.StatusOK,
		},
	}

	for _, testCase := range cases {
		t.Run(testCase.name, func(t *testing.T) {
			engine.mu.Lock()
			engine.running = true
			engine.mu.Unlock()

			request := httptest.NewRequest(http.MethodPost, "/containers/"+testContainerID+"/stop", nil)
			request.Host = "example.com"
			request.Header.Set("Accept", "application/json")
			for name, value := range testCase.headers {
				request.Header.Set(name, value)
			}
			recorder := httptest.NewRecorder()
			handler.ServeHTTP(recorder, request)

			if recorder.Code != testCase.wantStatus {
				t.Fatalf("status = %d, want %d. body: %s",
					recorder.Code, testCase.wantStatus, recorder.Body)
			}

			engine.mu.Lock()
			defer engine.mu.Unlock()
			if stopped := !engine.running; stopped != (testCase.wantStatus == http.StatusOK) {
				t.Errorf("container stopped = %v, but the request was answered with %d",
					stopped, recorder.Code)
			}
		})
	}
}

// TestReadOnlyRoutesStayOpenCrossOrigin: the guard belongs on the actions only. A
// cross-origin GET cannot change anything, and the browser will not hand the body
// to the calling page anyway — no CORS headers are sent.
func TestReadOnlyRoutesStayOpenCrossOrigin(t *testing.T) {
	handler, _ := newTestServer(t)

	for _, path := range []string{"/", "/partials/containers", "/api/containers", "/healthz"} {
		request := httptest.NewRequest(http.MethodGet, path, nil)
		request.Header.Set("Sec-Fetch-Site", "cross-site")
		recorder := httptest.NewRecorder()
		handler.ServeHTTP(recorder, request)

		if recorder.Code != http.StatusOK {
			t.Errorf("GET %s = %d, want 200", path, recorder.Code)
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
