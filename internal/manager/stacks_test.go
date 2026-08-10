package manager

import (
	"context"
	"errors"
	"net/http"
	"strings"
	"testing"

	"github.com/mkrage/dockontroler/internal/docker"
)

// stackMemberContainer registers one container of a Compose project. dependsOnLabel
// is written verbatim, so tests can use the shape Compose itself produces.
func stackMemberContainer(engine *fakeEngine, id, project, service string, running bool, dependsOnLabel string) *fakeContainer {
	labels := map[string]string{
		docker.LabelComposeProject: project,
		docker.LabelComposeService: service,
	}
	if dependsOnLabel != "" {
		labels[docker.LabelComposeDependsOn] = dependsOnLabel
	}
	return engine.add(&fakeContainer{
		ID:         hexID(id),
		Name:       project + "-" + service + "-1",
		Running:    running,
		Image:      service + ":latest",
		Config:     map[string]any{"Image": service + ":latest"},
		HostConfig: map[string]any{"NetworkMode": project + "_default"},
		Labels:     labels,
	})
}

// touched returns the containers the engine was asked to start or stop, in order,
// by name — which is what a stack operation is really about: not only that every
// container was reached, but in which order.
func touched(engine *fakeEngine, action string) []string {
	byID := map[string]string{}
	engine.mu.Lock()
	for id, container := range engine.containers {
		byID[id] = strings.TrimPrefix(container.Name, "/")
	}
	engine.mu.Unlock()

	var names []string
	for _, call := range engine.sequence() {
		if !strings.HasSuffix(call, "/"+action) {
			continue
		}
		ref := strings.TrimSuffix(strings.TrimPrefix(call, "POST /containers/"), "/"+action)
		if name, ok := byID[ref]; ok {
			names = append(names, name)
		} else {
			names = append(names, ref)
		}
	}
	return names
}

// TestStartStackFollowsDependsOn is the reason a stack control is worth more than
// clicking the cards one by one: Compose's own depends_on label is the only trace of
// the dependency graph left on the host, and starting an app before its database is
// how a stack comes up broken.
func TestStartStackFollowsDependsOn(t *testing.T) {
	engine := newFakeEngine()
	stackMemberContainer(engine, "aaaa11110000", "blog", "web", false, "db:service_started:true,cache:service_healthy:false")
	stackMemberContainer(engine, "bbbb22220000", "blog", "db", false, "")
	stackMemberContainer(engine, "cccc33330000", "blog", "cache", false, "")
	// A different project must not be dragged along.
	stackMemberContainer(engine, "dddd44440000", "other", "app", false, "")

	containers := newTestManager(t, engine, "")

	result, err := containers.StartStack(context.Background(), "blog")
	if err != nil {
		t.Fatalf("StartStack: %v", err)
	}
	if result.Changed != 3 || result.Total != 3 {
		t.Errorf("Changed=%d Total=%d, want 3 and 3", result.Changed, result.Total)
	}

	started := touched(engine, "start")
	if len(started) != 3 {
		t.Fatalf("started %v, want all three containers of the stack", started)
	}
	if last := started[len(started)-1]; last != "blog-web-1" {
		t.Errorf("start order %v, want the service that depends on the others last", started)
	}
	if engine.byName("other-app-1").Running {
		t.Error("a container of another project was started too")
	}
}

// TestStopStackReversesTheOrder: what depends on something else has to go down
// before the thing it depends on, or the database is pulled out from under an app
// that is still writing to it.
func TestStopStackReversesTheOrder(t *testing.T) {
	engine := newFakeEngine()
	stackMemberContainer(engine, "aaaa11110000", "blog", "web", true, "db:service_started:true")
	stackMemberContainer(engine, "bbbb22220000", "blog", "db", true, "")

	containers := newTestManager(t, engine, "")

	result, err := containers.StopStack(context.Background(), "blog")
	if err != nil {
		t.Fatalf("StopStack: %v", err)
	}
	if result.Changed != 2 {
		t.Errorf("Changed = %d, want both containers stopped", result.Changed)
	}

	stopped := touched(engine, "stop")
	want := []string{"blog-web-1", "blog-db-1"}
	if strings.Join(stopped, ",") != strings.Join(want, ",") {
		t.Errorf("stop order %v, want %v", stopped, want)
	}
	for _, name := range want {
		if engine.byName(name).Running {
			t.Errorf("%s is still running", name)
		}
	}
}

