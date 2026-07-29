package manager

import (
	"encoding/json"
	"errors"
	"flag"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"github.com/mkrage/dockontroler/internal/docker"
)

// Run `go test ./internal/manager -update` to rewrite the golden files after an
// intentional change to the create payload. Always read the resulting diff: this
// file is the safety net that catches configuration being dropped on recreate, so
// an unexplained change to it is a bug report, not a formatting nit.
var updateGolden = flag.Bool("update", false, "rewrite golden files")

// TestPlanRecreateGolden pins the entire create payload for a realistic Compose
// container.
//
// The targeted tests below each cover one trap. This one covers the thing they
// cannot: a field silently disappearing. HostConfig has around eighty keys, and
// the value here is that CapAdd, Sysctls, LogConfig, ShmSize and friends have to
// still be present, byte for byte, without anybody having remembered to assert on
// them individually.
func TestPlanRecreateGolden(t *testing.T) {
	inspected := loadInspect(t, "inspect_compose.json")

	plan, err := planRecreate(inspected)
	if err != nil {
		t.Fatalf("planRecreate: %v", err)
	}

	encoded, err := json.MarshalIndent(plan.Body, "", "\t")
	if err != nil {
		t.Fatalf("marshal plan body: %v", err)
	}
	encoded = append(encoded, '\n')

	goldenPath := filepath.Join("testdata", "plan_compose.golden.json")
	if *updateGolden {
		if err := os.WriteFile(goldenPath, encoded, 0o644); err != nil {
			t.Fatalf("write golden: %v", err)
		}
		t.Log("golden file rewritten")
		return
	}

	want, err := os.ReadFile(goldenPath)
	if err != nil {
		t.Fatalf("read golden: %v", err)
	}
	if string(encoded) != string(want) {
		t.Errorf("create payload differs from %s\n\n--- got ---\n%s\n--- want ---\n%s",
			goldenPath, encoded, want)
	}
}

func TestPlanRecreateGoldenPlanShape(t *testing.T) {
	inspected := loadInspect(t, "inspect_compose.json")

	plan, err := planRecreate(inspected)
	if err != nil {
		t.Fatalf("planRecreate: %v", err)
	}

	if plan.Name != "blog-web-1" {
		t.Errorf("Name = %q, want %q", plan.Name, "blog-web-1")
	}
	if !plan.WasRunning {
		t.Error("WasRunning = false, want true — the replacement must be started")
	}
	// NetworkMode names blog_default, so that is the one attached at create time.
	if plan.Primary == nil || plan.Primary.Name != "blog_default" {
		t.Fatalf("Primary = %+v, want blog_default", plan.Primary)
	}
	if len(plan.Extra) != 1 || plan.Extra[0].Name != "proxy" {
		t.Fatalf("Extra = %+v, want exactly [proxy]", plan.Extra)
	}
	// The static address on the proxy network must survive, or the container
	// comes back on a different IP.
	if ipam := plan.Extra[0].Config.IPAMConfig; ipam == nil || ipam.IPv4Address != "172.20.0.5" {
		t.Errorf("proxy IPAMConfig = %+v, want IPv4Address 172.20.0.5", ipam)
	}
	if !hasNote(plan.Notes, "anonymous volume") {
		t.Errorf("Notes = %q, want a note about the carried anonymous volume", plan.Notes)
	}
}

// TestPlanRecreateUsesTagNotImageID guards the bug the whole feature exists for:
// recreating from the resolved image id would rebuild an identical container and
// silently not pick up the new build.
func TestPlanRecreateUsesTagNotImageID(t *testing.T) {
	inspected := simpleInspect()
	inspected.Image = "sha256:" + strings.Repeat("f", 64)
	inspected.Config["Image"] = "myapp:latest"

	plan := mustPlan(t, inspected)

	if got := plan.Body["Image"]; got != "myapp:latest" {
		t.Errorf("Image = %v, want myapp:latest (the tag, not the resolved id)", got)
	}
}

func TestPlanRecreateHostnameHandling(t *testing.T) {
	t.Run("drops the auto-generated hostname", func(t *testing.T) {
		inspected := simpleInspect()
		// Docker fills this in with the container's own short id.
		inspected.Config["Hostname"] = shortID(inspected.ID)

		plan := mustPlan(t, inspected)

		if _, present := plan.Body["Hostname"]; present {
			t.Errorf("Hostname = %v, want it removed so the replacement gets its own",
				plan.Body["Hostname"])
		}
	})

	t.Run("keeps a hostname the user chose", func(t *testing.T) {
		inspected := simpleInspect()
		inspected.Config["Hostname"] = "blog.example.internal"

		plan := mustPlan(t, inspected)

		if got := plan.Body["Hostname"]; got != "blog.example.internal" {
			t.Errorf("Hostname = %v, want the configured value to survive", got)
		}
	})
}

