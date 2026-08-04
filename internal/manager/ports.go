package manager

import (
	"sort"
	"strconv"
	"strings"

	"github.com/mkrage/dockontroler/internal/docker"
)

// PortMapping is one way to reach a container from outside it: the port you type
// into a browser, and the container port behind it.
//
// Only reachable ports are modelled. A port the image merely exposes, without a
// host side, is left out — naming it in the overview would suggest an address
// that does not answer.
type PortMapping struct {
	// HostIP is the interface the port is published on, empty for every
	// interface. It is kept as Docker reports it: a container bound to one
	// address answers on that address only, so it decides where a link may point.
	HostIP string

	// HostPort is the port on the host. Zero for a container on the host network,
	// where nothing is translated and ContainerPort is already the host port.
	HostPort      int
	ContainerPort int
	Protocol      string // tcp, udp or sctp

	// HostNetwork marks a container sharing the host's network namespace
	// (network_mode: host). Docker publishes no ports for those, so these come
	// from what the image exposes — the best available answer to "which port",
	// and the reason such a container is not simply shown as having none.
	HostNetwork bool
}

// Port is the number to reach the service on from outside the container.
func (p PortMapping) Port() int {
	if p.HostPort != 0 {
		return p.HostPort
	}
	return p.ContainerPort
}

// Label is the short form for the overview: the port on its own when nothing is
// translated, "8080 → 80" when it is. The protocol is only spelled out when it
// is not the tcp everybody assumes.
func (p PortMapping) Label() string {
	label := strconv.Itoa(p.Port())
	if p.HostPort != 0 && p.HostPort != p.ContainerPort {
		label += " → " + strconv.Itoa(p.ContainerPort)
	}
	if p.Protocol != "" && p.Protocol != "tcp" {
		label += "/" + p.Protocol
	}
	return label
}

// Detail spells the mapping out for a tooltip, including the bound interface —
// which is the part that explains why a link points where it does.
func (p PortMapping) Detail() string {
	protocol := p.Protocol
	if protocol == "" {
		protocol = "tcp"
	}
	if p.HostNetwork {
		return "port " + strconv.Itoa(p.ContainerPort) + "/" + protocol +
			" on the host itself (network_mode: host)"
	}

	host := "host port " + strconv.Itoa(p.HostPort)
	if p.HostIP != "" {
		host = "host " + p.HostIP + ":" + strconv.Itoa(p.HostPort)
	}
	return host + " → container port " + strconv.Itoa(p.ContainerPort) + "/" + protocol
}

// Scheme guesses how a browser should address the service. It is a guess: the
// daemon knows which ports are published, never what speaks behind them.
func (p PortMapping) Scheme() string {
	switch p.ContainerPort {
	case 443, 8443, 9443:
		return "https"
	default:
		return "http"
	}
}

// URL is a link to the service, or "" when there cannot be a useful one.
//
// host is the address the browser used to reach docKontroler, ready to drop into
// a URL. That is the missing piece: the daemon reports a binding on 0.0.0.0, and
// nothing on this side knows which of the host's addresses the user can actually
// reach — but the browser just demonstrated one.
//
// A port bound to a specific address overrides it, because that address is the
// only one that answers. Loopback bindings are included in that: the link is then
// right when you browse from the host and honestly wrong from anywhere else,
// which beats inventing an address that was never bound.
func (p PortMapping) URL(host string) string {
	// UDP and SCTP have nothing a browser could open.
	if p.Protocol != "" && p.Protocol != "tcp" {
		return ""
	}

	target := host
	if p.HostIP != "" {
		target = hostForURL(p.HostIP)
	}
	if target == "" {
		return ""
	}
	return p.Scheme() + "://" + target + ":" + strconv.Itoa(p.Port())
}

// portsOf works out how a container can be reached, from whichever source knows.
func portsOf(summary docker.ContainerSummary, inspected *docker.ContainerInspect) []PortMapping {
	if inspected != nil && mapString(inspected.HostConfig, "NetworkMode") == "host" {
		return hostNetworkPorts(inspected)
	}

	mappings := publishedPorts(summary.Ports)
	if len(mappings) == 0 && inspected != nil {
		// A stopped container publishes nothing, so its list entry carries no ports
		// at all. The bindings it will use are still recorded, and showing them is
		// what makes "which port was that again" answerable without starting it
		// first.
		mappings = configuredPorts(mapSub(inspected.HostConfig, "PortBindings"))
	}
	return mappings
}

