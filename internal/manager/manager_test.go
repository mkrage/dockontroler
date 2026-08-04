package manager

import (
	"context"
	"errors"
	"net/http"
	"strings"
	"testing"

	"github.com/mkrage/dockontroler/internal/docker"
)

func runningContainer(engine *fakeEngine, id, name string) *fakeContainer {
	return engine.add(&fakeContainer{
		ID:      hexID(id),
		Name:    name,
		Running: true,
		Image:   name + ":latest",
		Config:  map[string]any{"Image": name + ":latest"},
		HostConfig: map[string]any{
			"NetworkMode":   "bridge",
			"RestartPolicy": map[string]any{"Name": docker.PolicyNo, "MaximumRetryCount": float64(0)},
		},
	})
}

func TestStartAndStop(t *testing.T) {
	engine := newFakeEngine()
	target := runningContainer(engine, "111111111111", "pihole")
	containers := newTestManager(t, engine, "")
	ctx := context.Background()

	if err := containers.Stop(ctx, target.ID); err != nil {
		t.Fatalf("Stop: %v", err)
	}
	if engine.byName("pihole").Running {
		t.Error("container is still running after Stop")
	}

	if err := containers.Start(ctx, target.ID); err != nil {
		t.Fatalf("Start: %v", err)
	}
	if !engine.byName("pihole").Running {
		t.Error("container is not running after Start")
	}
}

// TestStartIsIdempotent covers the Engine answering 304 for "already in that
// state", which the client must treat as success: the user's intent is satisfied.
func TestStartIsIdempotent(t *testing.T) {
	engine := newFakeEngine()
	target := runningContainer(engine, "111111111111", "pihole")
	containers := newTestManager(t, engine, "")

	if err := containers.Start(context.Background(), target.ID); err != nil {
		t.Fatalf("starting an already running container: %v", err)
	}
}

func TestSelfProtection(t *testing.T) {
	// The web UI sends full ids and the Telegram bot sends short ones, so the
	// guard has to recognise every way a container can be named.
	engine := newFakeEngine()
	self := runningContainer(engine, "5e1f00000000", "dockontroler")
	other := runningContainer(engine, "0000feed0000", "jellyfin")
	containers := newTestManager(t, engine, self.ID)
	ctx := context.Background()

	for _, ref := range []string{self.ID, shortID(self.ID), "dockontroler"} {
		t.Run("stop via "+ref, func(t *testing.T) {
			if err := containers.Stop(ctx, ref); !errors.Is(err, ErrProtected) {
				t.Errorf("Stop(%q) = %v, want ErrProtected", ref, err)
			}
		})
		t.Run("restart via "+ref, func(t *testing.T) {
			if err := containers.Restart(ctx, ref); !errors.Is(err, ErrProtected) {
				t.Errorf("Restart(%q) = %v, want ErrProtected", ref, err)
			}
		})
	}

	if !engine.byName("dockontroler").Running {
		t.Error("dockontroler stopped itself despite the guard")
	}

	// Other containers must remain fully controllable.
	if err := containers.Stop(ctx, other.ID); err != nil {
		t.Errorf("Stop on another container: %v", err)
	}
}

// TestSetPolicyAllowedForOwnContainer: changing when a container starts next time
// touches no running state, and being able to set your own policy is useful.
func TestSetPolicyAllowedForOwnContainer(t *testing.T) {
	engine := newFakeEngine()
	self := runningContainer(engine, "5e1f00000000", "dockontroler")
	containers := newTestManager(t, engine, self.ID)

	if err := containers.SetPolicy(context.Background(), self.ID, docker.PolicyUnlessStopped); err != nil {
		t.Fatalf("SetPolicy on own container: %v", err)
	}
}

func TestSetPolicy(t *testing.T) {
	engine := newFakeEngine()
	target := runningContainer(engine, "111111111111", "pihole")
	containers := newTestManager(t, engine, "")
	ctx := context.Background()

	for _, policy := range []string{docker.PolicyAlways, docker.PolicyUnlessStopped, docker.PolicyNo} {
		if err := containers.SetPolicy(ctx, target.ID, policy); err != nil {
			t.Fatalf("SetPolicy(%s): %v", policy, err)
		}
		got := engine.byName("pihole").HostConfig["RestartPolicy"].(map[string]any)["Name"]
		if got != policy {
			t.Errorf("daemon policy = %v, want %s", got, policy)
		}
	}
}