// TestPlanRecreateCarriesAnonymousVolumes covers the trap that eats databases: an
// anonymous volume appears only in the resolved mount list, so a recreate that
// copies HostConfig alone gets a fresh empty volume and orphans the data.
func TestPlanRecreateCarriesAnonymousVolumes(t *testing.T) {
	inspected := simpleInspect()
	inspected.Config["Volumes"] = map[string]any{"/var/lib/postgresql/data": map[string]any{}}
	inspected.Mounts = []docker.MountPoint{{
		Type:        "volume",
		Name:        "e7c1a0deadbeef",
		Destination: "/var/lib/postgresql/data",
		RW:          true,
	}}

	plan := mustPlan(t, inspected)

	hostConfig := plan.Body["HostConfig"].(map[string]any)
	binds := mapStrings(hostConfig, "Binds")
	want := "e7c1a0deadbeef:/var/lib/postgresql/data"
	if !contains(binds, want) {
		t.Errorf("Binds = %q, want it to contain %q", binds, want)
	}

	// Leaving the destination in Config.Volumes would make Docker create a
	// second, empty anonymous volume for the same path.
	if _, present := plan.Body["Volumes"]; present {
		t.Errorf("Volumes = %v, want the carried destination removed", plan.Body["Volumes"])
	}
}

func TestPlanRecreateDoesNotDuplicateNamedVolumes(t *testing.T) {
	inspected := simpleInspect()
	inspected.HostConfig["Binds"] = []any{"blog_data:/data"}
	inspected.Mounts = []docker.MountPoint{{
		Type:        "volume",
		Name:        "blog_data",
		Destination: "/data",
		RW:          true,
	}}

	plan := mustPlan(t, inspected)

	binds := mapStrings(plan.Body["HostConfig"].(map[string]any), "Binds")
	if len(binds) != 1 || binds[0] != "blog_data:/data" {
		t.Errorf("Binds = %q, want the single existing bind untouched", binds)
	}
}

// TestPlanRecreateCarriesMountStyleAnonymousVolumes covers the same trap reached
// through the long mount syntax: `--mount type=volume,target=/data` records a
// HostConfig.Mounts entry with no source, so the destination looks configured
// while the generated volume name lives only in the resolved mount list.
func TestPlanRecreateCarriesMountStyleAnonymousVolumes(t *testing.T) {
	inspected := simpleInspect()
	inspected.HostConfig["Mounts"] = []any{
		map[string]any{"Type": "volume", "Target": "/var/lib/postgresql/data"},
		map[string]any{"Type": "volume", "Source": "assets", "Target": "/srv/assets"},
	}
	inspected.Mounts = []docker.MountPoint{
		{
			Type:        "volume",
			Name:        "f1e2d3c4b5a6",
			Destination: "/var/lib/postgresql/data",
			RW:          true,
		},
		{Type: "volume", Name: "assets", Destination: "/srv/assets", RW: true},
	}

	plan := mustPlan(t, inspected)

	hostConfig := plan.Body["HostConfig"].(map[string]any)
	binds := mapStrings(hostConfig, "Binds")
	if want := "f1e2d3c4b5a6:/var/lib/postgresql/data"; !contains(binds, want) {
		t.Errorf("Binds = %q, want it to contain %q — the data would be left orphaned", binds, want)
	}
	// The named volume needs no bind: it is copied as part of HostConfig.Mounts.
	if contains(binds, "assets:/srv/assets") {
		t.Errorf("Binds = %q, want the named mount left to HostConfig", binds)
	}

	// The source-less spec has to go, or the Engine refuses the create with a
	// duplicate mount point for that target.
	mounts, ok := hostConfig["Mounts"].([]any)
	if !ok {
		t.Fatalf("Mounts = %T, want the named mount still there", hostConfig["Mounts"])
	}
	if len(mounts) != 1 {
		t.Fatalf("Mounts = %v, want only the named mount to survive", mounts)
	}
	if got := mapString(mounts[0].(map[string]any), "Target"); got != "/srv/assets" {
		t.Errorf("surviving mount targets %q, want /srv/assets", got)
	}

	if !hasNote(plan.Notes, "/var/lib/postgresql/data") {
		t.Errorf("Notes = %q, want the carried volume mentioned", plan.Notes)
	}
}

