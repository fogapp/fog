package dockerx

import (
	"context"
	"net/http"
)

// DiskUsage is Docker's own accounting of what it holds: image layers,
// container writable layers, volumes, and the buildkit cache. Bytes is
// the unique on-disk total — shared layers counted once — and
// Reclaimable is what a prune would free: images no container uses and
// build cache nothing is using, by the same arithmetic the `docker
// system df` column uses.
type DiskUsage struct {
	Bytes       int64
	Reclaimable int64
}

type dfResponse struct {
	LayersSize int64 `json:"LayersSize"`
	Images     []struct {
		Size       int64 `json:"Size"`
		SharedSize int64 `json:"SharedSize"`
		Containers int64 `json:"Containers"`
	} `json:"Images"`
	Containers []struct {
		SizeRw int64 `json:"SizeRw"`
	} `json:"Containers"`
	Volumes []struct {
		UsageData *struct {
			Size     int64 `json:"Size"`
			RefCount int64 `json:"RefCount"`
		} `json:"UsageData"`
	} `json:"Volumes"`
	BuildCache []struct {
		Size  int64 `json:"Size"`
		InUse bool  `json:"InUse"`
	} `json:"BuildCache"`
}

// DiskUsage asks the daemon to size everything it holds. The daemon walks
// its store to answer — seconds to minutes on a large one — so this goes
// through the untimed client and the caller's context bounds it; never
// call it on a request path.
func (c *Client) DiskUsage(ctx context.Context) (*DiskUsage, error) {
	var df dfResponse
	if err := c.do(ctx, c.stream, http.MethodGet, "/system/df", nil, &df); err != nil {
		return nil, err
	}
	return sumDiskUsage(df), nil
}

func sumDiskUsage(df dfResponse) *DiskUsage {
	u := &DiskUsage{Bytes: df.LayersSize}
	// An image a container uses is kept; its own layers (not the ones it
	// shares with others) are what the prune leaves behind.
	var usedLayers int64
	for _, im := range df.Images {
		if im.Containers != 0 && im.Size >= 0 && im.SharedSize >= 0 {
			usedLayers += im.Size - im.SharedSize
		}
	}
	u.Reclaimable = df.LayersSize - usedLayers
	for _, ct := range df.Containers {
		if ct.SizeRw > 0 {
			u.Bytes += ct.SizeRw
		}
	}
	for _, v := range df.Volumes {
		if v.UsageData == nil || v.UsageData.Size < 0 {
			continue
		}
		u.Bytes += v.UsageData.Size
		if v.UsageData.RefCount == 0 {
			u.Reclaimable += v.UsageData.Size
		}
	}
	for _, b := range df.BuildCache {
		if b.Size < 0 {
			continue
		}
		u.Bytes += b.Size
		if !b.InUse {
			u.Reclaimable += b.Size
		}
	}
	if u.Reclaimable < 0 {
		u.Reclaimable = 0
	}
	return u
}
