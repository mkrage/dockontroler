package docker

// Restart policy names as the Docker Engine spells them. Dockontroler
// deliberately exposes only these three; "on-failure" is a valid Docker
// policy but adds a retry-count field that the UI has no room for.
const (
	PolicyNo            = "no"
	PolicyAlways        = "always"
	PolicyUnlessStopped = "unless-stopped"
)

// Container states reported by the Engine.
const (
	StateCreated    = "created"
	StateRunning    = "running"
	StatePaused     = "paused"
	StateRestarting = "restarting"
	StateRemoving   = "removing"
	StateExited     = "exited"
	StateDead       = "dead"
)

// Labels that Docker Compose writes onto every container it creates.
const (
	LabelComposeProject = "com.docker.compose.project"
	LabelComposeService = "com.docker.compose.service"
	// LabelComposeConfigHash is how Compose decides whether a container still
	// matches its yaml. Recreating a container must preserve it verbatim, or
	// the next `docker compose up -d` will replace the container again.
	LabelComposeConfigHash = "com.docker.compose.config-hash"
	LabelComposeNumber     = "com.docker.compose.container-number"
)

// ContainerSummary is one entry of GET /containers/json. Only the fields the
// overview actually renders are modelled here.
type ContainerSummary struct {
	ID      string            `json:"Id"`
	Names   []string          `json:"Names"` // each with a leading slash
	Image   string            `json:"Image"`
	ImageID string            `json:"ImageID"`
	State   string            `json:"State"`
	Status  string            `json:"Status"` // human text, e.g. "Up 3 hours"
	Created int64             `json:"Created"`
	Labels  map[string]string `json:"Labels"`
}

// ContainerInspect is GET /containers/{id}/json.
//
// Config and HostConfig are intentionally left as untyped maps. Recreating a
// container means handing Docker back everything it just gave us, and
// HostConfig alone carries roughly eighty fields — Sysctls, Ulimits, CapAdd,
// DeviceRequests for GPU passthrough, LogConfig, and so on. Modelling them as
// a struct would silently drop every field we forgot to list, which is exactly
// the kind of bug that loses somebody's GPU or logging setup on recreate. So we
// keep the decoded JSON verbatim and only touch the handful of keys that must
// be adjusted (see manager/config_copy.go).
type ContainerInspect struct {
	ID    string `json:"Id"`
	Name  string `json:"Name"` // with a leading slash, e.g. "/nextcloud"
	Image string `json:"Image"`

	State struct {
		Running bool   `json:"Running"`
		Status  string `json:"Status"`
	} `json:"State"`

	// Mounts is the resolved mount list. It is the only place where anonymous
	// volumes appear under their generated names, which is what lets us carry
	// their data over to the new container.
	Mounts []MountPoint `json:"Mounts"`

	NetworkSettings struct {
		Networks map[string]EndpointSettings `json:"Networks"`
	} `json:"NetworkSettings"`

	Config     map[string]any `json:"Config"`
	HostConfig map[string]any `json:"HostConfig"`
}

// MountPoint is one entry of ContainerInspect.Mounts.
type MountPoint struct {
	Type        string `json:"Type"` // bind | volume | tmpfs | npipe
	Name        string `json:"Name"` // volume name; empty for binds and tmpfs
	Source      string `json:"Source"`
	Destination string `json:"Destination"`
	Mode        string `json:"Mode"`
	RW          bool   `json:"RW"`
}

// EndpointSettings is a network attachment as reported by inspect. It mixes
// configuration (IPAMConfig, Aliases) with runtime state (IPAddress, MacAddress,
// assigned by Docker at start). Only the configuration half may be sent back —
// see EndpointSettings.Config.
type EndpointSettings struct {
	IPAMConfig *EndpointIPAMConfig `json:"IPAMConfig"`
	Links      []string            `json:"Links"`
	Aliases    []string            `json:"Aliases"`
	DriverOpts map[string]string   `json:"DriverOpts"`

	NetworkID  string `json:"NetworkID"`
	EndpointID string `json:"EndpointID"`
	Gateway    string `json:"Gateway"`
	IPAddress  string `json:"IPAddress"`
	MacAddress string `json:"MacAddress"`
}

// Config returns the settable subset, safe to pass to create or connect.
func (e EndpointSettings) Config() *EndpointConfig {
	return &EndpointConfig{
		IPAMConfig: e.IPAMConfig,
		Links:      e.Links,
		Aliases:    e.Aliases,
		DriverOpts: e.DriverOpts,
	}
}

// EndpointConfig is the write shape of a network attachment.
type EndpointConfig struct {
	IPAMConfig *EndpointIPAMConfig `json:"IPAMConfig,omitempty"`
	Links      []string            `json:"Links,omitempty"`
	Aliases    []string            `json:"Aliases,omitempty"`
	DriverOpts map[string]string   `json:"DriverOpts,omitempty"`
}

// EndpointIPAMConfig holds statically assigned addresses. Present only when the
// user pinned an address, so copying it preserves static IPs across a recreate.
type EndpointIPAMConfig struct {
	IPv4Address  string   `json:"IPv4Address,omitempty"`
	IPv6Address  string   `json:"IPv6Address,omitempty"`
	LinkLocalIPs []string `json:"LinkLocalIPs,omitempty"`
}

// RestartPolicy is the body of POST /containers/{id}/update.
type RestartPolicy struct {
	Name              string `json:"Name"`
	MaximumRetryCount int    `json:"MaximumRetryCount"`
}

// CreateBody is the request body of POST /containers/create.
//
// The Engine expects the container Config fields *flattened* at the top level
// (Image, Env, Labels, ...) with HostConfig and NetworkingConfig nested beside
// them. Because Config is already an untyped map, the body is assembled as a
// map too — see manager/config_copy.go.
type CreateBody map[string]any

// NetworkingConfig is the "NetworkingConfig" key of a create body.
type NetworkingConfig struct {
	EndpointsConfig map[string]*EndpointConfig `json:"EndpointsConfig,omitempty"`
}

// CreateResponse is the reply to POST /containers/create.
type CreateResponse struct {
	ID       string   `json:"Id"`
	Warnings []string `json:"Warnings"`
}

// UpdateResponse is the reply to POST /containers/{id}/update.
type UpdateResponse struct {
	Warnings []string `json:"Warnings"`
}
