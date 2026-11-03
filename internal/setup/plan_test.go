package setup

import (
	"strings"
	"testing"
)

func TestPlanPartitions(t *testing.T) {
	if _, err := PlanPartitions(16 << 30); err == nil {
		t.Fatal("16GB disk must be refused")
	}
	p, err := PlanPartitions(500 << 30)
	if err != nil {
		t.Fatal(err)
	}
	if p.RootMB != 32768 {
		t.Fatalf("root = %d", p.RootMB)
	}
	if p.DataMB < 400<<10 {
		t.Fatalf("data = %d MiB, want the bulk of the disk", p.DataMB)
	}
	small, err := PlanPartitions(32 << 30)
	if err != nil {
		t.Fatal(err)
	}
	if small.RootMB != 10240 {
		t.Fatalf("small-disk root = %d, want 10240", small.RootMB)
	}
	if small.DataMB <= 0 {
		t.Fatalf("small-disk data = %d", small.DataMB)
	}
	mid, err := PlanPartitions(64 << 30)
	if err != nil {
		t.Fatal(err)
	}
	if mid.RootMB != 16384 {
		t.Fatalf("mid-disk root = %d, want 16384", mid.RootMB)
	}
	script := p.SfdiskScript()
	for _, want := range []string{"label: gpt", "type=uefi", `name="fog-recovery"`, `name="fog-data"`} {
		if !strings.Contains(script, want) {
			t.Fatalf("sfdisk script missing %q:\n%s", want, script)
		}
	}
}

func TestPartitionPath(t *testing.T) {
	cases := map[[2]string]string{
		{"/dev/sda", "3"}:     "/dev/sda3",
		{"/dev/nvme0n1", "1"}: "/dev/nvme0n1p1",
		{"/dev/mmcblk0", "4"}: "/dev/mmcblk0p4",
		{"/dev/vda", "2"}:     "/dev/vda2",
	}
	for in, want := range cases {
		n := int(in[1][0] - '0')
		if got := PartitionPath(in[0], n); got != want {
			t.Fatalf("PartitionPath(%s, %d) = %s, want %s", in[0], n, got, want)
		}
	}
}
