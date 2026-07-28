// Package manager holds Dockontroler's core logic.
//
// It knows nothing about HTTP or Telegram: both the web handlers and the bot are
// thin adapters over this package, which is what keeps the two interfaces from
// drifting apart or duplicating rules like "never stop yourself".
package manager

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"sync"
	"time"

	"github.com/mkrage/dockontroler/internal/docker"
)

// Errors callers are expected to distinguish. Everything else is passed through
// from the Engine.
var (
	// ErrNotFound means the container is gone — usually because it was removed
	// between rendering the page and clicking a button.
	ErrNotFound = errors.New("container not found")
	// ErrBusy means another operation on the same container is still running.
	ErrBusy = errors.New("another operation on this container is still running")
	// ErrProtected means the action was refused on Dockontroler's own container.
	ErrProtected = errors.New("dockontroler cannot do this to its own container")
	// ErrUnsupported means the container cannot be recreated, with the reason in
	// the wrapped message.
	ErrUnsupported = errors.New("not supported for this container")
)

// inspectConcurrency bounds the parallel inspect calls behind one List. The list
// endpoint omits the restart policy, so every container has to be inspected; a
// small pool keeps a fifty-container host well under 50 ms without opening fifty
// connections to the socket at once.
const inspectConcurrency = 8

// Manager performs container operations and builds the display model.
type Manager struct {
	docker      *docker.Client
	log         *slog.Logger
	stopTimeout time.Duration
	// selfID is the full id of Dockontroler's own container, or "" if it could
	// not be determined — in which case self-protection is inactive.
	selfID string
	busy   *busySet
}

// New returns a Manager. selfID may be empty; see DetectSelfID.
func New(client *docker.Client, log *slog.Logger, stopTimeout time.Duration, selfID string) *Manager {
	return &Manager{
		docker:      client,
		log:         log,
		stopTimeout: stopTimeout,
		selfID:      selfID,
		busy:        newBusySet(),
	}
}

// SelfID returns the id of Dockontroler's own container, or "" if unknown.
func (m *Manager) SelfID() string { return m.selfID }

// List returns every container on the host, grouped by Compose project.
func (m *Manager) List(ctx context.Context) (Overview, error) {
	summaries, err := m.docker.ListContainers(ctx)
	if err != nil {
		return Overview{}, fmt.Errorf("list containers: %w", err)
	}

	details := m.inspectAll(ctx, summaries)

	containers := make([]Container, 0, len(summaries))
	incomplete := 0
	for i, summary := range summaries {
		container := newContainer(summary, details[i], m.selfID)
		if container.Incomplete {
			incomplete++
		}
		containers = append(containers, container)
	}

	overview := buildOverview(containers)
	if incomplete > 0 {
		overview.Warning = fmt.Sprintf(
			"%d of %d containers could not be inspected; their restart policy may be shown incorrectly",
			incomplete, len(summaries))
	}
	return overview, nil
}

// Get returns a single container in display form. ref may be a full id, a short
// id or a name.
func (m *Manager) Get(ctx context.Context, ref string) (Container, error) {
	inspected, err := m.resolve(ctx, ref)
	if err != nil {
		return Container{}, err
	}

	// The list endpoint is the only source of the Engine's human-readable status
	// text ("Up 3 hours"), so find this container's entry in it.
	summary := docker.ContainerSummary{
		ID:     inspected.ID,
		Names:  []string{inspected.Name},
		State:  inspected.State.Status,
		Status: inspected.State.Status,
	}
	if summaries, err := m.docker.ListContainers(ctx); err == nil {
		for _, candidate := range summaries {
			if candidate.ID == inspected.ID {
				summary = candidate
				break
			}
		}
	}
	return newContainer(summary, inspected, m.selfID), nil
}

// Start starts a container. Already-running containers are accepted silently:
// the Engine reports that as success and the user's intent is satisfied either
// way.
func (m *Manager) Start(ctx context.Context, ref string) error {
	inspected, release, err := m.begin(ctx, ref)
	if err != nil {
		return err
	}
	defer release()

	m.log.Info("starting container", "container", inspected.Name, "id", shortID(inspected.ID))
	return translate(m.docker.StartContainer(ctx, inspected.ID))
}

// Stop stops a container, refusing to stop Dockontroler itself.
func (m *Manager) Stop(ctx context.Context, ref string) error {
	inspected, release, err := m.begin(ctx, ref)
	if err != nil {
		return err
	}
	defer release()
	if err := m.guardSelf(inspected, "stop"); err != nil {
		return err
	}

	m.log.Info("stopping container", "container", inspected.Name, "id", shortID(inspected.ID))
	return translate(m.docker.StopContainer(ctx, inspected.ID, m.stopTimeout))
}

// Restart restarts a container.
//
// This reuses the same container, so it does not pick up a rebuilt image — that
// is what Recreate is for.
func (m *Manager) Restart(ctx context.Context, ref string) error {
	inspected, release, err := m.begin(ctx, ref)
	if err != nil {
		return err
	}
	defer release()
	if err := m.guardSelf(inspected, "restart"); err != nil {
		return err
	}

	m.log.Info("restarting container", "container", inspected.Name, "id", shortID(inspected.ID))
	return translate(m.docker.RestartContainer(ctx, inspected.ID, m.stopTimeout))
}

