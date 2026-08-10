package web

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
	"github.com/mkrage/dockontroler/internal/manager"
)

const (
	testContainerID = "aaaaaaaaaaaabbbbbbbbbbbbccccccccccccdddddddddddd0000000011111111"
	testDBID        = "1111111122222222333333334444444455555555666666667777777788888888"
)

// stubEngine answers the handful of Engine endpoints the web layer reaches.
//
// The manager's own tests cover Docker semantics properly; these tests are about
// routing, response shapes and template rendering, so a purpose-built stub keeps
// them readable and independent of that package's test helpers.
//
// It holds two containers of one Compose project, because that is the smallest host
// on which the things this package renders exist at all: a stack to control as a
// whole, and a colour two cards have to share.
type stubEngine struct {
	mu         sync.Mutex
	containers []*stubContainer
}

type stubContainer struct {
	id      string
	service string
	// project is the Compose project, empty for a container Compose never touched —
	// which is a shape the panel view has a row for and the card view has not.
	project string
	image   string
	running bool
	policy  string
	// restarts counts how often the Engine was asked to restart this one, which is the
	// only way to tell a restart from a container that was already up.
	restarts int
	// publishes marks the one container that maps a host port, which is what the
	// port links are rendered from.
	publishes bool
	dependsOn string
}

func newStubEngine() *stubEngine {
	return &stubEngine{containers: []*stubContainer{
		{
			id:        testContainerID,
			service:   "web",
			project:   "blog",
			image:     "ghcr.io/me/blog:latest",
			running:   true,
			publishes: true,
			dependsOn: "db:service_started:true",
		},
		{id: testDBID, service: "db", project: "blog", image: "postgres:16", running: true},
	}}
}

// add puts another container on the stub host, for the tests that need a shape the
// default pair cannot show.
func (s *stubEngine) add(container *stubContainer) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.containers = append(s.containers, container)
}

func (s *stubEngine) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	s.mu.Lock()
	defer s.mu.Unlock()

	path := strings.TrimPrefix(r.URL.Path, "/v"+docker.APIVersion)

	if path == "/containers/json" {
		summaries := make([]docker.ContainerSummary, 0, len(s.containers))
		for _, container := range s.containers {
			summaries = append(summaries, container.summary())
		}
		writeJSON(w, http.StatusOK, summaries)
		return
	}

	notFound := func() {
		writeJSON(w, http.StatusNotFound, map[string]string{"message": "No such container"})
	}
	if !strings.HasPrefix(path, "/containers/") {
		notFound()
		return
	}

	ref, action, _ := strings.Cut(strings.TrimPrefix(path, "/containers/"), "/")
	container := s.findLocked(ref)
	if container == nil {
		notFound()
		return
	}

	switch action {
	case "json":
		writeJSON(w, http.StatusOK, container.inspect())

	case "start":
		container.running = true
		w.WriteHeader(http.StatusNoContent)

	case "stop":
		container.running = false
		w.WriteHeader(http.StatusNoContent)

	case "restart":
		container.restarts++
		container.running = true
		w.WriteHeader(http.StatusNoContent)

	case "update":
		var body struct {
			RestartPolicy docker.RestartPolicy `json:"RestartPolicy"`
		}
		_ = json.NewDecoder(r.Body).Decode(&body)
		container.policy = body.RestartPolicy.Name
		writeJSON(w, http.StatusOK, docker.UpdateResponse{})

	default:
		notFound()
	}
}

// findLocked resolves a container id. The caller must hold the mutex.
func (s *stubEngine) findLocked(id string) *stubContainer {
	for _, container := range s.containers {
		if container.id == id {
			return container
		}
	}
	return nil
}

// setRunning moves the host into the state a test is about to render.
func (s *stubEngine) setRunning(id string, running bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if container := s.findLocked(id); container != nil {
		container.running = running
	}
}

func (s *stubEngine) isRunning(id string) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	container := s.findLocked(id)
	return container != nil && container.running
}

func (s *stubEngine) restartsOf(id string) int {
	s.mu.Lock()
	defer s.mu.Unlock()
	if container := s.findLocked(id); container != nil {
		return container.restarts
	}
	return 0
}

func (s *stubEngine) policyOf(id string) string {
	s.mu.Lock()
	defer s.mu.Unlock()
	if container := s.findLocked(id); container != nil {
		return container.policy
	}
	return ""
}

