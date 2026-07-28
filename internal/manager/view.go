package manager

import (
	"sort"
	"strings"
	"time"

	"github.com/mkrage/dockontroler/internal/docker"
)

// Container is the display model handed to the web templates and the Telegram
// bot. It is a flattened, already-decided view: anything the UI would otherwise
// have to work out (which policy button is active, whether recreate is possible)
// is resolved here.
type Container struct {
	ID      string // full 64-character id
	ShortID string // first 12 characters, what Docker itself shows
	Name    string // without Docker's leading slash

	// Image is the reference the container was created from, as written in its
	// config — normally a tag such as "nextcloud:29". This is what a recreate
	// re-resolves, which is why it is preferred over the image id.
	Image string

	State  string // docker.State* constant
	Status string // the Engine's own wording, e.g. "Up 3 hours"

	// Policy is the restart policy, normalised so that a container created
	// before policies existed reads as "no" rather than "".
	Policy string

	Running bool
	Paused  bool

	// IsSelf marks Dockontroler's own container. Stop, restart and recreate are
	// refused for it: the process would kill itself mid-request.
	IsSelf bool

	ComposeProject string
	ComposeService string

	// CanRecreate is false when recreating cannot work, with Note explaining
	// why. Note is also set for cases that work but come with a caveat.
	CanRecreate bool
	Note        string

	// Incomplete means the container's details could not be inspected, so
	// Policy and Image may be missing. The row is still shown — a partial row
	// beats a container silently disappearing from the overview.
	Incomplete bool
}

// PolicyIs reports whether the container currently uses the given policy.
// Templates call this to highlight the active button.
func (c Container) PolicyIs(policy string) bool { return c.Policy == policy }

// StateClass is a coarse bucket for CSS and for the bot's status icon.
func (c Container) StateClass() string {
	switch c.State {
	case docker.StateRunning:
		return "running"
	case docker.StatePaused, docker.StateRestarting:
		return "transitional"
	case docker.StateDead:
		return "error"
	default:
		return "stopped"
	}
}

// StateIcon is the emoji the Telegram bot puts in front of a container name.
func (c Container) StateIcon() string {
	switch c.StateClass() {
	case "running":
		return "🟢"
	case "transitional":
		return "🟡"
	case "error":
		return "🔴"
	default:
		return "⚪"
	}
}

// Group is a set of containers belonging to one Compose project. Project is
// empty for containers that were not created by Compose.
type Group struct {
	Project    string
	Containers []Container
}

// Overview is everything the container list page renders.
type Overview struct {
	Groups  []Group
	Total   int
	Running int
	// Warning is set when the list is usable but incomplete, e.g. some
	// containers could not be inspected.
	Warning     string
	GeneratedAt time.Time
}

// newContainer flattens a list entry plus its inspect result into the display
// model. inspected may be nil when the inspect call failed.
func newContainer(summary docker.ContainerSummary, inspected *docker.ContainerInspect, selfID string) Container {
	container := Container{
		ID:      summary.ID,
		ShortID: shortID(summary.ID),
		Name:    strings.TrimPrefix(firstName(summary.Names), "/"),
		Image:   summary.Image,
		State:   summary.State,
		Status:  summary.Status,
		Running: summary.State == docker.StateRunning,
		Paused:  summary.State == docker.StatePaused,
		IsSelf:  selfID != "" && summary.ID == selfID,
		Policy:  docker.PolicyNo,
	}

	labels := summary.Labels
	if inspected == nil {
		container.Incomplete = true
		container.Note = "details unavailable"
		container.CanRecreate = false
	} else {
		if configured := mapString(inspected.Config, "Image"); configured != "" {
			container.Image = configured
		}
		if inspectedLabels := mapLabels(inspected.Config); len(inspectedLabels) > 0 {
			labels = inspectedLabels
		}
		if policy := mapString(mapSub(inspected.HostConfig, "RestartPolicy"), "Name"); policy != "" {
			container.Policy = policy
		}
		container.CanRecreate, container.Note = recreatability(inspected)
	}

	container.ComposeProject = labels[docker.LabelComposeProject]
	container.ComposeService = labels[docker.LabelComposeService]

	if container.IsSelf {
		container.CanRecreate = false
		container.Note = "this is dockontroler itself"
	}
	return container
}

// recreatability decides whether a recreate can work for this container, and
// returns a short note for the UI when there is something worth saying.
func recreatability(inspected *docker.ContainerInspect) (bool, string) {
	reference := mapString(inspected.Config, "Image")
	switch {
	case reference == "":
		return false, "no image reference recorded"
	case isBareImageID(reference):
		// Nothing to re-resolve: the container points straight at an image id,
		// so a recreate would produce a byte-identical container.
		return false, "pinned to an image id, not a tag"
	}

	// container:<id> network mode makes the container share another container's
	// network namespace. That reference cannot survive a replacement.
	if mode := mapString(inspected.HostConfig, "NetworkMode"); strings.HasPrefix(mode, "container:") {
		return false, "shares another container's network namespace"
	}

	if strings.Contains(reference, "@sha256:") {
		// Works, but the digest pins the exact image, so a rebuild will not be
		// picked up. Worth saying out loud rather than looking broken.
		return true, "digest-pinned, so the image will not change"
	}
	return true, ""
}

// isBareImageID reports whether reference is an image id rather than a name.
func isBareImageID(reference string) bool {
	if strings.HasPrefix(reference, "sha256:") {
		return true
	}
	if len(reference) != 64 {
		return false
	}
	for _, r := range reference {
		if !(r >= '0' && r <= '9' || r >= 'a' && r <= 'f') {
			return false
		}
	}
	return true
}

func firstName(names []string) string {
	if len(names) == 0 {
		return ""
	}
	return names[0]
}

func shortID(id string) string {
	if len(id) > 12 {
		return id[:12]
	}
	return id
}

// buildOverview groups containers by Compose project and sorts everything into a
// stable order, so the auto-refresh never reshuffles rows under the cursor.
func buildOverview(containers []Container) Overview {
	overview := Overview{Total: len(containers)}

	byProject := map[string][]Container{}
	for _, container := range containers {
		if container.Running {
			overview.Running++
		}
		byProject[container.ComposeProject] = append(byProject[container.ComposeProject], container)
	}

	for project, members := range byProject {
		sort.Slice(members, func(i, j int) bool {
			return containerLess(members[i], members[j])
		})
		overview.Groups = append(overview.Groups, Group{Project: project, Containers: members})
	}

	sort.Slice(overview.Groups, func(i, j int) bool {
		left, right := overview.Groups[i].Project, overview.Groups[j].Project
		// Containers without a Compose project sort last, under their own
		// heading, so real projects stay at the top of the page.
		if (left == "") != (right == "") {
			return right == ""
		}
		return strings.ToLower(left) < strings.ToLower(right)
	})

	overview.GeneratedAt = time.Now()
	return overview
}

// containerLess orders containers within a project: by Compose service name
// where available, otherwise by container name.
func containerLess(a, b Container) bool {
	aKey := a.ComposeService
	if aKey == "" {
		aKey = a.Name
	}
	bKey := b.ComposeService
	if bKey == "" {
		bKey = b.Name
	}
	if !strings.EqualFold(aKey, bKey) {
		return strings.ToLower(aKey) < strings.ToLower(bKey)
	}
	// Same service, e.g. a scaled Compose service: fall back to the name, then
	// the id, so the order never depends on map iteration.
	if !strings.EqualFold(a.Name, b.Name) {
		return strings.ToLower(a.Name) < strings.ToLower(b.Name)
	}
	return a.ID < b.ID
}
