package controller

import (
	"testing"

	infrav1 "github.com/k8s-nodescale/vcd-k8s-node-controller/api/v1alpha1"
)

// A data disk is a compute dimension like CPU or memory: raising it in the spec
// is a rollout, lowering it is a recreate, and a disk one side does not know
// about is "no opinion" so adoption never turns into a rollout.
func TestCompareFlavorWithDataDisks(t *testing.T) {
	have := func(disks ...infrav1.DiskStatus) *infrav1.FlavorStatus {
		return &infrav1.FlavorStatus{CPUs: 2, MemoryMB: 8192, OSDiskGB: 20, Disks: disks}
	}
	cases := []struct {
		name  string
		have  *infrav1.FlavorStatus
		want  *infrav1.FlavorStatus
		delta flavorDelta
	}{
		{
			name:  "same disks",
			have:  have(infrav1.DiskStatus{UnitNumber: 1, SizeGB: 10}, infrav1.DiskStatus{UnitNumber: 2, SizeGB: 30}),
			want:  have(infrav1.DiskStatus{UnitNumber: 1, SizeGB: 10}, infrav1.DiskStatus{UnitNumber: 2, SizeGB: 30}),
			delta: flavorEqual,
		},
		{
			name:  "a bigger data disk",
			have:  have(infrav1.DiskStatus{UnitNumber: 1, SizeGB: 10}),
			want:  have(infrav1.DiskStatus{UnitNumber: 1, SizeGB: 40}),
			delta: flavorGrow,
		},
		{
			name:  "a smaller data disk",
			have:  have(infrav1.DiskStatus{UnitNumber: 1, SizeGB: 40}),
			want:  have(infrav1.DiskStatus{UnitNumber: 1, SizeGB: 10}),
			delta: flavorShrink,
		},
		{
			name:  "a disk the platform does not report",
			have:  have(),
			want:  have(infrav1.DiskStatus{UnitNumber: 1, SizeGB: 10}),
			delta: flavorEqual,
		},
		{
			name: "disk growth together with a shrink of memory",
			have: func() *infrav1.FlavorStatus {
				f := have(infrav1.DiskStatus{UnitNumber: 1, SizeGB: 10})
				f.MemoryMB = 16384
				return f
			}(),
			want: func() *infrav1.FlavorStatus {
				f := have(infrav1.DiskStatus{UnitNumber: 1, SizeGB: 40})
				f.MemoryMB = 8192
				return f
			}(),
			delta: flavorMixed,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := compareFlavor(tc.have, tc.want); got != tc.delta {
				t.Fatalf("delta = %s, want %s", got, tc.delta)
			}
		})
	}
}

// A shrink cannot be applied in place, so it must take the recreate path.
func TestDataDiskShrinkNeedsRecreate(t *testing.T) {
	for _, d := range []flavorDelta{flavorShrink, flavorMixed} {
		if !d.needsRecreate() {
			t.Fatalf("%s should need a recreate", d)
		}
	}
	if flavorGrow.needsRecreate() {
		t.Fatal("a grow must be applied in place, not by recreating the worker")
	}
}

// The provider matches a spec disk to a platform disk by bus and unit, so both
// have to survive the conversion. A zero bus number matched nothing on the
// first live grow (0.14.1).
func TestResizeDisksKeepsTheBusNumber(t *testing.T) {
	got := resizeDisks([]infrav1.DiskStatus{
		{BusNumber: 1, UnitNumber: 0, SizeGB: 10},
		{BusNumber: 1, UnitNumber: 1, SizeGB: 0},
		{BusNumber: 1, UnitNumber: 2, SizeGB: 30},
	})
	if len(got) != 2 {
		t.Fatalf("%d disks, want 2: a zero size is not a request", len(got))
	}
	for i, want := range []infrav1.DiskStatus{
		{BusNumber: 1, UnitNumber: 0, SizeGB: 10},
		{BusNumber: 1, UnitNumber: 2, SizeGB: 30},
	} {
		if got[i].BusNumber != want.BusNumber || got[i].UnitNumber != want.UnitNumber || got[i].SizeGB != want.SizeGB {
			t.Fatalf("disk %d = %#v, want %#v", i, got[i], want)
		}
	}
}