// SetPolicy changes when Docker will start this container again. It works on
// stopped containers too and never restarts anything by itself.
//
// This is allowed on Dockontroler's own container: it changes no running state,
// and being able to set your own policy to unless-stopped is useful.
func (m *Manager) SetPolicy(ctx context.Context, ref, policy string) error {
	switch policy {
	case docker.PolicyNo, docker.PolicyAlways, docker.PolicyUnlessStopped:
	default:
		return fmt.Errorf("%w: unknown restart policy %q", ErrUnsupported, policy)
	}

	inspected, release, err := m.begin(ctx, ref)
	if err != nil {
		return err
	}
	defer release()

	warnings, err := m.docker.SetRestartPolicy(ctx, inspected.ID, policy)
	if err != nil {
		return translate(err)
	}
	for _, warning := range warnings {
		m.log.Warn("docker warning while setting restart policy",
			"container", inspected.Name, "warning", warning)
	}
	m.log.Info("restart policy changed",
		"container", inspected.Name, "id", shortID(inspected.ID), "policy", policy)
	return nil
}

// begin is the shared preamble of every operation: resolve the reference, then
// take the per-container lock so a double click cannot run twice.
//
// The returned release function must always be called.
func (m *Manager) begin(ctx context.Context, ref string) (*docker.ContainerInspect, func(), error) {
	inspected, err := m.resolve(ctx, ref)
	if err != nil {
		return nil, nil, err
	}
	release, ok := m.busy.acquire(inspected.ID)
	if !ok {
		return nil, nil, ErrBusy
	}
	return inspected, release, nil
}

// resolve accepts a full id, a short id or a name and returns the container.
//
// Every operation goes through this, which means the per-container lock and the
// self check always key on the real container id no matter how the caller
// referred to it — the web UI uses full ids, the bot uses short ones.
func (m *Manager) resolve(ctx context.Context, ref string) (*docker.ContainerInspect, error) {
	if ref == "" {
		return nil, ErrNotFound
	}
	inspected, err := m.docker.InspectContainer(ctx, ref)
	if err != nil {
		return nil, translate(err)
	}
	return inspected, nil
}

// guardSelf refuses actions that would take down Dockontroler mid-request.
func (m *Manager) guardSelf(inspected *docker.ContainerInspect, action string) error {
	if m.selfID != "" && inspected.ID == m.selfID {
		return fmt.Errorf("%w (cannot %s itself)", ErrProtected, action)
	}
	return nil
}

// inspectAll inspects every listed container concurrently. Failures yield a nil
// entry rather than an error: a container removed between list and inspect is
// normal, and one missing detail should not blank the whole page.
func (m *Manager) inspectAll(ctx context.Context, summaries []docker.ContainerSummary) []*docker.ContainerInspect {
	results := make([]*docker.ContainerInspect, len(summaries))

	var wait sync.WaitGroup
	slots := make(chan struct{}, inspectConcurrency)
	for i, summary := range summaries {
		wait.Add(1)
		go func(index int, id string) {
			defer wait.Done()
			slots <- struct{}{}
			defer func() { <-slots }()

			inspected, err := m.docker.InspectContainer(ctx, id)
			if err != nil {
				m.log.Debug("inspect failed while listing", "id", shortID(id), "error", err)
				return
			}
			results[index] = inspected
		}(i, summary.ID)
	}
	wait.Wait()
	return results
}

// translate maps Engine errors onto this package's sentinels so callers can
// react without knowing about HTTP status codes.
func translate(err error) error {
	if err == nil {
		return nil
	}
	if docker.IsNotFound(err) {
		return ErrNotFound
	}
	return err
}

// UserMessage renders err for display.
//
// The Engine's own wording is preferred where available — "port is already
// allocated" or "no such image" is far more useful than anything this package
// could invent — with the HTTP plumbing left out.
func UserMessage(err error) string {
	if err == nil {
		return ""
	}
	var apiErr *docker.APIError
	if errors.As(err, &apiErr) && apiErr.Message != "" {
		return apiErr.Message
	}
	return err.Error()
}

// busySet tracks which containers currently have an operation running.
//
// A second request for a busy container is rejected instead of queued: queueing
// a double-clicked stop would replay intent whose result the user has already
// seen, and start-stop-start racing through a queue is worse than an honest
// "still working on it".
type busySet struct {
	mu       sync.Mutex
	inFlight map[string]struct{}
}

func newBusySet() *busySet {
	return &busySet{inFlight: map[string]struct{}{}}
}

// acquire reserves key. ok is false if it was already taken; release must be
// called exactly once when ok is true.
func (b *busySet) acquire(key string) (release func(), ok bool) {
	b.mu.Lock()
	defer b.mu.Unlock()

	if _, taken := b.inFlight[key]; taken {
		return nil, false
	}
	b.inFlight[key] = struct{}{}

	var once sync.Once
	return func() {
		once.Do(func() {
			b.mu.Lock()
			delete(b.inFlight, key)
			b.mu.Unlock()
		})
	}, true
}
