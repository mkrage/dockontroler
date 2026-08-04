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

	// Ports are the ways this container can be reached from outside, sorted by
	// host port. Empty for a container that publishes nothing — a database behind
	// a Compose network, say, which is reachable only by its neighbours.
	Ports []PortMapping

	// Policy is the restart policy, normalised so that a container created
	// before policies existed reads as "no" rather than "".
	Policy string

	Running bool
	Paused  bool

	// IsSelf marks docKontroler's own container. Stop, restart and recreate are
	// refused for it: the process would kill itself mid-request.
	IsSelf bool

	ComposeProject string
	ComposeService string
	// ComposeFiles are the compose files the container was created from, as Compose
	// itself recorded them. Empty for anything not created by Compose.
	ComposeFiles []string

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

// PolicyFile is the compose file to edit so that a restart policy set here survives
// the container being rebuilt from its yaml. Empty when Compose was not involved.
//
// The last of the files, because later ones override earlier ones: that is where a
// restart: line ends up winning.
func (c Container) PolicyFile() string {
	if len(c.ComposeFiles) == 0 {
		return ""
	}
	return c.ComposeFiles[len(c.ComposeFiles)-1]
}

// Active reports whether the container is up, or on its way there.
//
// Restarting counts as active on purpose: a container in a crash loop is not
// resting, it is the one thing on the page that wants attention. What is left is
// everything that is not going to do anything until somebody starts it.
func (c Container) Active() bool {
	switch c.State {
	case docker.StateRunning, docker.StatePaused, docker.StateRestarting:
		return true
	default:
		return false
	}
}

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

	container.Ports = portsOf(summary, inspected)

	container.ComposeProject = labels[docker.LabelComposeProject]
	container.ComposeService = labels[docker.LabelComposeService]
	container.ComposeFiles = splitComposeFiles(labels[docker.LabelComposeConfigFiles])

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

	// A --rm container is deleted by the daemon, along with its anonymous volumes,
	// the moment it stops. Recreate stops the original before renaming it out of
	// the way, so by the time anything could go wrong there is nothing left to
	// restore — the one case where this operation would destroy data instead of
	// changing nothing. Refuse it rather than reordering: auto-removal is
	// triggered by the stop, whatever the container is called.
	if mapBool(inspected.HostConfig, "AutoRemove") {
		return false, "removed automatically when it stops, so it could not be restored"
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

// splitComposeFiles reads the comma-separated list Compose records. Paths are kept
// verbatim: they are meaningful to whoever ran Compose, and rewriting them to
// something that looks host-like would only invent a location.
func splitComposeFiles(raw string) []string {
	if raw == "" {
		return nil
	}
	var files []string
	for _, path := range strings.Split(raw, ",") {
		if path = strings.TrimSpace(path); path != "" {
			files = append(files, path)
		}
	}
	return files
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
