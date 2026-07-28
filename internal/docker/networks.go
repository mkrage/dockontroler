package docker

import (
	"context"
	"net/http"
	"net/url"
)

// ConnectNetwork attaches an existing container to a network.
//
// Recreate needs this because a create call reliably attaches only one network.
// Containers on several networks — a common Compose setup with a frontend and a
// database network — get their first network at create time and the rest here.
//
// network may be a network id or name; config may be nil for defaults.
func (c *Client) ConnectNetwork(ctx context.Context, network, containerID string, config *EndpointConfig) error {
	body := struct {
		Container      string          `json:"Container"`
		EndpointConfig *EndpointConfig `json:"EndpointConfig,omitempty"`
	}{
		Container:      containerID,
		EndpointConfig: config,
	}
	path := "/networks/" + url.PathEscape(network) + "/connect"
	return c.do(ctx, http.MethodPost, path, nil, body, nil)
}
