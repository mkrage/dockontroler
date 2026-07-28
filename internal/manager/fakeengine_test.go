package manager

import (
	"encoding/json"
	"fmt"
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
)

// fakeEngine is a stand-in for the Docker Engine API.
//
// It is a small simulator rather than a set of canned replies: it keeps real
// container state, so tests can assert on what the host looks like afterwards
// ("is the original back under its own name?") instead of only on which calls
// were made. That is the whole point for the recreate tests, where the question
// is always about the end state.
type fakeEngine struct {
	mu         sync.Mutex
	containers map[string]*fakeContainer // keyed by full id
	calls      []string
	connects   []string
	nextID     int

	// failOn makes a specific request fail, which is how the rollback paths get
	// exercised. Called with the version-stripped path, e.g.
	// "/containers/create".
	failOn func(method, path string) (status int, message string, fail bool)
}

type fakeContainer struct {
	ID         string
	Name       string // with Docker's leading slash
	Running    bool
	Status     string
	Config     map[string]any
	HostConfig map[string]any
	Mounts     []docker.MountPoint
	Networks   map[string]docker.EndpointSettings
	Labels     map[string]string
	Image      string
}

func newFakeEngine() *fakeEngine {
	return &fakeEngine{containers: map[string]*fakeContainer{}}
}

// add registers a container and returns it for further tweaking.
func (f *fakeEngine) add(c *fakeContainer) *fakeContainer {
	if c.Config == nil {
		c.Config = map[string]any{}
	}
	if c.HostConfig == nil {
		c.HostConfig = map[string]any{}
	}
	if c.Status == "" {
		if c.Running {
			c.Status = "Up 2 hours"
		} else {
			c.Status = "Exited (0) 5 minutes ago"
		}
	}
	if !strings.HasPrefix(c.Name, "/") {
		c.Name = "/" + c.Name
	}
	f.containers[c.ID] = c
	return c
}

// start wires the engine into a docker.Client. The server is closed by t.Cleanup.
func (f *fakeEngine) start(t *testing.T) *docker.Client {
	t.Helper()
	server := httptest.NewServer(f)
	t.Cleanup(server.Close)
	return docker.NewWithBaseURL(server.URL)
}

// lookup resolves a full id, a 12-character short id, or a name.
func (f *fakeEngine) lookup(ref string) *fakeContainer {
	if c, ok := f.containers[ref]; ok {
		return c
	}
	for _, c := range f.containers {
		if strings.HasPrefix(c.ID, ref) && len(ref) >= 12 {
			return c
		}
		if c.Name == "/"+strings.TrimPrefix(ref, "/") {
			return c
		}
	}
	return nil
}

// byName returns the container currently holding a name, or nil.
func (f *fakeEngine) byName(name string) *fakeContainer {
	f.mu.Lock()
	defer f.mu.Unlock()
	for _, container := range f.containers {
		if container.Name == "/"+strings.TrimPrefix(name, "/") {
			return container
		}
	}
	return nil
}

// count returns how many containers exist.
func (f *fakeEngine) count() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return len(f.containers)
}

// connected returns the networks that were attached via /networks/{name}/connect.
func (f *fakeEngine) connected() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]string(nil), f.connects...)
}

// sequence returns the requests in order as "METHOD /path", query stripped, for
// asserting that a rollback ran in the right order.
func (f *fakeEngine) sequence() []string {
	f.mu.Lock()
	defer f.mu.Unlock()

	out := make([]string, 0, len(f.calls))
	for _, call := range f.calls {
		if cut := strings.IndexByte(call, '?'); cut >= 0 {
			call = call[:cut]
		}
		out = append(out, call)
	}
	return out
}

// called reports whether method+path was requested, ignoring the query string.
func (f *fakeEngine) called(method, path string) bool {
	for _, call := range f.sequence() {
		if call == method+" "+path {
			return true
		}
	}
	return false
}

