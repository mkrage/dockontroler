package manager

import (
	"reflect"
	"testing"

	"github.com/mkrage/dockontroler/internal/docker"
)

// TestPortsOfRunningContainer: the Engine reports one published port twice, once
// per address family, and lists exposed-but-unpublished ports alongside the real
// mappings. Both would be misleading in the overview.
func TestPortsOfRunningContainer(t *testing.T) {
	summary := docker.ContainerSummary{
		State: docker.StateRunning,
		Ports: []docker.Port{
			{IP: "::", PrivatePort: 80, PublicPort: 8080, Type: "tcp"},
			{IP: "0.0.0.0", PrivatePort: 80, PublicPort: 8080, Type: "tcp"},
			{PrivatePort: 9000, Type: "tcp"}, // exposed by the image, not published
			{IP: "0.0.0.0", PrivatePort: 53, PublicPort: 5353, Type: "udp"},
		},
	}

	got := portsOf(summary, nil)
	want := []PortMapping{
		{HostPort: 5353, ContainerPort: 53, Protocol: "udp"},
		{HostPort: 8080, ContainerPort: 80, Protocol: "tcp"},
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("ports = %+v, want %+v", got, want)
	}
}

// TestPortsOfStoppedContainer: a stopped container's list entry carries no ports
// at all, so the configured bindings are the only place left to read them from.
func TestPortsOfStoppedContainer(t *testing.T) {
	inspected := &docker.ContainerInspect{
		HostConfig: map[string]any{
			"NetworkMode": "bridge",
			"PortBindings": map[string]any{
				"80/tcp": []any{map[string]any{"HostIp": "", "HostPort": "8080"}},
				// "any free port, decided at start time": there is no number yet.
				"443/tcp": []any{map[string]any{"HostIp": "", "HostPort": ""}},
			},
		},
	}

	got := portsOf(docker.ContainerSummary{State: docker.StateExited}, inspected)
	want := []PortMapping{{HostPort: 8080, ContainerPort: 80, Protocol: "tcp"}}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("ports = %+v, want %+v", got, want)
	}
}

// TestPortsOnHostNetwork: nothing is published for network_mode: host, so without
// the fallback to the image's exposed ports such a container would look as if it
// had no ports — while in fact it holds the host's own.
func TestPortsOnHostNetwork(t *testing.T) {
	inspected := &docker.ContainerInspect{
		Config: map[string]any{
			"ExposedPorts": map[string]any{"8123/tcp": map[string]any{}},
		},
		HostConfig: map[string]any{"NetworkMode": "host"},
	}

	got := portsOf(docker.ContainerSummary{State: docker.StateRunning}, inspected)
	want := []PortMapping{{ContainerPort: 8123, Protocol: "tcp", HostNetwork: true}}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("ports = %+v, want %+v", got, want)
	}
	if label := got[0].Label(); label != "8123" {
		t.Errorf("label = %q, want the bare port: nothing is translated", label)
	}
	if url := got[0].URL("nas.local"); url != "http://nas.local:8123" {
		t.Errorf("url = %q", url)
	}
}

func TestPortLabels(t *testing.T) {
	cases := []struct {
		name    string
		mapping PortMapping
		want    string
	}{
		{
			name:    "translated",
			mapping: PortMapping{HostPort: 8080, ContainerPort: 80, Protocol: "tcp"},
			want:    "8080 → 80",
		},
		{
			// Nothing to explain when both sides agree, which is the common case.
			name:    "same on both sides",
			mapping: PortMapping{HostPort: 3625, ContainerPort: 3625, Protocol: "tcp"},
			want:    "3625",
		},
		{
			name:    "protocol worth naming",
			mapping: PortMapping{HostPort: 5353, ContainerPort: 53, Protocol: "udp"},
			want:    "5353 → 53/udp",
		},
	}
	for _, testCase := range cases {
		t.Run(testCase.name, func(t *testing.T) {
			if got := testCase.mapping.Label(); got != testCase.want {
				t.Errorf("Label() = %q, want %q", got, testCase.want)
			}
		})
	}
}

func TestPortURL(t *testing.T) {
	cases := []struct {
		name    string
		mapping PortMapping
		host    string
		want    string
	}{
		{
			// The wildcard binding is the whole reason the browser's own address is
			// needed: 0.0.0.0 is not something anyone can open.
			name:    "wildcard uses the address the browser came from",
			mapping: PortMapping{HostPort: 8080, ContainerPort: 80, Protocol: "tcp"},
			host:    "192.168.1.10",
			want:    "http://192.168.1.10:8080",
		},
		{
			// A container bound to one address answers on that address only, so it
			// overrides whatever the browser used.
			name:    "a bound address wins",
			mapping: PortMapping{HostIP: "127.0.0.1", HostPort: 8080, ContainerPort: 80, Protocol: "tcp"},
			host:    "192.168.1.10",
			want:    "http://127.0.0.1:8080",
		},
		{
			name:    "ipv6 keeps its brackets",
			mapping: PortMapping{HostIP: "fd00::1", HostPort: 8080, ContainerPort: 80, Protocol: "tcp"},
			host:    "192.168.1.10",
			want:    "http://[fd00::1]:8080",
		},
		{
			name:    "https where the container port says so",
			mapping: PortMapping{HostPort: 8443, ContainerPort: 443, Protocol: "tcp"},
			host:    "nas.local",
			want:    "https://nas.local:8443",
		},
		{
			// Nothing a browser could open.
			name:    "udp is not a link",
			mapping: PortMapping{HostPort: 5353, ContainerPort: 53, Protocol: "udp"},
			host:    "nas.local",
			want:    "",
		},
		{
			name:    "no host, no link",
			mapping: PortMapping{HostPort: 8080, ContainerPort: 80, Protocol: "tcp"},
			host:    "",
			want:    "",
		},
	}
	for _, testCase := range cases {
		t.Run(testCase.name, func(t *testing.T) {
			if got := testCase.mapping.URL(testCase.host); got != testCase.want {
				t.Errorf("URL(%q) = %q, want %q", testCase.host, got, testCase.want)
			}
		})
	}
}