// TestRestartStackGoesForwardsAndTouchesOnlyWhatIsUp: a restart is a start's order,
// not a stop's — the database has to be back before the thing that talks to it. And it
// restarts what is running rather than bringing the stack up: the button beside it does
// that, and a "restart" that quietly starts three containers nobody asked for is the
// wrong surprise from a button whose neighbour is Stop all.
func TestRestartStackGoesForwardsAndTouchesOnlyWhatIsUp(t *testing.T) {
	engine := newFakeEngine()
	stackMemberContainer(engine, "aaaa11110000", "blog", "web", true, "db:service_started:true")
	stackMemberContainer(engine, "bbbb22220000", "blog", "db", true, "")
	stackMemberContainer(engine, "cccc33330000", "blog", "worker", false, "")

	containers := newTestManager(t, engine, "")

	result, err := containers.RestartStack(context.Background(), "blog")
	if err != nil {
		t.Fatalf("RestartStack: %v", err)
	}
	if result.Changed != 2 || result.Total != 3 {
		t.Errorf("Changed=%d Total=%d, want the two running containers of three", result.Changed, result.Total)
	}

	restarted := touched(engine, "restart")
	want := []string{"blog-db-1", "blog-web-1"}
	if strings.Join(restarted, ",") != strings.Join(want, ",") {
		t.Errorf("restart order %v, want %v", restarted, want)
	}
	if started := touched(engine, "start"); len(started) != 0 {
		t.Errorf("started %v, want a stopped container left alone", started)
	}
	if engine.byName("blog-worker-1").Running {
		t.Error("the container that was down was started by a restart")
	}
	if want := "Stack blog: 2 of 3 containers restarted."; result.Message() != want {
		t.Errorf("Message() = %q, want %q", result.Message(), want)
	}
}

// TestRestartStackWithNothingRunning: "already restarted" is not a state anything can
// be in, so the outcome has to say what actually happened.
func TestRestartStackWithNothingRunning(t *testing.T) {
	engine := newFakeEngine()
	stackMemberContainer(engine, "aaaa11110000", "blog", "web", false, "")

	containers := newTestManager(t, engine, "")

	result, err := containers.RestartStack(context.Background(), "blog")
	if err != nil {
		t.Fatalf("RestartStack: %v", err)
	}
	if result.Changed != 0 {
		t.Errorf("Changed = %d, want nothing touched", result.Changed)
	}
	if want := "Stack blog has nothing running to restart."; result.Message() != want {
		t.Errorf("Message() = %q, want %q", result.Message(), want)
	}
}

// TestRestartStackLeavesDockontrolerAlone: restarting the stack it lives in would kill
// the request half-way through, exactly as stopping it would.
func TestRestartStackLeavesDockontrolerAlone(t *testing.T) {
	engine := newFakeEngine()
	self := stackMemberContainer(engine, "5e1f00000000", "tools", "dockontroler", true, "")
	stackMemberContainer(engine, "aaaa11110000", "tools", "watchtower", true, "")

	containers := newTestManager(t, engine, self.ID)

	result, err := containers.RestartStack(context.Background(), "tools")
	if err != nil {
		t.Fatalf("RestartStack: %v", err)
	}
	if restarted := touched(engine, "restart"); len(restarted) != 1 || restarted[0] != "tools-watchtower-1" {
		t.Errorf("restarted %v, want only the other container", restarted)
	}
	if len(result.Notes) != 1 || !strings.Contains(result.Notes[0], "docKontroler") {
		t.Errorf("Notes = %q, want one saying docKontroler was left alone", result.Notes)
	}
}