// queryOf returns the query of the last request matching method+path.
func (f *fakeEngine) queryOf(t *testing.T, method, path string) url.Values {
	t.Helper()
	f.mu.Lock()
	defer f.mu.Unlock()

	prefix := method + " " + path
	for i := len(f.calls) - 1; i >= 0; i-- {
		call := f.calls[i]
		raw, query, _ := strings.Cut(call, "?")
		if raw == prefix {
			parsed, err := url.ParseQuery(query)
			if err != nil {
				t.Fatalf("parse query of %q: %v", call, err)
			}
			return parsed
		}
	}
	t.Fatalf("no request matching %q, calls were:\n%s", prefix, strings.Join(f.calls, "\n"))
	return nil
}

func (f *fakeEngine) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	path := strings.TrimPrefix(r.URL.Path, "/v"+docker.APIVersion)

	f.mu.Lock()
	defer f.mu.Unlock()

	entry := r.Method + " " + path
	if r.URL.RawQuery != "" {
		entry += "?" + r.URL.RawQuery
	}
	f.calls = append(f.calls, entry)

	if f.failOn != nil {
		if status, message, fail := f.failOn(r.Method, path); fail {
			writeEngineError(w, status, message)
			return
		}
	}

	switch {
	case path == "/containers/json" && r.Method == http.MethodGet:
		f.handleList(w)
	case path == "/containers/create" && r.Method == http.MethodPost:
		f.handleCreate(w, r)
	case strings.HasPrefix(path, "/networks/") && strings.HasSuffix(path, "/connect"):
		name := strings.TrimSuffix(strings.TrimPrefix(path, "/networks/"), "/connect")
		f.connects = append(f.connects, name)
		w.WriteHeader(http.StatusOK)
	case strings.HasPrefix(path, "/containers/"):
		f.handleContainer(w, r, strings.TrimPrefix(path, "/containers/"))
	default:
		writeEngineError(w, http.StatusNotFound, "no such endpoint: "+path)
	}
}

func (f *fakeEngine) handleContainer(w http.ResponseWriter, r *http.Request, rest string) {
	ref, action, _ := strings.Cut(rest, "/")

	container := f.lookup(ref)
	if container == nil {
		writeEngineError(w, http.StatusNotFound, "No such container: "+ref)
		return
	}

	switch {
	case action == "json" && r.Method == http.MethodGet:
		writeJSON(w, http.StatusOK, container.inspect())

	case action == "start":
		if container.Running {
			w.WriteHeader(http.StatusNotModified)
			return
		}
		container.Running = true
		container.Status = "Up 1 second"
		w.WriteHeader(http.StatusNoContent)

	case action == "stop":
		if !container.Running {
			w.WriteHeader(http.StatusNotModified)
			return
		}
		container.Running = false
		container.Status = "Exited (0) 1 second ago"
		w.WriteHeader(http.StatusNoContent)

	case action == "restart":
		container.Running = true
		w.WriteHeader(http.StatusNoContent)

	case action == "rename":
		newName := r.URL.Query().Get("name")
		if newName == "" {
			writeEngineError(w, http.StatusBadRequest, "missing name")
			return
		}
		if other := f.lookup(newName); other != nil && other != container {
			writeEngineError(w, http.StatusConflict,
				fmt.Sprintf("Conflict. The container name %q is already in use", "/"+newName))
			return
		}
		container.Name = "/" + newName
		w.WriteHeader(http.StatusNoContent)

	case action == "update":
		var body struct {
			RestartPolicy docker.RestartPolicy `json:"RestartPolicy"`
		}
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			writeEngineError(w, http.StatusBadRequest, "malformed body")
			return
		}
		container.HostConfig["RestartPolicy"] = map[string]any{
			"Name":              body.RestartPolicy.Name,
			"MaximumRetryCount": float64(body.RestartPolicy.MaximumRetryCount),
		}
		writeJSON(w, http.StatusOK, docker.UpdateResponse{})

	case action == "" && r.Method == http.MethodDelete:
		delete(f.containers, container.ID)
		w.WriteHeader(http.StatusNoContent)

	default:
		writeEngineError(w, http.StatusNotFound, "unsupported action: "+action)
	}
}