// publishedPorts reads the live mappings from a list entry.
func publishedPorts(ports []docker.Port) []PortMapping {
	var mappings []PortMapping
	for _, port := range ports {
		if port.PublicPort == 0 {
			// Exposed by the image but not published: not reachable from the host.
			continue
		}
		mappings = append(mappings, PortMapping{
			HostIP:        specificIP(port.IP),
			HostPort:      port.PublicPort,
			ContainerPort: port.PrivatePort,
			Protocol:      protocolOr(port.Type),
		})
	}
	return tidyPorts(mappings)
}

// configuredPorts reads HostConfig.PortBindings, which is what a stopped
// container has instead of live mappings. The shape is
// {"80/tcp": [{"HostIp": "", "HostPort": "8080"}]}.
func configuredPorts(bindings map[string]any) []PortMapping {
	var mappings []PortMapping
	for spec, raw := range bindings {
		containerPort, protocol := splitPortSpec(spec)
		if containerPort == 0 {
			continue
		}
		list, _ := raw.([]any)
		for _, entry := range list {
			binding, _ := entry.(map[string]any)
			hostPort, err := strconv.Atoi(mapString(binding, "HostPort"))
			if err != nil || hostPort == 0 {
				// An empty HostPort means "any free port", chosen when the container
				// starts. There is no number to show yet.
				continue
			}
			mappings = append(mappings, PortMapping{
				HostIP:        specificIP(mapString(binding, "HostIp")),
				HostPort:      hostPort,
				ContainerPort: containerPort,
				Protocol:      protocol,
			})
		}
	}
	return tidyPorts(mappings)
}

// hostNetworkPorts falls back to the image's exposed ports. On the host network
// there is no translation, so those numbers are host ports as they stand.
func hostNetworkPorts(inspected *docker.ContainerInspect) []PortMapping {
	var mappings []PortMapping
	for spec := range mapSub(inspected.Config, "ExposedPorts") {
		containerPort, protocol := splitPortSpec(spec)
		if containerPort == 0 {
			continue
		}
		mappings = append(mappings, PortMapping{
			ContainerPort: containerPort,
			Protocol:      protocol,
			HostNetwork:   true,
		})
	}
	return tidyPorts(mappings)
}

// tidyPorts sorts the mappings and drops duplicates, so the auto-refresh cannot
// reorder them and the IPv4/IPv6 pair Docker reports for one published port is
// shown once.
func tidyPorts(mappings []PortMapping) []PortMapping {
	if len(mappings) == 0 {
		return nil
	}
	sort.Slice(mappings, func(i, j int) bool {
		left, right := mappings[i], mappings[j]
		if left.Port() != right.Port() {
			return left.Port() < right.Port()
		}
		if left.ContainerPort != right.ContainerPort {
			return left.ContainerPort < right.ContainerPort
		}
		if left.Protocol != right.Protocol {
			return left.Protocol < right.Protocol
		}
		return left.HostIP < right.HostIP
	})

	unique := mappings[:1]
	for _, mapping := range mappings[1:] {
		if mapping != unique[len(unique)-1] {
			unique = append(unique, mapping)
		}
	}
	return unique
}

// splitPortSpec reads Docker's "80/tcp" notation.
func splitPortSpec(spec string) (port int, protocol string) {
	number, protocol := spec, ""
	if slash := strings.IndexByte(spec, '/'); slash >= 0 {
		number, protocol = spec[:slash], spec[slash+1:]
	}
	parsed, err := strconv.Atoi(number)
	if err != nil {
		return 0, ""
	}
	return parsed, protocolOr(protocol)
}

func protocolOr(protocol string) string {
	if protocol == "" {
		return "tcp"
	}
	return protocol
}

// specificIP keeps only an address that narrows where the port answers. The
// wildcards mean "every interface", which is the same as knowing nothing about
// where to point a link.
func specificIP(ip string) string {
	switch strings.Trim(ip, "[]") {
	case "", "0.0.0.0", "::":
		return ""
	default:
		return ip
	}
}

// hostForURL makes an address safe to put in a URL: an IPv6 literal needs
// brackets, or everything after its first colon reads as a port.
func hostForURL(host string) string {
	host = strings.Trim(host, "[]")
	if strings.Contains(host, ":") {
		return "[" + host + "]"
	}
	return host
}
