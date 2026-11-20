package mgmt

import (
	"context"
	"testing"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	infrav1 "github.com/k8s-nodescale/vcd-k8s-node-controller/api/v1alpha1"
)

// fitPool is a pool whose snapshot has room in every dimension: 15.6 GHz free
// at 1:1, 814 GB of memory and 23.8 TB on the datastore.
func fitPool(perCore int32) *infrav1.K8sWorkerPool {
	p := testPool(1)
	p.Spec.Flavor.CPUs = 6
	p.Spec.Flavor.MemoryMB = 4096
	p.Spec.Flavor.OSDiskGB = 40
	p.Spec.Flavor.Disks = []infrav1.DiskSpec{{SizeGB: 10}, {SizeGB: 30}}
	assumption := perCore
	p.Spec.Capacity.CPUPerCore = &assumption
	p.Status.Capacity = &infrav1.CapacityStatus{
		CPUMHz:         34680,
		CPULimitMHz:    69360,
		CPUMHzPerCore:  2600,
		CPUPerCore:     perCore,
		Physical:       true,
		MemoryMB:       738015,
		MemoryLimitMB:  1571338,
		StorageMB:      8565484,
		StorageLimitMB: 33554432,
	}
	return p
}

func want(t *testing.T, got *int32, n int32, what string) {
	t.Helper()
	if got == nil {
		t.Fatalf("%s: no number published, want %d", what, n)
	}
	if *got != n {
		t.Fatalf("%s = %d, want %d", what, *got, n)
	}
}

// A worker costs 6 vCPU x 2600 MHz at 1:1 and half of that at 2:1, and the
// numbers follow the same charge the pre-flight makes.
func TestFitChargesCPUAtTheAssumption(t *testing.T) {
	p := fitPool(1)
	f := poolFit(p)
	want(t, f.CPU, 2, "cpu at 1:1") // 34680 / 15600
	if f.CPUPerWorkerMHz != 15600 {
		t.Fatalf("cpuPerWorkerMHz = %d, want 15600", f.CPUPerWorkerMHz)
	}

	p = fitPool(2)
	f = poolFit(p)
	want(t, f.CPU, 4, "cpu at 2:1") // 34680 / 7800
	if f.CPUPerWorkerMHz != 7800 {
		t.Fatalf("cpuPerWorkerMHz = %d at 2:1, want 7800", f.CPUPerWorkerMHz)
	}
}

// Memory and disk are charged in full: the flavour's allocation, not what the
// guest happens to use.
func TestFitChargesMemoryAndDiskInFull(t *testing.T) {
	f := poolFit(fitPool(1))
	want(t, f.Memory, 203, "memory")   // (1571338-738015) / 4096
	want(t, f.Storage, 305, "storage") // (33554432-8565484) / (80*1024)
	if f.DiskPerWorkerMB != 80*1024 {
		t.Fatalf("diskPerWorkerMB = %d, want 81920", f.DiskPerWorkerMB)
	}
}

// A dimension that cannot be measured is left out instead of reading zero: a
// missing number must not become the reason the pool stops.
func TestFitOmitsWhatItCannotMeasure(t *testing.T) {
	p := fitPool(1)
	p.Status.Capacity.CPULimitMHz = 0
	p.Status.Capacity.CPUMHzPerCore = 0
	p.Spec.Flavor.MemoryMB = 0
	p.Spec.Flavor.OSDiskGB = 0
	p.Spec.Flavor.Disks = nil

	f := poolFit(p)
	if f.CPU != nil || f.Memory != nil || f.Storage != nil {
		t.Fatalf("unmeasurable dimensions were published: %#v", f)
	}
}

// No snapshot, no fit.
func TestFitNeedsASnapshot(t *testing.T) {
	p := fitPool(1)
	p.Status.Capacity = nil
	if f := poolFit(p); f != nil {
		t.Fatalf("fit = %#v without a snapshot, want nil", f)
	}
}

// A pool whose vCPU count is unknown cannot be charged at all.
func TestFitNeedsAFlavour(t *testing.T) {
	p := fitPool(1)
	p.Spec.Flavor.CPUs = 0
	if f := poolFit(p); f.CPU != nil {
		t.Fatalf("cpu = %d with no vCPU count", *f.CPU)
	}
	_ = metav1.Now
}

// The page charges a worker itself so the assumption switch reacts before any
// collection answers. That needs the core speed in the DTO - it was missing once
// and the local charge silently fell back to the snapshot, which is how the
// breakdown lost its "free ... of ..." half.
func TestCapacityViewCarriesTheChargeBasis(t *testing.T) {
	p := fitPool(1)
	col := &Collector{Client: testClient(t, p)}
	pools, err := col.CollectOnce(context.Background())
	if err != nil {
		t.Fatalf("collect: %v", err)
	}
	if len(pools) != 1 || pools[0].Capacity == nil {
		t.Fatalf("no capacity in the view: %#v", pools)
	}
	c := pools[0].Capacity
	if c.CPUMHzPerCore != 2600 {
		t.Fatalf("cpuMHzPerCore = %d in the view, want 2600", c.CPUMHzPerCore)
	}
	if c.Fit == nil || c.Fit.CPU == nil {
		t.Fatalf("no cpu fit in the view: %#v", c.Fit)
	}
	if !c.Physical {
		t.Fatal("physical was dropped from the view")
	}
}

// A worker that is still starting has a VM but no measured load. Its standing
// charge is spent, so it must reduce the number of workers that still fit - the
// pane promised room that the previous click had already committed.
func TestFitChargesWorkersThatAreStillStarting(t *testing.T) {
	p := fitPool(1)
	if f := poolFit(p); f.CPU == nil || *f.CPU != 2 {
		t.Fatalf("cpu = %v, want 2 with nothing starting", f.CPU)
	}
	p.Status.Nodes = []infrav1.WorkerNodeStatus{
		{Name: "w-1", Phase: infrav1.NodePhaseReady},
		{Name: "w-2", Phase: infrav1.NodePhaseProvisioning},
		{Name: "w-3", Phase: infrav1.NodePhaseJoining},
	}
	f := poolFit(p)
	// 34680 - 2*15600 = 3480, which fits nothing.
	if f.CPU == nil || *f.CPU != 0 {
		t.Fatalf("cpu = %v, want 0 while two workers are starting", f.CPU)
	}
	if f.Memory == nil || *f.Memory != 201 {
		t.Fatalf("memory = %v, want 201 after the two reservations", f.Memory)
	}
}