func (f *fakeEngine) handleList(w http.ResponseWriter) {
	summaries := make([]docker.ContainerSummary, 0, len(f.containers))
	for _, container := range f.containers {
		state := docker.StateExited
		if container.Running {
			state = docker.StateRunning
		}
		summaries = append(summaries, docker.ContainerSummary{
			ID:      container.ID,
			Names:   []string{container.Name},
			Image:   container.Image,
			State:   state,
			Status:  container.Status,
			Created: time.Now().Add(-time.Hour).Unix(),
			Labels:  container.Labels,
		})
	}
	writeJSON(w, http.StatusOK, summaries)
}

func (f *fakeEngine) handleCreate(w http.ResponseWriter, r *http.Request) {
	var body map[string]any
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		writeEngineError(w, http.StatusBadRequest, "malformed body")
		return
	}
	name := r.URL.Query().Get("name")
	if other := f.lookup(name); other != nil {
		writeEngineError(w, http.StatusConflict,
			fmt.Sprintf("Conflict. The container name %q is already in use", "/"+name))
		return
	}

	f.nextID++
	// Distinctive but still a valid 64-character hex id.
	id := fmt.Sprintf("%064x", 0xbeef0000+f.nextID)

	hostConfig, _ := body["HostConfig"].(map[string]any)
	if hostConfig == nil {
		hostConfig = map[string]any{}
	}
	config := map[string]any{}
	for key, value := range body {
		if key != "HostConfig" && key != "NetworkingConfig" {
			config[key] = value
		}
	}

	f.add(&fakeContainer{
		ID:         id,
		Name:       name,
		Config:     config,
		HostConfig: hostConfig,
		Image:      fmt.Sprint(config["Image"]),
	})
	writeJSON(w, http.StatusCreated, docker.CreateResponse{ID: id})
}

// inspect renders the container the way the Engine would.
func (c *fakeContainer) inspect() map[string]any {
	config := map[string]any{}
	for key, value := range c.Config {
		config[key] = value
	}
	if _, ok := config["Image"]; !ok {
		config["Image"] = c.Image
	}
	if len(c.Labels) > 0 {
		labels := map[string]any{}
		for key, value := range c.Labels {
			labels[key] = value
		}
		config["Labels"] = labels
	}

	status := docker.StateExited
	if c.Running {
		status = docker.StateRunning
	}
	return map[string]any{
		"Id":              c.ID,
		"Name":            c.Name,
		"Image":           "sha256:" + strings.Repeat("a", 64),
		"State":           map[string]any{"Running": c.Running, "Status": status},
		"Mounts":          c.Mounts,
		"NetworkSettings": map[string]any{"Networks": c.Networks},
		"Config":          config,
		"HostConfig":      c.HostConfig,
	}
}

func writeJSON(w http.ResponseWriter, status int, payload any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(payload)
}

func writeEngineError(w http.ResponseWriter, status int, message string) {
	writeJSON(w, status, map[string]string{"message": message})
}

// testLogger discards output; tests assert on behaviour, not on log lines.
func testLogger() *slog.Logger {
	return slog.New(slog.NewTextHandler(io.Discard, nil))
}

// newTestManager wires a Manager to the fake engine.
func newTestManager(t *testing.T, engine *fakeEngine, selfID string) *Manager {
	t.Helper()
	return New(engine.start(t), testLogger(), 5*time.Second, selfID)
}

// hexID builds a plausible 64-character container id from a short prefix.
func hexID(prefix string) string {
	if len(prefix) >= 64 {
		return prefix[:64]
	}
	return prefix + strings.Repeat("0", 64-len(prefix))
}
