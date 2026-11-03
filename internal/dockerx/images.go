package dockerx

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"sort"
	"strings"
)

type imageDetail struct {
	ID          string   `json:"Id"`
	RepoDigests []string `json:"RepoDigests"`
}

// ImageID is the local image ID ref names now ("sha256:…").
func (c *Client) ImageID(ctx context.Context, ref string) (string, error) {
	var d imageDetail
	if err := c.get(ctx, "/images/"+ref+"/json", &d); err != nil {
		return "", err
	}
	return d.ID, nil
}

// TagImage names image src repo:tag too, moving the name if it named
// another. A name keeps an image: the containerd image store deletes one
// no name refers to, even under a container still running it.
func (c *Client) TagImage(ctx context.Context, src, repo, tag string) error {
	q := url.Values{"repo": {repo}, "tag": {tag}}
	if err := c.lifecycle(ctx, http.MethodPost, "/images/"+src+"/tag?"+q.Encode(), nil); err != nil {
		return err
	}
	if id, err := c.ImageID(ctx, repo+":"+tag); err != nil || id == "" {
		return fmt.Errorf("tagging %s as %s:%s didn't take: %v", src, repo, tag, err)
	}
	return nil
}

// ImageDigest reports the registry manifest digest of a locally present
// image reference ("sha256:…"). It is what fog records as the version an
// image app is running: a tag moves, a digest is the thing the tag pointed
// at on the day the app deployed.
//
// An image built here rather than pulled has no repo digest at all — git
// apps are identified by their commit instead.
func (c *Client) ImageDigest(ctx context.Context, ref string) (string, error) {
	var d imageDetail
	// The reference goes into the path unescaped: Docker's API expects
	// "repo/name:tag" with its slashes intact, and percent-encoding them
	// turns a valid name into a 404.
	if err := c.get(ctx, "/images/"+ref+"/json", &d); err != nil {
		return "", err
	}
	for _, rd := range d.RepoDigests {
		if i := strings.Index(rd, "@"); i >= 0 {
			return rd[i+1:], nil
		}
	}
	return "", fmt.Errorf("no repo digest for %s", ref)
}

// PinDigest rewrites an image reference to name one exact manifest, so a
// deploy lands on the version asked for and not on wherever the tag has
// moved since. "lscr.io/linuxserver/jellyfin:latest" + "sha256:ab…" becomes
// "lscr.io/linuxserver/jellyfin@sha256:ab…".
func PinDigest(ref, digest string) string {
	if digest == "" {
		return ref
	}
	return ImageName(ref) + "@" + digest
}

// ImageSummary is one local image as /images/json lists it.
type ImageSummary struct {
	ID       string   `json:"Id"`
	RepoTags []string `json:"RepoTags"`
	Created  int64    `json:"Created"` // unix seconds
}

// ListImages lists the local images matching a reference filter
// ("fog/hub" matches every fog/hub:<tag>), newest first.
func (c *Client) ListImages(ctx context.Context, reference string) ([]ImageSummary, error) {
	filters, err := json.Marshal(map[string][]string{"reference": {reference}})
	if err != nil {
		return nil, err
	}
	var out []ImageSummary
	if err := c.get(ctx, "/images/json?filters="+url.QueryEscape(string(filters)), &out); err != nil {
		return nil, err
	}
	sort.SliceStable(out, func(i, j int) bool { return out[i].Created > out[j].Created })
	return out, nil
}

// ImageName strips any tag or digest from a reference, leaving the name. A
// colon only ends the name when it comes after the last slash — a registry
// host may carry a port ("nas.lan:5000/media/app"), and cutting there would
// leave a reference to a machine.
func ImageName(ref string) string {
	if i := strings.Index(ref, "@"); i >= 0 {
		ref = ref[:i]
	}
	if i := strings.LastIndex(ref, ":"); i > strings.LastIndex(ref, "/") {
		ref = ref[:i]
	}
	return ref
}
