package dockerx

import "testing"

// The total is the unique layer bytes plus everything else Docker holds;
// reclaimable is what no container or build is using.
func TestSumDiskUsage(t *testing.T) {
	var df dfResponse
	df.LayersSize = 1000
	df.Images = []struct {
		Size       int64 `json:"Size"`
		SharedSize int64 `json:"SharedSize"`
		Containers int64 `json:"Containers"`
	}{
		{Size: 600, SharedSize: 100, Containers: 2}, // in use: 500 own bytes
		{Size: 500, SharedSize: 100, Containers: 0}, // unused: reclaimable
	}
	df.Containers = []struct {
		SizeRw int64 `json:"SizeRw"`
	}{{SizeRw: 10}, {SizeRw: -1}}
	df.BuildCache = []struct {
		Size  int64 `json:"Size"`
		InUse bool  `json:"InUse"`
	}{{Size: 200, InUse: true}, {Size: 300}}
	got := sumDiskUsage(df)
	if got.Bytes != 1000+10+200+300 {
		t.Errorf("Bytes = %d, want 1510", got.Bytes)
	}
	if got.Reclaimable != (1000-500)+300 {
		t.Errorf("Reclaimable = %d, want 800", got.Reclaimable)
	}
}