// TestStopStackLeavesDockontrolerRunning: stopping the stack docKontroler happens to
// live in would kill the request half-way through and leave the rest of the stack
// wherever it had got to. Everything else still goes down, and the user is told.
func TestStopStackLeavesDockontrolerRunning(t *testing.T) {
	engine := newFakeEngine()
	self := stackMemberContainer(engine, "5e1f00000000", "tools", "dockontroler", true, "")
	stackMemberContainer(engine, "aaaa11110000", "tools", "watchtower", true, "")

	containers := newTestManager(t, engine, self.ID)

	result, err := containers.StopStack(context.Background(), "tools")
	if err != nil {
		t.Fatalf("StopStack: %v", err)
	}
	if result.Changed != 1 {
		t.Errorf("Changed = %d, want only the other container stopped", result.Changed)
	}
	if !engine.byName("tools-dockontroler-1").Running {
		t.Fatal("docKontroler stopped itself as part of its own stack")
	}
	if engine.byName("tools-watchtower-1").Running {
		t.Error("the rest of the stack was left running")
	}
	if len(result.Notes) != 1 || !strings.Contains(result.Notes[0], "docKontroler") {
		t.Errorf("Notes = %q, want one saying docKontroler was left alone", result.Notes)
	}
}

// TestStackSkipsWhatIsAlreadyInState: a stack somebody starts twice is the normal
// case, and it must read as "nothing to do" rather than as an error.
func TestStackSkipsWhatIsAlreadyInState(t *testing.T) {
	engine := newFakeEngine()
	stackMemberContainer(engine, "aaaa11110000", "blog", "web", true, "db:service_started:true")
	stackMemberContainer(engine, "bbbb22220000", "blog", "db", false, "")

	containers := newTestManager(t, engine, "")

	result, err := containers.StartStack(context.Background(), "blog")
	if err != nil {
		t.Fatalf("StartStack: %v", err)
	}
	if result.Changed != 1 || result.Total != 2 {
		t.Errorf("Changed=%d Total=%d, want only the stopped container touched", result.Changed, result.Total)
	}
	if started := touched(engine, "start"); len(started) != 1 || started[0] != "blog-db-1" {
		t.Errorf("started %v, want only blog-db-1", started)
	}

	// And again, with the whole stack up.
	engine2 := newFakeEngine()
	stackMemberContainer(engine2, "aaaa11110000", "blog", "web", true, "")
	containers2 := newTestManager(t, engine2, "")

	result, err = containers2.StartStack(context.Background(), "blog")
	if err != nil {
		t.Fatalf("StartStack on a running stack: %v", err)
	}
	if result.Changed != 0 {
		t.Errorf("Changed = %d, want nothing touched", result.Changed)
	}
	if want := "Stack blog was already running."; result.Message() != want {
		t.Errorf("Message() = %q, want %q", result.Message(), want)
	}
}

// TestStackReportsPartialFailure: one container refusing to stop must not hide that
// the others did, and must not read as a success either.
func TestStackReportsPartialFailure(t *testing.T) {
	engine := newFakeEngine()
	stackMemberContainer(engine, "aaaa11110000", "blog", "web", true, "db:service_started:true")
	stackMemberContainer(engine, "bbbb22220000", "blog", "db", true, "")

	failing := hexID("aaaa11110000")
	engine.failOn = func(method, path string) (int, string, bool) {
		if path == "/containers/"+failing+"/stop" {
			return http.StatusInternalServerError, "cannot stop container: device or resource busy", true
		}
		return 0, "", false
	}

	containers := newTestManager(t, engine, "")

	result, err := containers.StopStack(context.Background(), "blog")
	if err == nil {
		t.Fatal("StopStack returned no error although a container failed to stop")
	}
	if !strings.Contains(err.Error(), "blog-web-1") || !strings.Contains(err.Error(), "device or resource busy") {
		t.Errorf("error = %q, want it to name the container and the daemon's reason", err)
	}
	if result.Changed != 1 {
		t.Errorf("Changed = %d, want the container that could be stopped counted", result.Changed)
	}
	// The failure must not stop the walk: the rest of the stack still goes down.
	if engine.byName("blog-db-1").Running {
		t.Error("the walk stopped at the first failure instead of continuing")
	}
}