// TestPlanRecreateRefusesAutoRemove: a --rm container is deleted by the daemon the
// moment it stops, and recreate stops the original before renaming it out of the
// way — so there would be nothing left to roll back to. Refusing up front is the
// only way to keep the "worst case is nothing changed" promise.
func TestPlanRecreateRefusesAutoRemove(t *testing.T) {
	inspected := simpleInspect()
	inspected.HostConfig["AutoRemove"] = true

	if _, err := planRecreate(inspected); !errors.Is(err, ErrUnsupported) {
		t.Fatalf("err = %v, want ErrUnsupported: stopping it would delete it for good", err)
	}

	// The UI and the bot both hide the button based on this, so the refusal has to
	// be visible before anybody clicks.
	canRecreate, note := recreatability(inspected)
	if canRecreate {
		t.Error("recreatability said yes, so the button would still be offered")
	}
	if note == "" {
		t.Error("no reason given, leaving the disabled button unexplained")
	}
}

func TestPlanRecreateKeepsReadOnlyMountMode(t *testing.T) {
	inspected := simpleInspect()
	inspected.Mounts = []docker.MountPoint{{
		Type:        "volume",
		Name:        "reference",
		Destination: "/srv/reference",
		RW:          false,
	}}

	plan := mustPlan(t, inspected)

	binds := mapStrings(plan.Body["HostConfig"].(map[string]any), "Binds")
	if !contains(binds, "reference:/srv/reference:ro") {
		t.Errorf("Binds = %q, want the read-only flag preserved", binds)
	}
}

func TestPlanRecreateStripsRuntimeNetworkState(t *testing.T) {
	inspected := simpleInspect()
	inspected.HostConfig["NetworkMode"] = "app_net"
	inspected.NetworkSettings.Networks = map[string]docker.EndpointSettings{
		"app_net": {
			Aliases:    []string{"api", shortID(inspected.ID)},
			IPAddress:  "172.19.0.7",
			MacAddress: "02:42:ac:13:00:07",
			NetworkID:  hexID("netid"),
		},
	}

	plan := mustPlan(t, inspected)

	// The addresses Docker assigned at start must not be sent back: they were
	// never configuration, and pinning them would be wrong on the next start.
	encoded, err := json.Marshal(plan.Body["NetworkingConfig"])
	if err != nil {
		t.Fatalf("marshal NetworkingConfig: %v", err)
	}
	for _, leaked := range []string{"172.19.0.7", "02:42:ac:13:00:07", "NetworkID"} {
		if strings.Contains(string(encoded), leaked) {
			t.Errorf("NetworkingConfig leaks runtime state %q: %s", leaked, encoded)
		}
	}

	// The alias Docker derives from the container's own short id would otherwise
	// leave the replacement answering to the id of the container it replaced.
	aliases := plan.Primary.Config.Aliases
	if !reflect.DeepEqual(aliases, []string{"api"}) {
		t.Errorf("Aliases = %q, want only [api]", aliases)
	}
}

func TestPlanRecreateNetworkModes(t *testing.T) {
	t.Run("host mode sends no endpoints", func(t *testing.T) {
		inspected := simpleInspect()
		inspected.HostConfig["NetworkMode"] = "host"
		inspected.NetworkSettings.Networks = map[string]docker.EndpointSettings{"host": {}}

		plan := mustPlan(t, inspected)

		if plan.Primary != nil || len(plan.Extra) != 0 {
			t.Errorf("Primary=%+v Extra=%+v, want none — HostConfig.NetworkMode covers host mode",
				plan.Primary, plan.Extra)
		}
		if _, present := plan.Body["NetworkingConfig"]; present {
			t.Error("NetworkingConfig must be absent in host mode")
		}
	})

	t.Run("shared namespace is refused", func(t *testing.T) {
		inspected := simpleInspect()
		inspected.HostConfig["NetworkMode"] = "container:" + hexID("other")

		_, err := planRecreate(inspected)

		if !errors.Is(err, ErrUnsupported) {
			t.Fatalf("err = %v, want ErrUnsupported: the reference cannot survive a replacement", err)
		}
	})
}

