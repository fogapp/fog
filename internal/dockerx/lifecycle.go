package dockerx

import (
	"context"
	"net/http"
	"net/url"
	"strconv"
)

// Container and image lifecycle over the Engine API. These replace
// `docker rm`/`stop`/`network disconnect`/`rmi` shell-outs: no CLI process
// per call, no argv parsing, and an error that says what the daemon said.
//
// Each treats "it's already gone" as done — the outcome the caller wanted
// is the thing's absence, and the CLI calls these replace had their
// failure ignored for exactly that case. Anything else the daemon refuses
// (an image still in use, a removal already in progress) is an error.
//
// They run on the timeout-free client: a graceful stop may legitimately
// take its whole grace period, which the general client's ten seconds
// would cut short. The caller's context is the deadline.

// Remove deletes a container; force kills it first if it is running.
func (c *Client) Remove(ctx context.Context, id string, force bool) error {
	q := url.Values{}
	if force {
		q.Set("force", "true")
	}
	return c.lifecycle(ctx, http.MethodDelete, "/containers/"+url.PathEscape(id)+"?"+q.Encode(), nil)
}

// SetRestartPolicy changes a container's restart policy ("no",
// "unless-stopped", …) in place.
func (c *Client) SetRestartPolicy(ctx context.Context, id, policy string) error {
	return c.lifecycle(ctx, http.MethodPost, "/containers/"+url.PathEscape(id)+"/update",
		map[string]any{"RestartPolicy": map[string]any{"Name": policy}})
}

// Stop asks a container to stop, killing it once timeoutSec seconds have
// passed. A container that is already stopped is not an error.
func (c *Client) Stop(ctx context.Context, id string, timeoutSec int) error {
	return c.lifecycle(ctx, http.MethodPost, "/containers/"+url.PathEscape(id)+"/stop?t="+strconv.Itoa(timeoutSec), nil)
}

// NetworkDisconnect takes a container off a network, which drops it out
// of that network's DNS.
func (c *Client) NetworkDisconnect(ctx context.Context, network, container string, force bool) error {
	return c.lifecycle(ctx, http.MethodPost, "/networks/"+url.PathEscape(network)+"/disconnect",
		map[string]any{"Container": container, "Force": force})
}

// RemoveImage untags and deletes an image. An image a container still uses
// is refused, not forced.
func (c *Client) RemoveImage(ctx context.Context, ref string) error {
	// The reference goes into the path unescaped, as in ImageDigest:
	// "repo/name:tag" keeps its slashes.
	return c.lifecycle(ctx, http.MethodDelete, "/images/"+ref, nil)
}

// lifecycle sends one request whose success has no body worth reading.
// 404 is success (see above); 304 is Stop's "already stopped".
func (c *Client) lifecycle(ctx context.Context, method, path string, body any) error {
	err := c.do(ctx, c.stream, method, path, body, nil)
	if hasStatus(err, http.StatusNotModified, http.StatusNotFound) {
		return nil
	}
	return err
}