// name is the container name Docker would report: what Compose builds out of the
// project and the service, or the plain name of a container it never made.
func (c *stubContainer) name() string {
	if c.project == "" {
		return c.service
	}
	return c.project + "-" + c.service + "-1"
}

func (c *stubContainer) labels() map[string]string {
	if c.project == "" {
		return nil
	}
	labels := map[string]string{
		docker.LabelComposeProject:     c.project,
		docker.LabelComposeService:     c.service,
		docker.LabelComposeConfigFiles: "/data/compose/7/docker-compose.yml",
	}
	if c.dependsOn != "" {
		labels[docker.LabelComposeDependsOn] = c.dependsOn
	}
	return labels
}

func (c *stubContainer) summary() docker.ContainerSummary {
	state, status := docker.StateExited, "Exited (0) 1 minute ago"
	var ports []docker.Port
	if c.running {
		state, status = docker.StateRunning, "Up 3 hours"
		if c.publishes {
			// As the Engine reports it: one published port per address family, plus a
			// port the image only exposes. Ports are absent entirely while stopped,
			// which is what makes the fallback to HostConfig.PortBindings matter.
			ports = []docker.Port{
				{IP: "0.0.0.0", PrivatePort: 80, PublicPort: 8080, Type: "tcp"},
				{IP: "::", PrivatePort: 80, PublicPort: 8080, Type: "tcp"},
				{PrivatePort: 9000, Type: "tcp"},
			}
		}
	}
	return docker.ContainerSummary{
		ID:      c.id,
		Names:   []string{"/" + c.name()},
		Image:   c.image,
		State:   state,
		Status:  status,
		Created: time.Now().Add(-time.Hour).Unix(),
		Ports:   ports,
		Labels:  c.labels(),
	}
}

func (c *stubContainer) inspect() map[string]any {
	policy := c.policy
	if policy == "" {
		policy = docker.PolicyUnlessStopped
	}

	// The inspect labels win over the ones in the list entry, so they have to be here
	// as well -- which is where Docker keeps them too.
	labels := map[string]any{}
	for key, value := range c.labels() {
		labels[key] = value
	}

	bindings := map[string]any{}
	if c.publishes {
		bindings["80/tcp"] = []any{map[string]any{"HostIp": "", "HostPort": "8080"}}
	}

	return map[string]any{
		"Id":    c.id,
		"Name":  "/" + c.name(),
		"Image": "sha256:" + strings.Repeat("f", 64),
		"State": map[string]any{"Running": c.running},
		"Config": map[string]any{
			"Image":  c.image,
			"Labels": labels,
		},
		"HostConfig": map[string]any{
			"NetworkMode":   "blog_default",
			"RestartPolicy": map[string]any{"Name": policy},
			"PortBindings":  bindings,
		},
		"NetworkSettings": map[string]any{"Networks": map[string]any{}},
	}
}

