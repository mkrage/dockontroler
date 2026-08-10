package manager

import (
	"context"
	"fmt"
	"sort"
	"strings"
	"time"

	"github.com/mkrage/dockontroler/internal/docker"
)

// The three things a stack operation can be. Spelled out rather than a bool,
// because the direction decides both the order the stack is walked in and every
// message.
const (
	stackStart   = "start"
	stackStop    = "stop"
	stackRestart = "restart"
)

// StackResult reports what a project-wide start, stop or restart did.
type StackResult struct {
	Project string
	// Action is stackStart or stackStop.
	Action string
	// Changed is how many containers were actually started or stopped. It is lower
	// than Total whenever part of the stack was already in the requested state,
	// which is the normal case rather than the exception.
	Changed int
	// Total is how many containers the project has.
	Total int
	// Notes are containers that were deliberately left alone, e.g. docKontroler's
	// own. Not failures: the operation did what it could.
	Notes []string
}

// Message is the one-line outcome, ready for a toast or a chat reply.
func (r StackResult) Message() string {
	done := "started"
	// What to say when the operation found nothing to do. A restart needs its own
	// wording: "already restarted" is not a state anything can be in, and what
	// actually happened is that there was nothing running to restart.
	settled := fmt.Sprintf("Stack %s was already running.", r.Project)
	switch r.Action {
	case stackStop:
		done = "stopped"
		settled = fmt.Sprintf("Stack %s was already stopped.", r.Project)
	case stackRestart:
		done = "restarted"
		settled = fmt.Sprintf("Stack %s has nothing running to restart.", r.Project)
	}

	switch {
	case r.Changed == 0 && len(r.Notes) > 0:
		// Something was deliberately left alone, so "already running" would be a
		// claim about it that the notes contradict.
		return fmt.Sprintf("Nothing to %s in stack %s.", r.Action, r.Project)
	case r.Changed == 0:
		return settled
	case r.Changed == r.Total:
		return fmt.Sprintf("Stack %s %s, all %d containers.", r.Project, done, r.Total)
	default:
		return fmt.Sprintf("Stack %s: %d of %d containers %s.", r.Project, r.Changed, r.Total, done)
	}
}

// StartStack starts every container of a Compose project, dependencies first.
//
// This is the operation the overview is usually after: a stack is the unit
// somebody deploys, and its containers are rarely interesting one at a time.
func (m *Manager) StartStack(ctx context.Context, project string) (StackResult, error) {
	return m.actOnStack(ctx, project, stackStart)
}

// StopStack stops every container of a Compose project, dependents first.
//
// docKontroler's own container is skipped if it happens to be part of the stack,
// with a note saying so: stopping it would kill the request mid-flight and leave
// the rest of the stack in whatever state it had reached.
func (m *Manager) StopStack(ctx context.Context, project string) (StackResult, error) {
	return m.actOnStack(ctx, project, stackStop)
}

// RestartStack restarts every running container of a Compose project, dependencies
// first — the operation after a config change, where the alternative is stopping the
// stack and starting it again and hoping nothing was missed in between.
//
// It restarts what is up and leaves what is down alone. Bringing a stopped container
// up is what the button next to this one does, and a "restart" that quietly starts
// three containers nobody asked for is the wrong kind of surprise for a button whose
// neighbour is Stop all.
func (m *Manager) RestartStack(ctx context.Context, project string) (StackResult, error) {
	return m.actOnStack(ctx, project, stackRestart)
}

// stackKey is the busy-set key for a whole project. Container keys are 64-character
// hex ids, so the prefix cannot collide with one.
func stackKey(project string) string { return "stack:" + project }

