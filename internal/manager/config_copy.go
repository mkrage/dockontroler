package manager

import (
	"fmt"
	"sort"
	"strings"

	"github.com/mkrage/dockontroler/internal/docker"
)

// networkAttachment is one network the replacement container must join.
type networkAttachment struct {
	Name   string
	Config *docker.EndpointConfig
}

// recreatePlan is the complete description of the replacement container,
// computed from an inspect result before anything is touched.
//
// Keeping this separate from the Engine calls in recreate.go is what makes the
// risky part testable: planRecreate is pure, so every awkward container shape
// can be checked against a golden file without a Docker daemon.
type recreatePlan struct {
	// Name is the original container name, without Docker's leading slash.
	Name string
	// Body is the POST /containers/create payload.
	Body docker.CreateBody
	// Primary is attached at create time. A create call reliably attaches only
	// one network, so the rest follow via /networks/{id}/connect.
	Primary *networkAttachment
	Extra   []networkAttachment
	// WasRunning records whether to start the replacement.
	WasRunning bool
	// Notes are things worth telling the user or the log, e.g. anonymous volumes
	// that were carried over.
	Notes []string
}

// planRecreate turns an inspect result into a plan for an equivalent container
// built from the current state of its image tag.
//
// The whole function exists because a container is bound to the image *id* it was
// created from. Restarting reuses that id, so picking up a rebuilt image means
// building a new container with the same configuration — and "the same
// configuration" is where the traps are. Each one is handled below.
func planRecreate(inspected *docker.ContainerInspect) (recreatePlan, error) {
	if inspected == nil || inspected.Config == nil {
		return recreatePlan{}, fmt.Errorf("%w: container has no recorded configuration", ErrUnsupported)
	}
	if ok, reason := recreatability(inspected); !ok {
		return recreatePlan{}, fmt.Errorf("%w: %s", ErrUnsupported, reason)
	}

	plan := recreatePlan{
		Name:       strings.TrimPrefix(inspected.Name, "/"),
		WasRunning: inspected.State.Running,
	}
	if plan.Name == "" {
		return recreatePlan{}, fmt.Errorf("%w: container has no name", ErrUnsupported)
	}

	// The create body is the container Config with HostConfig and
	// NetworkingConfig nested beside it. Start from a copy of the config we were
	// given, so every field we never look at survives untouched.
	body := cloneMap(inspected.Config)
	hostConfig := cloneMap(inspected.HostConfig)

	// Trap 1: the image reference.
	//
	// inspected.Image is the resolved image *id*; Config.Image is the reference
	// as written by whoever created the container ("nextcloud:29"). Using the id
	// would rebuild the identical container and quietly do nothing, which is the
	// entire bug this feature exists to avoid. recreatability() has already
	// rejected the cases where Config.Image is itself an id.
	body["Image"] = mapString(inspected.Config, "Image")

	// Trap 2: the hostname.
	//
	// Unless the user set one, Docker fills Hostname in with the container's own
	// short id. Copying that would give the replacement a hostname pointing at a
	// container that no longer exists.
	if mapString(inspected.Config, "Hostname") == shortID(inspected.ID) {
		delete(body, "Hostname")
	}

	// Trap 3: anonymous volumes.
	notes := carryAnonymousVolumes(inspected, body, hostConfig)
	plan.Notes = append(plan.Notes, notes...)

	// Trap 4: networks.
	primary, extra := planNetworks(inspected, hostConfig)
	plan.Primary, plan.Extra = primary, extra

	body["HostConfig"] = hostConfig
	if primary != nil {
		body["NetworkingConfig"] = docker.NetworkingConfig{
			EndpointsConfig: map[string]*docker.EndpointConfig{
				primary.Name: primary.Config,
			},
		}
	}

	// Labels are copied verbatim as part of Config, including
	// com.docker.compose.config-hash. That matters: Compose compares that hash
	// to decide whether a container still matches its yaml, so preserving it
	// means a later `docker compose up -d` sees no drift and leaves the
	// container alone.

	if strings.Contains(mapString(inspected.Config, "Image"), "@sha256:") {
		plan.Notes = append(plan.Notes,
			"image is digest-pinned, so the container is rebuilt but the image stays the same")
	}

	plan.Body = body
	return plan, nil
}