func newTestServer(t *testing.T) (http.Handler, *stubEngine) {
	t.Helper()

	engine := newStubEngine()
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

// get renders a page or fragment and returns its body.
func get(t *testing.T, handler http.Handler, path string) string {
	t.Helper()
	recorder := httptest.NewRecorder()
	handler.ServeHTTP(recorder, httptest.NewRequest(http.MethodGet, path, nil))
	if recorder.Code != http.StatusOK {
		t.Fatalf("GET %s = %d, want 200. body:\n%s", path, recorder.Code, recorder.Body)
	}
	return recorder.Body.String()
}

func TestIndexRendersTheContainerList(t *testing.T) {
	handler, _ := newTestServer(t)
	body := get(t, handler, "/")

	for _, want := range []string{
		"<!DOCTYPE html>",
		"blog",                   // the Compose project
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

	// The current policy must be the one the control shows, or it tells the user
	// nothing about the state it is supposed to represent.
	if !strings.Contains(body, `value="unless-stopped" selected`) {
		t.Error("the policy control does not preselect the container's own policy")
	}

	// A policy set here is overwritten the next time the container is rebuilt from
	// its yaml, so the row has to say which file to write it into.
	if !strings.Contains(body, "/data/compose/7/docker-compose.yml") {
		t.Error("the row does not name the compose file the policy has to be written into")
	}

	// The filter box lives in the topbar, and app.js matches against the haystack the
	// card carries rather than its rendered text.
	if !strings.Contains(body, `id="filter"`) {
		t.Error("the page has no filter box")
	}
	if !strings.Contains(body, `data-search="blog-web-1 blog web ghcr.io/me/blog:latest`) {
		t.Errorf("the card carries no searchable text:\n%s", body)
	}
}

// TestIndexShowsPortsAndLinksThem covers the point of showing ports at all: being
// able to go from "which port was that again" to the service in one click. The
// link has to be built from the address the browser used, because a wildcard
// binding is not something anyone can open.
func TestIndexShowsPortsAndLinksThem(t *testing.T) {
	handler, engine := newTestServer(t)

	page := func() string {
		request := httptest.NewRequest(http.MethodGet, "/", nil)
		request.Host = "192.168.1.10:3625"
		recorder := httptest.NewRecorder()
		handler.ServeHTTP(recorder, request)
		return recorder.Body.String()
	}

	body := page()
	if !strings.Contains(body, `href="http://192.168.1.10:8080"`) {
		t.Errorf("no link to the published port built from the browser's own address:\n%s", body)
	}
	if !strings.Contains(body, "8080 → 80") {
		t.Error("the port mapping is not shown")
	}
	// Reported twice by the Engine, once per address family, but it is one port.
	//
	// Counted as the chip's own text, between the tags, rather than anywhere in the
	// page: the card also carries the label inside data-search, so that is a second
	// occurrence which has nothing to do with the deduplication this is about.
	if count := strings.Count(body, ">8080 → 80<"); count != 1 {
		t.Errorf("the port is shown %d times, want once", count)
	}
	// Exposed by the image but not published: unreachable, so naming it would
	// only offer a link that cannot work.
	if strings.Contains(body, ">9000<") {
		t.Error("an exposed-but-unpublished port is shown as if it were reachable")
	}

	// Stopped: the mapping is still worth showing, a link is not — nothing is
	// listening on it.
	engine.setRunning(testContainerID, false)
	body = page()

	if !strings.Contains(body, "8080 → 80") {
		t.Error("a stopped container does not show its configured port")
	}
	if strings.Contains(body, `href="http://192.168.1.10:8080"`) {
		t.Error("a stopped container's port is offered as a link")
	}
}

// TestStoppedContainersGetTheirOwnSection: the grid is for the live host, and what is
// not running goes below it. The section must not be there when there is nothing in
// it, a running container must never end up inside it — and it starts open, because a
// stopped container is still part of what is on this host.
func TestStoppedContainersGetTheirOwnSection(t *testing.T) {
	handler, engine := newTestServer(t)

	body := get(t, handler, "/")
	if strings.Contains(body, `data-fold="stopped"`) {
		t.Error("the not-running section is rendered while everything is running")
	}

	engine.setRunning(testContainerID, false)
	body = get(t, handler, "/")

	section := strings.Index(body, `data-fold="stopped"`)
	if section < 0 {
		t.Fatalf("no section for the stopped container:\n%s", body)
	}
	// The card itself, found by the attribute only a card has, not by its name as a
	// bare string: the name is also in the tooltip of this container's cell in the
	// strip at the top of the page, which is above the section on purpose and would
	// otherwise look like the card being in the wrong place.
	if card := strings.Index(body, `data-name="blog-web-1"`); card < section {
		t.Error("the stopped container is in the grid above instead of the section below")
	}
	if !strings.Contains(body, `data-fold="stopped" open`) {
		t.Error("the section starts folded, so half the host is a click away on every load")
	}
}

// TestBothSectionsFoldTheSameWay: the section for what is up is a section like the one
// below it, not a bare grid — whoever wants to see what is not running wants the cards
// above out of the way just as much as the other way round. Both start open.
func TestBothSectionsFoldTheSameWay(t *testing.T) {
	handler, engine := newTestServer(t)
	engine.setRunning(testContainerID, false)

	body := get(t, handler, "/")
	for _, want := range []string{`data-fold="active" open`, `data-fold="stopped" open`} {
		if !strings.Contains(body, want) {
			t.Errorf("no foldable section rendered as %s:\n%s", want, body)
		}
	}
}

// TestStacksStripTiesAProjectTogether: the grid has no per-project headings, so what
// belongs together is said by a colour the cards of one project share — and the strip
// is both the legend for it and the place a whole stack is controlled from.
func TestStacksStripTiesAProjectTogether(t *testing.T) {
	handler, engine := newTestServer(t)
	body := get(t, handler, "/")

	for _, want := range []string{
		`class="stacks"`,
		`data-project="blog"`,
		">2/2<", // both containers of the stack are up
		`action="/stacks/blog/stop"`,
		// Taking a whole service down asks first, unlike a single container.
		`data-confirm="Stop the whole blog`,
	} {
		if !strings.Contains(body, want) {
			t.Errorf("the stacks strip does not contain %q:\n%s", want, body)
		}
	}

	// Both cards of the project carry the same colour class, or the colour says
	// nothing about what belongs together.
	if count := strings.Count(body, "row--stacked pc0"); count != 2 {
		t.Errorf("%d cards carry the project colour, want both of them", count)
	}

	// Everything is up, so there is nothing for "Start all" to do, and it is not
	// offered as a form that would post for nothing.
	if strings.Contains(body, `action="/stacks/blog/start"`) {
		t.Error("Start all posts although the whole stack is already running")
	}
	if !strings.Contains(body, `title="every container in this stack is already up"`) {
		t.Error("Start all is not shown as disabled while the whole stack is up")
	}

	// With part of the stack down, both directions are live and the count says which.
	engine.setRunning(testDBID, false)
	body = get(t, handler, "/")

	if !strings.Contains(body, `action="/stacks/blog/start"`) {
		t.Error("Start all is not offered although part of the stack is down")
	}
	if !strings.Contains(body, ">1/2<") {
		t.Error("the chip does not say how much of the stack is up")
	}
	// The colour has to hold across the split, or the card in the section below stops
	// looking like part of the stack in the grid above.
	if count := strings.Count(body, "row--stacked pc0"); count != 2 {
		t.Errorf("%d cards carry the project colour once the stack is split, want both", count)
	}
}

// TestSingleContainerProjectIsNoStack: two thirds of the projects on a typical host
// hold one container. A chip and a colour for each of them would be noise — the
// container's own buttons already are the stack control.
func TestSingleContainerProjectIsNoStack(t *testing.T) {
	groups := []manager.Group{
		{Project: "blog", Containers: []manager.Container{{Name: "blog-web-1"}, {Name: "blog-db-1"}}},
		{Project: "pihole", Containers: []manager.Container{{Name: "pihole"}}},
		{Project: "", Containers: []manager.Container{{Name: "loose"}, {Name: "also-loose"}}},
	}

	classes := projectClasses(groups)
	if len(classes) != 1 || classes["blog"] != "pc0" {
		t.Errorf("classes = %v, want only the multi-container project to get one", classes)
	}

	stacks := overviewData{Overview: manager.Overview{Groups: groups}}.Stacks()
	if len(stacks) != 1 || stacks[0].Project != "blog" {
		t.Errorf("stacks = %+v, want only blog", stacks)
	}
	if stacks[0].Total != 2 || stacks[0].Active != 0 {
		t.Errorf("stack = %+v, want 2 containers, none of them active", stacks[0])
	}
}

// TestProjectColoursMatchTheStylesheet: Go hands out pcN classes and app.css defines
// them, and nothing but this test connects the two. A stack whose slot has no rule
// would silently come out with no colour at all.
func TestProjectColoursMatchTheStylesheet(t *testing.T) {
	css, err := staticFS.ReadFile("static/app.css")
	if err != nil {
		t.Fatalf("read app.css: %v", err)
	}
	stylesheet := string(css)

	for slot := 0; slot < projectColours; slot++ {
		if !strings.Contains(stylesheet, fmt.Sprintf(".pc%d {", slot)) {
			t.Errorf(".pc%d is handed out but app.css does not define it", slot)
		}
	}
	if strings.Contains(stylesheet, fmt.Sprintf(".pc%d {", projectColours)) {
		t.Errorf(".pc%d is defined in app.css but never handed out", projectColours)
	}
}

// TestProjectColoursOnlyRepeatOnceExhausted: the slots wrap, and with six of them a
// host running seven stacks gave its first and its last the same colour, next to each
// other in the same strip. There is no palette that never repeats — this pins down
// where it starts to.
func TestProjectColoursOnlyRepeatOnceExhausted(t *testing.T) {
	var groups []manager.Group
	for i := 0; i <= projectColours; i++ {
		project := fmt.Sprintf("p%02d", i)
		groups = append(groups, manager.Group{
			Project:    project,
			Containers: []manager.Container{{Name: project + "-a"}, {Name: project + "-b"}},
		})
	}

	classes := projectClasses(groups)
	distinct := map[string]int{}
	for _, class := range classes {
		distinct[class]++
	}
	if len(distinct) != projectColours {
		t.Errorf("%d distinct colours for %d stacks, want all %d slots used",
			len(distinct), len(groups), projectColours)
	}

	first, wrapped := classes["p00"], classes[fmt.Sprintf("p%02d", projectColours)]
	if first != wrapped {
		t.Errorf("the %dth stack got %q, want it back at %q where the slots start over",
			projectColours+1, wrapped, first)
	}
}

// TestSearchTextCoversWhatSomebodyWouldType: the filter matches this string and
// nothing else, so what is missing from it cannot be searched for.
func TestSearchTextCoversWhatSomebodyWouldType(t *testing.T) {
	subject := card{Container: manager.Container{
		Name:           "blog-web-1",
		ComposeProject: "blog",
		ComposeService: "web",
		Image:          "ghcr.io/me/Blog:latest",
		Ports:          []manager.PortMapping{{HostPort: 8080, ContainerPort: 80, Protocol: "tcp"}},
	}}

	haystack := subject.SearchText()
	for _, term := range []string{"blog-web-1", "blog", "web", "ghcr.io/me/blog:latest", "8080"} {
		if !strings.Contains(haystack, term) {
			t.Errorf("SearchText() = %q, missing %q", haystack, term)
		}
	}
	// Lowercased once here rather than on every keystroke in the browser, so a capital
	// in an image reference must not survive.
	if strings.Contains(haystack, "Blog") {
		t.Errorf("SearchText() = %q, want it lowercased", haystack)
	}
}

// TestUnitSearchTextIsTheRowItself: a panel row's haystack must not swallow its
// containers'. The browser tells the two cases apart by exactly this — a row found by
// its own name keeps all its containers, a row found through one service shows that
// service — and folding the members' words in here would collapse both into the first.
func TestUnitSearchTextIsTheRowItself(t *testing.T) {
	subject := unit{Name: "blog", Project: "blog", Containers: []card{
		{Container: manager.Container{Name: "blog-web-1", ComposeService: "web", Image: "nginx:1.27"}},
	}}

	if haystack := subject.SearchText(); haystack != "blog" {
		t.Errorf("SearchText() = %q, want the row's own name and nothing its containers add", haystack)
	}
}

func TestFragmentIsJustTheList(t *testing.T) {
	handler, _ := newTestServer(t)
	body := get(t, handler, "/partials/containers")

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
	// The strip is refreshed with the list, or its counts would go stale as soon as
	// anything changed.
	if !strings.Contains(body, `class="stacks"`) {
		t.Error("the fragment is missing the stacks strip, so its counts would never update")
	}
}

// panelHost adds the two shapes the card view's strip has no control for, which is
// the whole reason the panel view exists: a Compose project holding exactly one
// container, and a container Compose never touched.
func panelHost(engine *stubEngine) {
	engine.add(&stubContainer{
		id:      strings.Repeat("a", 63) + "1",
		service: "n8n",
		project: "n8n",
		image:   "n8nio/n8n:1.4",
		running: true,
	})
	engine.add(&stubContainer{
		id:      strings.Repeat("b", 63) + "2",
		service: "portainer",
		image:   "portainer/portainer-ce:2",
		running: false,
	})
}

// TestPanelGivesEverythingASwitch: the point of this view is that nothing on the host
// is without one. The strip in the card view shows only projects of more than one
// container, so a project of one and a container with no project have no whole-service
// button anywhere — here they are rows like any other.
func TestPanelGivesEverythingASwitch(t *testing.T) {
	handler, engine := newTestServer(t)
	panelHost(engine)
	// One of the two containers of blog down, so that row has all three of its buttons
	// live at once — a fully running stack is offered no Start.
	engine.setRunning(testDBID, false)

	body := get(t, handler, "/?view=panel")

	for _, want := range []string{
		`data-unit="blog"`,      // the project of two
		`data-unit="n8n"`,       // the project of one
		`data-unit="portainer"`, // no project at all
	} {
		if !strings.Contains(body, want) {
			t.Errorf("the panel has no row %s:\n%s", want, body)
		}
	}

	// A project posts to the stack routes whatever its size, so a project of one is
	// stopped as a stack and reported as one.
	for _, want := range []string{
		`action="/stacks/blog/start"`,
		`action="/stacks/blog/restart"`,
		`action="/stacks/blog/stop"`,
		`action="/stacks/n8n/stop"`,
	} {
		if !strings.Contains(body, want) {
			t.Errorf("the panel is missing %s:\n%s", want, body)
		}
	}
	// A container with no project has no stack to post to.
	if !strings.Contains(body, `action="/containers/`+strings.Repeat("b", 63)+`2/start"`) {
		t.Errorf("the row for a container without a project does not drive it directly:\n%s", body)
	}

	// The strip would be the same information twice, and the one thing it can say that
	// a row cannot — the colour a card carries — has no cards to explain here.
	if strings.Contains(body, `class="stacks"`) {
		t.Error("the panel view still renders the stacks strip")
	}
	// The fraction, which is what says a stack is not all there — the bar beside it says
	// whether the missing one is switched off or dying.
	if !strings.Contains(body, `>1/2<`) {
		t.Errorf("the row does not say how much of the stack is up:\n%s", body)
	}
}

// TestPanelRowsFoldShut: the row is the answer, its containers are the detail behind
// it — and the fold has to be the mechanism the refresh already knows how to preserve.
func TestPanelRowsFoldShut(t *testing.T) {
	handler, _ := newTestServer(t)
	body := get(t, handler, "/?view=panel")

	if !strings.Contains(body, `data-fold="unit:blog"`) {
		t.Errorf("the row is not foldable:\n%s", body)
	}
	if strings.Contains(body, `data-fold="unit:blog" open`) {
		t.Error("the rows start unfolded, which is the card view with extra steps")
	}
	// The containers are in the markup, folded away rather than fetched on demand:
	// nothing here renders a row in the browser.
	if !strings.Contains(body, `class="members"`) || !strings.Contains(body, "blog-web-1") {
		t.Errorf("the containers of the row are not in the folded markup:\n%s", body)
	}
}

// TestViewChoiceSticks: the switch is two links, so which view you are in has to
// survive a page load — including the redirect a form post without JavaScript ends in.
func TestViewChoiceSticks(t *testing.T) {
	handler, _ := newTestServer(t)

	recorder := httptest.NewRecorder()
	handler.ServeHTTP(recorder, httptest.NewRequest(http.MethodGet, "/?view=panel", nil))
	if recorder.Code != http.StatusOK {
		t.Fatalf("GET /?view=panel = %d, want 200", recorder.Code)
	}

	var remembered *http.Cookie
	for _, cookie := range recorder.Result().Cookies() {
		if cookie.Name == viewCookie {
			remembered = cookie
		}
	}
	if remembered == nil || remembered.Value != viewPanel {
		t.Fatalf("cookie = %+v, want the panel view remembered", remembered)
	}

	// The plain root URL, as the redirect after an action leads to it.
	request := httptest.NewRequest(http.MethodGet, "/", nil)
	request.AddCookie(remembered)
	recorder = httptest.NewRecorder()
	handler.ServeHTTP(recorder, request)

	if !strings.Contains(recorder.Body.String(), `class="panel"`) {
		t.Errorf("the remembered view was not rendered:\n%s", recorder.Body)
	}
	// And the fragment the auto-refresh fetches has to answer in the same view, or the
	// next poll would swap the panel out for the cards.
	if body := get(t, handler, "/partials/containers?view=panel"); !strings.Contains(body, `class="panel"`) {
		t.Errorf("the fragment ignores the view:\n%s", body)
	}
}

// TestRestartStackReachesEveryRunningContainer: the third button of a panel row is the
// one operation the page did not have, and it must reach the whole stack rather than
// the container somebody happened to aim at.
func TestRestartStackReachesEveryRunningContainer(t *testing.T) {
	handler, engine := newTestServer(t)

	request := httptest.NewRequest(http.MethodPost, "/stacks/blog/restart", nil)
	request.Header.Set("Accept", "application/json")
	recorder := httptest.NewRecorder()
	handler.ServeHTTP(recorder, request)

	if recorder.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200. body: %s", recorder.Code, recorder.Body)
	}
	if engine.restartsOf(testContainerID) != 1 || engine.restartsOf(testDBID) != 1 {
		t.Errorf("restarts: web=%d db=%d, want one each",
			engine.restartsOf(testContainerID), engine.restartsOf(testDBID))
	}
	if !engine.isRunning(testContainerID) || !engine.isRunning(testDBID) {
		t.Error("the stack did not come back up")
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
	if overview.Total != 2 || overview.Running != 2 {
		t.Errorf("Total=%d Running=%d, want 2 and 2", overview.Total, overview.Running)
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
	if engine.isRunning(testContainerID) {
		t.Error("the container was not stopped")
	}
}

// TestStackActionReachesEveryContainer: the reason the strip exists is that one click
// should take the whole stack down, not the container that happened to be clicked.
func TestStackActionReachesEveryContainer(t *testing.T) {
	handler, engine := newTestServer(t)

	post := func(path string) string {
		request := httptest.NewRequest(http.MethodPost, path, nil)
		request.Header.Set("Accept", "application/json")
		recorder := httptest.NewRecorder()
		handler.ServeHTTP(recorder, request)
		if recorder.Code != http.StatusOK {
			t.Fatalf("POST %s = %d, want 200. body: %s", path, recorder.Code, recorder.Body)
		}
		var payload struct {
			OK      bool   `json:"ok"`
			Message string `json:"message"`
		}
		if err := json.Unmarshal(recorder.Body.Bytes(), &payload); err != nil {
			t.Fatalf("decode response: %v", err)
		}
		if !payload.OK {
			t.Fatalf("POST %s answered %+v, want ok", path, payload)
		}
		return payload.Message
	}

	message := post("/stacks/blog/stop")
	if engine.isRunning(testContainerID) || engine.isRunning(testDBID) {
		t.Error("part of the stack is still running")
	}
	if !strings.Contains(message, "blog") {
		t.Errorf("message = %q, want it to name the stack", message)
	}

	post("/stacks/blog/start")
	if !engine.isRunning(testContainerID) || !engine.isRunning(testDBID) {
		t.Error("part of the stack was not started")
	}

	// Starting it again changes nothing, and has to say so rather than fail.
	message = post("/stacks/blog/start")
	if !strings.Contains(message, "already") {
		t.Errorf("message = %q, want it to say the stack was already running", message)
	}
}

func TestUnknownStackIs404(t *testing.T) {
	handler, _ := newTestServer(t)

	request := httptest.NewRequest(http.MethodPost, "/stacks/nosuchstack/stop", nil)
	request.Header.Set("Accept", "application/json")
	recorder := httptest.NewRecorder()
	handler.ServeHTTP(recorder, request)

	if recorder.Code != http.StatusNotFound {
		t.Errorf("status = %d, want 404", recorder.Code)
	}
}

// TestActionWithoutJavaScriptRedirects covers the progressive-enhancement path: a
// plain form submission must still work, with the outcome carried in the URL.
func TestActionWithoutJavaScriptRedirects(t *testing.T) {
	handler, _ := newTestServer(t)

	for _, path := range []string{
		"/containers/" + testContainerID + "/stop",
		"/stacks/blog/stop",
	} {
		request := httptest.NewRequest(http.MethodPost, path, nil)
		recorder := httptest.NewRecorder()
		handler.ServeHTTP(recorder, request)

		if recorder.Code != http.StatusSeeOther {
			t.Fatalf("POST %s = %d, want 303", path, recorder.Code)
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
	if got := engine.policyOf(testContainerID); got != docker.PolicyAlways {
		t.Errorf("daemon policy = %q, want always", got)
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
		{http.MethodGet, "/stacks/blog/stop", http.StatusMethodNotAllowed},
		{http.MethodGet, "/stacks/blog/start", http.StatusMethodNotAllowed},
		{http.MethodGet, "/stacks/blog/restart", http.StatusMethodNotAllowed},
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

	// A stack action is guarded by the same wrapper, and it is the more dangerous of
	// the two: one post takes a whole service down.
	for _, path := range []string{"/containers/" + testContainerID + "/stop", "/stacks/blog/stop"} {
		for _, testCase := range cases {
			t.Run(testCase.name+" on "+path, func(t *testing.T) {
				engine.setRunning(testContainerID, true)
				engine.setRunning(testDBID, true)

				request := httptest.NewRequest(http.MethodPost, path, nil)
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

				if stopped := !engine.isRunning(testContainerID); stopped != (testCase.wantStatus == http.StatusOK) {
					t.Errorf("container stopped = %v, but the request was answered with %d",
						stopped, recorder.Code)
				}
			})
		}
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