func (m *Manager) actOnStack(ctx context.Context, project, action string) (StackResult, error) {
	project = strings.TrimSpace(project)
	if project == "" {
		return StackResult{}, fmt.Errorf("%w: no stack given", ErrNotFound)
	}

	members, err := m.stackMembers(ctx, project)
	if err != nil {
		return StackResult{}, err
	}
	if len(members) == 0 {
		return StackResult{}, fmt.Errorf("%w: no container belongs to the stack %s", ErrNotFound, project)
	}

	// One operation per stack at a time. The per-container locks below would catch
	// most of a double click anyway, but as a pile of "still running" notes rather
	// than as the one honest answer.
	release, ok := m.busy.acquire(stackKey(project))
	if !ok {
		return StackResult{}, fmt.Errorf("%w on the stack %s", ErrBusy, project)
	}
	defer release()

	// Only a stop walks the graph backwards. A restart goes the way a start does:
	// the database comes back before the thing that talks to it.
	order := orderMembers(members, action == stackStop)
	log := m.log.With("stack", project, "action", action)
	log.Info("stack operation starting", "containers", len(order))

	// Detached from the caller's context for the reason a recreate is: a stack stop
	// abandoned half-way leaves the project in a state nobody asked for, and the
	// caller closing a browser tab is not a reason to stop in the middle. The
	// deadline scales with the stack, because the containers are walked one at a
	// time and each may use its full grace period.
	opCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), m.stackDeadline(len(order)))
	defer cancel()

	result := StackResult{Project: project, Action: action, Total: len(order)}
	var failures []string

	for _, member := range order {
		skip, note := member.skipReason(action)
		if note != "" {
			result.Notes = append(result.Notes, note)
		}
		if skip {
			continue
		}

		memberRelease, ok := m.busy.acquire(member.id)
		if !ok {
			failures = append(failures, member.name+": another operation on it is still running")
			continue
		}
		var opErr error
		switch action {
		case stackStop:
			opErr = m.docker.StopContainer(opCtx, member.id, m.stopTimeout)
		case stackRestart:
			opErr = m.docker.RestartContainer(opCtx, member.id, m.stopTimeout)
		default:
			opErr = m.docker.StartContainer(opCtx, member.id)
		}
		memberRelease()

		switch {
		case opErr == nil:
			result.Changed++
		case docker.IsNotFound(opErr):
			// Removed between listing the stack and getting here. Nothing to do, and
			// nothing the user needs to hear about.
			log.Debug("container disappeared during the stack operation", "container", member.name)
		default:
			failures = append(failures, member.name+": "+UserMessage(opErr))
		}
	}

	log.Info("stack operation finished", "changed", result.Changed, "failed", len(failures))
	if len(failures) > 0 {
		// Deliberately not phrased as "3 of 7", which would count the containers that
		// were already in the requested state as failures.
		done := "started"
		switch action {
		case stackStop:
			done = "stopped"
		case stackRestart:
			done = "restarted"
		}
		return result, fmt.Errorf("stack %s: %d %s, %d failed; %s",
			project, result.Changed, done, len(failures), strings.Join(failures, "; "))
	}
	return result, nil
}

// stackDeadline bounds a whole project-wide operation.
//
// A flat number would be wrong at both ends: a stack of ten containers that each
// use their full stop timeout needs minutes, and a stack of two should not be able
// to hang for them.
func (m *Manager) stackDeadline(members int) time.Duration {
	deadline := time.Duration(members) * (m.stopTimeout + 20*time.Second)
	switch {
	case deadline < time.Minute:
		return time.Minute
	case deadline > 20*time.Minute:
		return 20 * time.Minute
	default:
		return deadline
	}
}

// stackMember is one container of a project, with just enough to decide whether to
// touch it and in which order.
type stackMember struct {
	id      string
	name    string
	service string
	// deps are the services this one waits for, from Compose's own label.
	deps   []string
	state  string
	isSelf bool
}

// skipReason decides whether this container should be left alone for the given
// action, and returns the note the user should see when the reason is worth saying
// out loud. A container that is already in the requested state is skipped silently:
// that is the normal case, not something to report.
func (s stackMember) skipReason(action string) (skip bool, note string) {
	if action == stackRestart {
		switch {
		case !isActiveState(s.state):
			// Silently: a stack that is half down is the normal case for this button,
			// and naming every container it left alone would bury the outcome.
			return true, ""
		case s.isSelf:
			return true, "left docKontroler itself running"
		case s.state == docker.StatePaused:
			// Same as starting: what a paused container needs is unpausing, which
			// docKontroler does not offer, and Docker will not do it for a restart.
			return true, s.name + " is paused, which restarting cannot resume"
		}
		return false, ""
	}

	if action == stackStop {
		switch {
		case !isActiveState(s.state):
			return true, ""
		case s.isSelf:
			return true, "left docKontroler itself running"
		}
		return false, ""
	}

	switch s.state {
	case docker.StateRunning:
		return true, ""
	case docker.StatePaused:
		// Docker refuses to start a paused container; it has to be unpaused, which
		// docKontroler does not offer. Saying so beats a failure nobody can act on.
		return true, s.name + " is paused, which starting cannot resume"
	case docker.StateRestarting:
		return true, ""
	}
	return false, ""
}

