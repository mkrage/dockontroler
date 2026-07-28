package manager

import (
	"context"
	"errors"
	"net/http"
	"strings"
	"testing"

	"github.com/mkrage/dockontroler/internal/docker"
)

// recreatableContainer is a running Compose-style container on two networks, which
// is the shape that exercises the interesting parts of recreate.
func recreatableContainer(engine *fakeEngine) *fakeContainer {
	return engine.add(&fakeContainer{
		ID:      hexID("aaa111bbb222"),
		Name:    "blog-web-1",
		Running: true,
		Image:   "ghcr.io/me/blog:latest",
		Config: map[string]any{
			"Image":    "ghcr.io/me/blog:latest",
			"Hostname": "aaa111bbb222",
		},
		HostConfig: map[string]any{"NetworkMode": "blog_default"},
		Networks: map[string]docker.EndpointSettings{
			"blog_default": {Aliases: []string{"web"}},
			"proxy":        {},
		},
		Labels: map[string]string{
			docker.LabelComposeProject: "blog",
			docker.LabelComposeService: "web",
		},
	})
}

func TestRecreateReplacesTheContainer(t *testing.T) {
	engine := newFakeEngine()
	original := recreatableContainer(engine)
	containers := newTestManager(t, engine, "")

	result, err := containers.Recreate(context.Background(), original.ID)
	if err != nil {
		t.Fatalf("Recreate: %v", err)
	}

	if result.NewID == original.ID {
		t.Error("NewID equals the old id — nothing was actually replaced")
	}
	if engine.count() != 1 {
		t.Errorf("%d containers remain, want exactly 1 (the old one must be removed)", engine.count())
	}

	replacement := engine.byName("blog-web-1")
	if replacement == nil {
		t.Fatal("no container holds the original name after the recreate")
	}
	if replacement.ID != result.NewID {
		t.Errorf("the container named blog-web-1 is %s, want the replacement %s",
			shortID(replacement.ID), shortID(result.NewID))
	}
	if !replacement.Running {
		t.Error("the replacement is not running, but the original was")
	}

	// Only the secondary network goes through /networks/connect; the primary is
	// attached by the create call itself.
	if connected := engine.connected(); len(connected) != 1 || connected[0] != "proxy" {
		t.Errorf("connected networks = %q, want exactly [proxy]", connected)
	}
}

// TestRecreateRemovesOldContainerWithoutItsVolumes pins the flag that decides
// whether a recreate preserves a database or destroys it.
func TestRecreateRemovesOldContainerWithoutItsVolumes(t *testing.T) {
	engine := newFakeEngine()
	original := recreatableContainer(engine)
	containers := newTestManager(t, engine, "")

	if _, err := containers.Recreate(context.Background(), original.ID); err != nil {
		t.Fatalf("Recreate: %v", err)
	}

	query := engine.queryOf(t, http.MethodDelete, "/containers/"+original.ID)
	if got := query.Get("v"); got != "false" {
		t.Errorf("delete query v=%q, want \"false\" — removing volumes would destroy the data "+
			"the replacement is now using", got)
	}
}

func TestRecreateRollsBackWhenCreateFails(t *testing.T) {
	engine := newFakeEngine()
	original := recreatableContainer(engine)
	engine.failOn = func(method, path string) (int, string, bool) {
		if method == http.MethodPost && path == "/containers/create" {
			return http.StatusInternalServerError, "no such image: ghcr.io/me/blog:latest", true
		}
		return 0, "", false
	}
	containers := newTestManager(t, engine, "")

	_, err := containers.Recreate(context.Background(), original.ID)
	if err == nil {
		t.Fatal("Recreate succeeded, want the create failure reported")
	}
	if !strings.Contains(UserMessage(err), "no such image") {
		t.Errorf("UserMessage = %q, want the daemon's own explanation", UserMessage(err))
	}

	assertOriginalRestored(t, engine, original)
}