// carryAnonymousVolumes makes the replacement reuse the previous container's
// anonymous volumes instead of getting fresh empty ones.
//
// This is the trap that eats databases. An image with a VOLUME instruction, or a
// `docker run -v /var/lib/postgresql/data`, produces a volume with a generated
// name that appears nowhere in HostConfig — only in the resolved Mounts list. A
// recreate that copies HostConfig alone therefore creates a brand-new empty
// volume and leaves the old data orphaned under a random name.
//
// The fix is to turn each such volume into an explicit bind, and to drop the
// matching entry from Config.Volumes so Docker does not generate a new one for
// the same path.
func carryAnonymousVolumes(inspected *docker.ContainerInspect, body, hostConfig map[string]any) []string {
	covered := coveredDestinations(hostConfig)

	binds := mapStrings(hostConfig, "Binds")
	declared := mapSub(body, "Volumes")

	var carried []string
	for _, mount := range inspected.Mounts {
		if mount.Type != "volume" || mount.Name == "" || mount.Destination == "" {
			continue
		}
		if covered[mount.Destination] {
			// Already named in HostConfig, so it is a normal named volume and
			// gets copied along with HostConfig anyway.
			continue
		}

		binds = append(binds, formatBind(mount))
		carried = append(carried, mount.Destination)

		// Without this, Docker sees the destination in Config.Volumes and
		// creates a second, empty anonymous volume for it.
		if declared != nil {
			delete(declared, mount.Destination)
		}
	}

	if len(carried) == 0 {
		return nil
	}

	sort.Strings(binds)
	hostConfig["Binds"] = binds
	if declared != nil && len(declared) == 0 {
		delete(body, "Volumes")
	}

	sort.Strings(carried)
	return []string{fmt.Sprintf("kept %d anonymous volume(s): %s",
		len(carried), strings.Join(carried, ", "))}
}

// coveredDestinations lists every container path HostConfig already mounts,
// through either the older Binds strings or the newer Mounts objects.
func coveredDestinations(hostConfig map[string]any) map[string]bool {
	covered := map[string]bool{}

	for _, bind := range mapStrings(hostConfig, "Binds") {
		// "source:destination[:options]" — Linux paths and volume names contain
		// no colons, so the second field is the destination.
		if parts := strings.Split(bind, ":"); len(parts) >= 2 {
			covered[parts[1]] = true
		}
	}

	if mounts, ok := hostConfig["Mounts"].([]any); ok {
		for _, entry := range mounts {
			if mount, ok := entry.(map[string]any); ok {
				if target := mapString(mount, "Target"); target != "" {
					covered[target] = true
				}
			}
		}
	}
	return covered
}

// formatBind renders a mount as a bind string, preserving its access mode.
func formatBind(mount docker.MountPoint) string {
	bind := mount.Name + ":" + mount.Destination
	switch {
	case mount.Mode != "":
		// Docker records things like "ro", "rw" or SELinux labels ("z", "rw,z")
		// here; passing the recorded value through keeps them all.
		return bind + ":" + mount.Mode
	case !mount.RW:
		return bind + ":ro"
	default:
		return bind
	}
}

// planNetworks decides which network the replacement joins at create time and
// which ones it joins afterwards.
//
// Only configuration is carried over. Inspect also reports the addresses and MAC
// Docker assigned at start; copying those into a create call either fails or
// pins values that were never meant to be fixed. Statically configured addresses
// live in IPAMConfig and are preserved.
func planNetworks(inspected *docker.ContainerInspect, hostConfig map[string]any) (*networkAttachment, []networkAttachment) {
	mode := mapString(hostConfig, "NetworkMode")

	// host and none are expressed entirely through HostConfig.NetworkMode; the
	// matching entry in Networks is bookkeeping and must not be sent back.
	if mode == "host" || mode == "none" {
		return nil, nil
	}

	names := make([]string, 0, len(inspected.NetworkSettings.Networks))
	for name := range inspected.NetworkSettings.Networks {
		names = append(names, name)
	}
	// Sorted so the plan is deterministic — map iteration order is not, and a
	// golden-file test would flap.
	sort.Strings(names)
	if len(names) == 0 {
		return nil, nil
	}

	// The network named by NetworkMode is the container's primary one, so it is
	// the natural choice to attach at create time.
	primaryIndex := 0
	for i, name := range names {
		if name == mode {
			primaryIndex = i
			break
		}
	}

	attachment := func(name string) networkAttachment {
		settings := inspected.NetworkSettings.Networks[name]
		config := settings.Config()
		config.Aliases = cleanAliases(config.Aliases, inspected.ID)
		return networkAttachment{Name: name, Config: config}
	}

	primary := attachment(names[primaryIndex])
	var extra []networkAttachment
	for i, name := range names {
		if i != primaryIndex {
			extra = append(extra, attachment(name))
		}
	}
	return &primary, extra
}

// cleanAliases drops the alias Docker generates from the container's own short
// id. Carrying it over would leave the replacement answering to a name derived
// from the container it replaced.
func cleanAliases(aliases []string, containerID string) []string {
	short := shortID(containerID)
	kept := make([]string, 0, len(aliases))
	for _, alias := range aliases {
		if alias == short || alias == containerID {
			continue
		}
		kept = append(kept, alias)
	}
	if len(kept) == 0 {
		return nil
	}
	return kept
}