func TestSetPolicyRejectsUnsupportedValues(t *testing.T) {
	engine := newFakeEngine()
	target := runningContainer(engine, "111111111111", "pihole")
	containers := newTestManager(t, engine, "")

	// on-failure is a real Docker policy but deliberately outside this tool's
	// surface, so it must be rejected rather than passed through.
	for _, policy := range []string{"on-failure", "on-failure:3", "", "ALWAYS"} {
		err := containers.SetPolicy(context.Background(), target.ID, policy)
		if !errors.Is(err, ErrUnsupported) {
			t.Errorf("SetPolicy(%q) = %v, want ErrUnsupported", policy, err)
		}
	}
}

func TestListGroupsByComposeProject(t *testing.T) {
	engine := newFakeEngine()
	addWithProject(engine, "aaa000000001", "zeta-api-1", "zeta", "api")
	addWithProject(engine, "aaa000000002", "alpha-db-1", "alpha", "db")
	addWithProject(engine, "aaa000000003", "alpha-web-1", "alpha", "web")
	runningContainer(engine, "aaa000000004", "pihole")
	containers := newTestManager(t, engine, "")

	overview, err := containers.List(context.Background())
	if err != nil {
		t.Fatalf("List: %v", err)
	}

	if overview.Total != 4 {
		t.Errorf("Total = %d, want 4", overview.Total)
	}

	var projects []string
	for _, group := range overview.Groups {
		projects = append(projects, group.Project)
	}
	// Projects alphabetically, then the containers Compose knows nothing about,
	// so real projects stay at the top of the page.
	want := []string{"alpha", "zeta", ""}
	if strings.Join(projects, ",") != strings.Join(want, ",") {
		t.Errorf("group order = %q, want %q", projects, want)
	}

	// Within a project, ordered by service name.
	alpha := overview.Groups[0].Containers
	if len(alpha) != 2 || alpha[0].ComposeService != "db" || alpha[1].ComposeService != "web" {
		t.Errorf("alpha members = %+v, want db before web", alpha)
	}
}

// TestListSurvivesAFailedInspect: one unreadable container must not blank the
// whole page, since the overview is what the user reaches for when something is
// already wrong.
// TestPolicyFileFromComposeLabel: a policy set here lives on the container, so it is
// overwritten the next time the container is rebuilt from its yaml. Compose records
// the files it used, which is the one thing that lets the UI say where to write the
// policy down permanently.
func TestPolicyFileFromComposeLabel(t *testing.T) {
	build := func(configFiles string) Container {
		summary := docker.ContainerSummary{
			ID:    hexID("blog"),
			Names: []string{"/blog-web-1"},
			State: docker.StateRunning,
			Labels: map[string]string{
				docker.LabelComposeProject:     "blog",
				docker.LabelComposeConfigFiles: configFiles,
			},
		}
		return newContainer(summary, nil, "")
	}

	t.Run("single file", func(t *testing.T) {
		got := build("/data/compose/7/docker-compose.yml")
		if want := "/data/compose/7/docker-compose.yml"; got.PolicyFile() != want {
			t.Errorf("PolicyFile() = %q, want %q", got.PolicyFile(), want)
		}
	})

	t.Run("overrides win", func(t *testing.T) {
		// Later files override earlier ones, so the last is where a restart: line
		// actually takes effect.
		got := build("/srv/blog/docker-compose.yml,/srv/blog/docker-compose.override.yml")
		if want := "/srv/blog/docker-compose.override.yml"; got.PolicyFile() != want {
			t.Errorf("PolicyFile() = %q, want %q", got.PolicyFile(), want)
		}
		if len(got.ComposeFiles) != 2 {
			t.Errorf("ComposeFiles = %q, want both files kept", got.ComposeFiles)
		}
	})

	t.Run("not compose managed", func(t *testing.T) {
		if got := build(""); got.PolicyFile() != "" {
			t.Errorf("PolicyFile() = %q, want empty so the UI stays quiet", got.PolicyFile())
		}
	})

	// The label is what a card has room for. Every Portainer stack shares the long
	// prefix, so the directory and file name are the only part that identifies one.
	t.Run("label keeps the identifying end", func(t *testing.T) {
		cases := map[string]string{
			"/Volume2/@apps/Portainer/compose/43/docker-compose.yml": "43/docker-compose.yml",
			"/srv/blog/docker-compose.override.yml":                  "blog/docker-compose.override.yml",
			`C:\stacks\blog\compose.yaml`:                            `blog\compose.yaml`,
			"docker-compose.yml":                                     "docker-compose.yml",
			"":                                                       "",
		}
		for files, want := range cases {
			if got := build(files).PolicyFileLabel(); got != want {
				t.Errorf("PolicyFileLabel() for %q = %q, want %q", files, got, want)
			}
		}
	})
}