func TestPlanRecreateRefusesBareImageID(t *testing.T) {
	for name, reference := range map[string]string{
		"sha256 prefixed": "sha256:" + strings.Repeat("a", 64),
		"bare hex id":     strings.Repeat("b", 64),
	} {
		t.Run(name, func(t *testing.T) {
			inspected := simpleInspect()
			inspected.Config["Image"] = reference

			_, err := planRecreate(inspected)

			if !errors.Is(err, ErrUnsupported) {
				t.Fatalf("err = %v, want ErrUnsupported: there is no tag to re-resolve", err)
			}
		})
	}
}

func TestPlanRecreateNotesDigestPinnedImage(t *testing.T) {
	inspected := simpleInspect()
	inspected.Config["Image"] = "postgres@sha256:" + strings.Repeat("c", 64)

	plan := mustPlan(t, inspected)

	if !hasNote(plan.Notes, "digest-pinned") {
		t.Errorf("Notes = %q, want a note that the image will not change", plan.Notes)
	}
}

// TestPlanRecreatePreservesUnknownFields is the reason Config and HostConfig are
// kept as untyped maps. A struct would silently drop anything not modelled, which
// for HostConfig means somebody's GPU passthrough or seccomp profile.
func TestPlanRecreatePreservesUnknownFields(t *testing.T) {
	inspected := simpleInspect()
	inspected.HostConfig["DeviceRequests"] = []any{
		map[string]any{"Driver": "nvidia", "Count": float64(-1)},
	}
	inspected.HostConfig["SomethingDockerAddedLater"] = "keep me"
	inspected.Config["AlsoNew"] = float64(42)

	plan := mustPlan(t, inspected)

	hostConfig := plan.Body["HostConfig"].(map[string]any)
	if _, present := hostConfig["DeviceRequests"]; !present {
		t.Error("DeviceRequests was dropped — GPU passthrough would be lost")
	}
	if got := hostConfig["SomethingDockerAddedLater"]; got != "keep me" {
		t.Errorf("unknown HostConfig key = %v, want it copied verbatim", got)
	}
	if got := plan.Body["AlsoNew"]; got != float64(42) {
		t.Errorf("unknown Config key = %v, want it copied verbatim", got)
	}
}

func TestPlanRecreateKeepsComposeConfigHash(t *testing.T) {
	inspected := loadInspect(t, "inspect_compose.json")

	plan := mustPlan(t, inspected)

	labels, ok := plan.Body["Labels"].(map[string]any)
	if !ok {
		t.Fatalf("Labels = %T, want a map", plan.Body["Labels"])
	}
	// Compose compares this hash to decide whether a container still matches its
	// yaml. Changing it would make the next `docker compose up -d` replace the
	// container all over again.
	if got := labels[docker.LabelComposeConfigHash]; got != "9f8e7d6c5b4a" {
		t.Errorf("%s = %v, want it preserved verbatim", docker.LabelComposeConfigHash, got)
	}
}

func TestPlanRecreateStoppedContainerStaysStopped(t *testing.T) {
	inspected := simpleInspect()
	inspected.State.Running = false

	plan := mustPlan(t, inspected)

	if plan.WasRunning {
		t.Error("WasRunning = true, want false — recreating a stopped container must not start it")
	}
}

// ---------- helpers ----------

// simpleInspect is a minimal but valid container: one image tag, bridge network,
// running.
func simpleInspect() *docker.ContainerInspect {
	inspected := &docker.ContainerInspect{
		ID:   hexID("abc123def456"),
		Name: "/app",
		Config: map[string]any{
			"Image": "myapp:latest",
		},
		HostConfig: map[string]any{
			"NetworkMode": "bridge",
		},
	}
	inspected.State.Running = true
	inspected.NetworkSettings.Networks = map[string]docker.EndpointSettings{}
	return inspected
}

func loadInspect(t *testing.T, name string) *docker.ContainerInspect {
	t.Helper()
	raw, err := os.ReadFile(filepath.Join("testdata", name))
	if err != nil {
		t.Fatalf("read %s: %v", name, err)
	}
	var inspected docker.ContainerInspect
	if err := json.Unmarshal(raw, &inspected); err != nil {
		t.Fatalf("decode %s: %v", name, err)
	}
	return &inspected
}

func mustPlan(t *testing.T, inspected *docker.ContainerInspect) recreatePlan {
	t.Helper()
	plan, err := planRecreate(inspected)
	if err != nil {
		t.Fatalf("planRecreate: %v", err)
	}
	return plan
}

func hasNote(notes []string, substring string) bool {
	for _, note := range notes {
		if strings.Contains(note, substring) {
			return true
		}
	}
	return false
}

func contains(values []string, want string) bool {
	for _, value := range values {
		if value == want {
			return true
		}
	}
	return false
}