// stackMembers finds every container of a project.
//
// The list endpoint carries the Compose labels, so this needs no inspect call —
// which matters, because the alternative is one per container before anything at
// all happens.
func (m *Manager) stackMembers(ctx context.Context, project string) ([]stackMember, error) {
	summaries, err := m.docker.ListContainers(ctx)
	if err != nil {
		return nil, fmt.Errorf("list containers: %w", err)
	}

	var members []stackMember
	for _, summary := range summaries {
		if summary.Labels[docker.LabelComposeProject] != project {
			continue
		}
		members = append(members, stackMember{
			id:      summary.ID,
			name:    strings.TrimPrefix(firstName(summary.Names), "/"),
			service: summary.Labels[docker.LabelComposeService],
			deps:    dependsOn(summary.Labels[docker.LabelComposeDependsOn]),
			state:   summary.State,
			isSelf:  m.selfID != "" && summary.ID == m.selfID,
		})
	}
	return members, nil
}

// dependsOn reads Compose's depends_on label, whose entries look like
// "db:service_started:true".
//
// Only the service name is taken. The condition says how long `compose up` waits
// before moving on, and waiting for a health check is not something this can do
// from the outside — so the order is honoured, the gating is not.
func dependsOn(raw string) []string {
	if raw == "" {
		return nil
	}
	var deps []string
	for _, entry := range strings.Split(raw, ",") {
		if service, _, _ := strings.Cut(strings.TrimSpace(entry), ":"); service != "" {
			deps = append(deps, service)
		}
	}
	return deps
}

// orderMembers puts the stack into the order the operation should walk it:
// dependencies before the services that need them, reversed when stopping.
//
// The graph comes from Compose's own label, so the result matches what
// `compose up` would do. Where the label is missing — anything created before
// Compose v2 — the order falls back to the service name, which is at least stable
// instead of depending on map iteration.
func orderMembers(members []stackMember, reverse bool) []stackMember {
	byService := map[string][]stackMember{}
	var services []string
	for _, member := range members {
		key := member.service
		if key == "" {
			// Not a Compose service, so nothing can depend on it. Its own container
			// name keeps it in the ordering as a node of its own.
			key = member.name
		}
		if _, seen := byService[key]; !seen {
			services = append(services, key)
		}
		byService[key] = append(byService[key], member)
	}
	sort.Strings(services)

	// Only dependencies that are actually here can be waited for: an entry naming a
	// service with no container in this project — scaled to zero, or removed — is
	// dropped rather than blocking everything downstream of it forever.
	waiting := map[string]map[string]bool{}
	for _, service := range services {
		deps := map[string]bool{}
		for _, member := range byService[service] {
			for _, dep := range member.deps {
				if dep != service && byService[dep] != nil {
					deps[dep] = true
				}
			}
		}
		waiting[service] = deps
	}

	ordered := make([]stackMember, 0, len(members))
	done := map[string]bool{}
	appendService := func(service string) {
		done[service] = true
		group := byService[service]
		sort.Slice(group, func(i, j int) bool { return group[i].name < group[j].name })
		ordered = append(ordered, group...)
	}

	for len(done) < len(services) {
		progress := false
		for _, service := range services {
			if done[service] || !depsSatisfied(waiting[service], done) {
				continue
			}
			appendService(service)
			progress = true
		}
		if !progress {
			// A dependency cycle, which Compose itself refuses — so this can only be
			// a hand-edited label. There is nothing left to order by, so the rest goes
			// in service-name order rather than looping forever.
			for _, service := range services {
				if !done[service] {
					appendService(service)
				}
			}
		}
	}

	if reverse {
		for left, right := 0, len(ordered)-1; left < right; left, right = left+1, right-1 {
			ordered[left], ordered[right] = ordered[right], ordered[left]
		}
	}
	return ordered
}

func depsSatisfied(deps, done map[string]bool) bool {
	for dep := range deps {
		if !done[dep] {
			return false
		}
	}
	return true
}
