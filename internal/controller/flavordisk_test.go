package controller

import (
	"testing"

	infrav1 "github.com/k8s-nodescale/vcd-k8s-node-controller/api/v1alpha1"
)

// A data disk is a compute dimension like CPU or memory: raising it in the spec
// is a rollout, lowering it is... guarded. A disk that is bigger in the platform
// than in the spec is what a hand-grown disk looks like, and recreating the VM
// to match a smaller spec would destroy the data.
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
			delta: flavorDiskShrink,
		},
		{
			name: "a smaller system disk",
			have: func() *infrav1.FlavorStatus {
				f := have()
				f.OSDiskGB = 40
				return f
			}(),
			want:  have(),
			delta: flavorDiskShrink,
		},
		{
			name: "a smaller vCPU is still a recreate",
			have: func() *infrav1.FlavorStatus {
				f := have()
				f.CPUs = 8
				return f
			}(),
			want:  have(),
			delta: flavorShrink,
		},
		{
			name: "disk shrink wins over a CPU grow",
			have: func() *infrav1.FlavorStatus {
				f := have(infrav1.DiskStatus{UnitNumber: 1, SizeGB: 40})
				f.CPUs = 1
				return f
			}(),
			want: func() *infrav1.FlavorStatus {
				f := have(infrav1.DiskStatus{UnitNumber: 1, SizeGB: 10})
				f.CPUs = 2
				return f
			}(),
			delta: flavorDiskShrink,
		},
		{
			name:  "a disk the platform does not report",
			have:  have(),
			want:  have(infrav1.DiskStatus{UnitNumber: 1, SizeGB: 10}),
			delta: flavorEqual,
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

// A disk shrink must never take the recreate path: that is the whole point of
// separating it from the other shrinks.
func TestDiskShrinkIsGuardedNotRecreated(t *testing.T) {
	if flavorDiskShrink.needsRecreate() {
		t.Fatal("a disk shrink would recreate the worker and destroy its data")
	}
	if !flavorDiskShrink.guarded() {
		t.Fatal("a disk shrink must be guarded")
	}
	for _, d := range []flavorDelta{flavorShrink, flavorMixed, flavorTemplate} {
		if d.guarded() {
			t.Fatalf("%s must not be guarded", d)
		}
	}
	if !flavorShrink.needsRecreate() {
		t.Fatal("a vCPU or memory shrink is still a recreate")
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