func TestListSurvivesAFailedInspect(t *testing.T) {
	engine := newFakeEngine()
	runningContainer(engine, "111111111111", "pihole")
	broken := runningContainer(engine, "222222222222", "grafana")
	engine.failOn = func(method, path string) (int, string, bool) {
		if method == http.MethodGet && path == "/containers/"+broken.ID+"/json" {
			return http.StatusInternalServerError, "boom", true
		}
		return 0, "", false
	}
	containers := newTestManager(t, engine, "")

	overview, err := containers.List(context.Background())
	if err != nil {
		t.Fatalf("List: %v", err)
	}

	if overview.Total != 2 {
		t.Fatalf("Total = %d, want both containers listed", overview.Total)
	}
	if overview.Warning == "" {
		t.Error("Warning is empty, want the partial result flagged")
	}

	byName := map[string]Container{}
	for _, group := range overview.Groups {
		for _, container := range group.Containers {
			byName[container.Name] = container
		}
	}
	if byName["grafana"].Incomplete != true {
		t.Error("grafana should be marked incomplete")
	}
	if byName["grafana"].CanRecreate {
		t.Error("a container that could not be inspected must not offer recreate")
	}
	if byName["pihole"].Incomplete {
		t.Error("pihole was inspected fine and must not be marked incomplete")
	}
	if !byName["pihole"].CanRecreate {
		t.Error("pihole was inspected fine and should still offer recreate")
	}
}

func TestGetResolvesShortID(t *testing.T) {
	engine := newFakeEngine()
	target := runningContainer(engine, "abcdef123456", "jellyfin")
	containers := newTestManager(t, engine, "")

	container, err := containers.Get(context.Background(), shortID(target.ID))
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if container.Name != "jellyfin" {
		t.Errorf("Name = %q, want jellyfin", container.Name)
	}
	if container.ShortID != shortID(target.ID) {
		t.Errorf("ShortID = %q, want %q", container.ShortID, shortID(target.ID))
	}
	if container.Status == "" {
		t.Error("Status is empty; Get should pick up the daemon's status text")
	}
}

// TestBusySetRejectsRatherThanQueues documents the deliberate choice: a second
// click while the first action runs is refused, because queueing a start-stop-start
// sequence would replay intent the user has already seen the result of.
//
// The mechanism is tested directly rather than through Manager, which would need a
// blocking daemon to reproduce the race reliably.
func TestBusySetRejectsRatherThanQueues(t *testing.T) {
	busy := newBusySet()

	release, ok := busy.acquire("abc")
	if !ok {
		t.Fatal("first acquire failed")
	}
	if _, ok := busy.acquire("abc"); ok {
		t.Error("second acquire on a busy key succeeded, want refusal")
	}
	if otherRelease, ok := busy.acquire("xyz"); !ok {
		t.Error("a different container must not be blocked")
	} else {
		otherRelease()
	}

	release()
	if _, ok := busy.acquire("abc"); !ok {
		t.Error("acquire after release failed")
	}
}

func TestBusySetReleaseIsIdempotent(t *testing.T) {
	busy := newBusySet()
	release, _ := busy.acquire("abc")

	release()
	release() // a double defer must not free somebody else's later lock

	other, ok := busy.acquire("abc")
	if !ok {
		t.Fatal("acquire failed")
	}
	release() // the stale release must not touch the new holder
	if _, ok := busy.acquire("abc"); ok {
		t.Error("a stale release freed the lock held by another operation")
	}
	other()
}

func addWithProject(engine *fakeEngine, id, name, project, service string) *fakeContainer {
	container := runningContainer(engine, id, name)
	container.Labels = map[string]string{
		docker.LabelComposeProject: project,
		docker.LabelComposeService: service,
	}
	return container
}