func TestStackOnUnknownProject(t *testing.T) {
	engine := newFakeEngine()
	runningContainer(engine, "111111111111", "pihole") // no Compose labels at all
	containers := newTestManager(t, engine, "")

	for _, project := range []string{"blog", ""} {
		if _, err := containers.StartStack(context.Background(), project); !errors.Is(err, ErrNotFound) {
			t.Errorf("StartStack(%q) = %v, want ErrNotFound", project, err)
		}
	}
}

// TestOrderMembersWithoutLabels: containers created before Compose v2 carry no
// depends_on label, so there is nothing to order by but the service name — which at
// least has to be stable, rather than whatever the map iterated to.
func TestOrderMembersWithoutLabels(t *testing.T) {
	members := []stackMember{
		{name: "s-web-1", service: "web"},
		{name: "s-api-1", service: "api"},
		{name: "s-db-1", service: "db"},
	}

	for attempt := 0; attempt < 5; attempt++ {
		ordered := orderMembers(members, false)
		got := make([]string, 0, len(ordered))
		for _, member := range ordered {
			got = append(got, member.service)
		}
		if strings.Join(got, ",") != "api,db,web" {
			t.Fatalf("order = %v, want the service names in order", got)
		}
	}
}

// TestOrderMembersSurvivesACycle: a dependency cycle is something Compose itself
// refuses, so it can only come from a hand-edited label — but it must not hang the
// operation that reads it.
func TestOrderMembersSurvivesACycle(t *testing.T) {
	members := []stackMember{
		{name: "s-a-1", service: "a", deps: []string{"b"}},
		{name: "s-b-1", service: "b", deps: []string{"a"}},
		{name: "s-c-1", service: "c"},
	}

	ordered := orderMembers(members, false)
	if len(ordered) != 3 {
		t.Fatalf("ordered %d members, want all 3", len(ordered))
	}
	if ordered[0].service != "c" {
		t.Errorf("first = %q, want the service outside the cycle to be ordered normally", ordered[0].service)
	}
}

// TestOrderMembersIgnoresAbsentDependencies: a depends_on entry whose service has no
// container here — scaled to zero, or removed — must not block what waits for it.
func TestOrderMembersIgnoresAbsentDependencies(t *testing.T) {
	members := []stackMember{
		{name: "s-web-1", service: "web", deps: []string{"gone"}},
	}

	if ordered := orderMembers(members, false); len(ordered) != 1 {
		t.Fatalf("ordered %d members, want the one that is here", len(ordered))
	}
}

// TestOrderMembersKeepsScaledServicesTogether: two containers of one service are one
// node of the graph, and their order between themselves must not depend on map
// iteration either.
func TestOrderMembersKeepsScaledServicesTogether(t *testing.T) {
	members := []stackMember{
		{name: "s-worker-2", service: "worker", deps: []string{"db"}},
		{name: "s-db-1", service: "db"},
		{name: "s-worker-1", service: "worker", deps: []string{"db"}},
	}

	ordered := orderMembers(members, false)
	got := make([]string, 0, len(ordered))
	for _, member := range ordered {
		got = append(got, member.name)
	}
	if want := "s-db-1,s-worker-1,s-worker-2"; strings.Join(got, ",") != want {
		t.Errorf("order = %v, want %s", got, want)
	}
}
