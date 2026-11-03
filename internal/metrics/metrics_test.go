package metrics

import (
	"testing"
	"time"
)

func TestSeriesRingOrder(t *testing.T) {
	s := New(t.TempDir(), time.Second, 3*time.Second) // ring of 3

	if got := s.Series(); len(got) != 0 {
		t.Fatalf("empty sampler returned %d samples", len(got))
	}

	push := func(ts int64) { s.ring.Add(Sample{T: ts}) }

	push(1)
	push(2)
	if got := s.Series(); len(got) != 2 || got[0].T != 1 || got[1].T != 2 {
		t.Fatalf("partial ring wrong: %+v", got)
	}

	push(3)
	push(4) // wraps, evicting T=1
	got := s.Series()
	if len(got) != 3 || got[0].T != 2 || got[1].T != 3 || got[2].T != 4 {
		t.Fatalf("wrapped ring wrong: %+v", got)
	}
}

func TestTakeProducesSaneSample(t *testing.T) {
	s := New(t.TempDir(), time.Second, 10*time.Second)
	s.cpu.prime()
	s.take()

	got := s.Series()
	if len(got) != 1 {
		t.Fatalf("expected 1 sample, got %d", len(got))
	}
	smp := got[0]
	if smp.T == 0 {
		t.Error("sample has no timestamp")
	}
	if smp.CPUPct < 0 || smp.CPUPct > 100 {
		t.Errorf("cpuPct out of range: %v", smp.CPUPct)
	}
	if smp.MemTotal == 0 || smp.MemUsed > smp.MemTotal {
		t.Errorf("memory figures implausible: used=%d total=%d", smp.MemUsed, smp.MemTotal)
	}
	if smp.DiskTotal == 0 || smp.DiskUsed > smp.DiskTotal {
		t.Errorf("disk figures implausible: used=%d total=%d", smp.DiskUsed, smp.DiskTotal)
	}
}