func TestRecreateRollsBackWhenTheReplacementWillNotStart(t *testing.T) {
	engine := newFakeEngine()
	original := recreatableContainer(engine)
	// Fail only the replacement's start. The rollback restarts the original, and
	// that must still work — otherwise the test would prove nothing.
	engine.failOn = func(method, path string) (int, string, bool) {
		if method == http.MethodPost && strings.HasSuffix(path, "/start") &&
			!strings.Contains(path, original.ID) {
			return http.StatusInternalServerError, "port is already allocated", true
		}
		return 0, "", false
	}
	containers := newTestManager(t, engine, "")

	_, err := containers.Recreate(context.Background(), original.ID)
	if err == nil {
		t.Fatal("Recreate succeeded, want the start failure reported")
	}
	if !strings.Contains(UserMessage(err), "port is already allocated") {
		t.Errorf("UserMessage = %q, want the daemon's own explanation", UserMessage(err))
	}

	assertOriginalRestored(t, engine, original)
}

func TestRecreateRollsBackWhenAnExtraNetworkFails(t *testing.T) {
	engine := newFakeEngine()
	original := recreatableContainer(engine)
	engine.failOn = func(method, path string) (int, string, bool) {
		if strings.HasPrefix(path, "/networks/proxy/") {
			return http.StatusInternalServerError, "network proxy not found", true
		}
		return 0, "", false
	}
	containers := newTestManager(t, engine, "")

	if _, err := containers.Recreate(context.Background(), original.ID); err == nil {
		t.Fatal("Recreate succeeded, want the network failure reported")
	}

	assertOriginalRestored(t, engine, original)
}

// assertOriginalRestored checks the property the whole rollback design exists for:
// after a failure, the host looks exactly as it did before.
func assertOriginalRestored(t *testing.T, engine *fakeEngine, original *fakeContainer) {
	t.Helper()

	if engine.count() != 1 {
		t.Errorf("%d containers remain, want 1 — the half-built replacement was not cleaned up",
			engine.count())
	}

	restored := engine.byName("blog-web-1")
	if restored == nil {
		t.Fatalf("no container holds the original name; it is probably stranded as %q",
			"blog-web-1"+backupSuffix)
	}
	if restored.ID != original.ID {
		t.Errorf("blog-web-1 is now %s, want the original %s",
			shortID(restored.ID), shortID(original.ID))
	}
	if !restored.Running {
		t.Error("the original was left stopped, but it was running before the recreate")
	}
}

func TestRecreateLeavesAStoppedContainerStopped(t *testing.T) {
	engine := newFakeEngine()
	original := recreatableContainer(engine)
	original.Running = false
	containers := newTestManager(t, engine, "")

	result, err := containers.Recreate(context.Background(), original.ID)
	if err != nil {
		t.Fatalf("Recreate: %v", err)
	}

	replacement := engine.byName("blog-web-1")
	if replacement == nil {
		t.Fatal("no container holds the original name")
	}
	if replacement.Running {
		t.Error("the replacement was started, but the original was stopped")
	}
	if engine.called(http.MethodPost, "/containers/"+result.NewID+"/start") {
		t.Error("start was called on the replacement of a stopped container")
	}
}

func TestRecreateRefusedForOwnContainer(t *testing.T) {
	engine := newFakeEngine()
	original := recreatableContainer(engine)
	containers := newTestManager(t, engine, original.ID)

	_, err := containers.Recreate(context.Background(), original.ID)

	if !errors.Is(err, ErrProtected) {
		t.Fatalf("err = %v, want ErrProtected — dockontroler must not replace itself mid-request", err)
	}
	if engine.called(http.MethodPost, "/containers/"+original.ID+"/stop") {
		t.Error("the container was stopped despite the refusal")
	}
}

func TestRecreateReportsMissingContainer(t *testing.T) {
	engine := newFakeEngine()
	containers := newTestManager(t, engine, "")

	_, err := containers.Recreate(context.Background(), hexID("deadbeefdead"))

	if !errors.Is(err, ErrNotFound) {
		t.Fatalf("err = %v, want ErrNotFound", err)
	}
}
