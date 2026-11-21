package mgmt

import (
	"context"
	"fmt"
	"testing"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	infrav1 "github.com/k8s-nodescale/vcd-k8s-node-controller/api/v1alpha1"
	"github.com/k8s-nodescale/vcd-k8s-node-controller/internal/cloud"
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
		CapacityModel:  string(cloud.CapacityPhysical),
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
	if c.CapacityModel != string(cloud.CapacityPhysical) {
		t.Fatalf("capacityModel = %q in the view, want physical", c.CapacityModel)
	}
}

// On hosts, every worker the pool has is charged its whole shape, whatever the
// hosts currently measure: that is the entitlement, and it is what the gate
// applies. A worker still starting already holds one, so it must reduce the
// number that still fit - the pane otherwise promises room the previous click
// committed.
func TestFitChargesEveryWorkerOnAPhysicalPlatform(t *testing.T) {
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
	// 34680 - 3*15600 is negative: three charges already exceed the free part.
	if f.CPU == nil || *f.CPU != 0 {
		t.Fatalf("cpu = %v, want 0 with three workers charged", f.CPU)
	}
	// (1571338 - 738015 - 3*4096) / 4096 = 200.4, so 200 workers of 4 GB.
	if f.Memory == nil || *f.Memory != 200 {
		t.Fatalf("memory = %v, want 200 after three charges", f.Memory)
	}
}

// A quota books a VM the moment it is created, so a worker still starting is
// already in the platform's used figure. Charging it again was the drift of
// 2026-11-21: the pane said fewer fit than the gate admitted.
func TestQuotaFitDoesNotChargeWhatThePlatformAlreadyBooked(t *testing.T) {
	p := fitPool(1)
	p.Status.Capacity.CapacityModel = string(cloud.CapacityQuota)
	p.Status.Capacity.CPUPerCore = 0
	p.Status.Nodes = []infrav1.WorkerNodeStatus{
		{Name: "w-1", Phase: infrav1.NodePhaseProvisioning},
		{Name: "w-2", Phase: infrav1.NodePhaseJoining},
	}
	f := poolFit(p)
	// 34680 free of 15600 per worker is two, exactly as before they started:
	// the used figure already contains them.
	if f.CPU == nil || *f.CPU != 2 {
		t.Fatalf("cpu = %v, want 2: a quota already booked the starting workers", f.CPU)
	}
	// (1571338 - 738015) / 4096 = 203.4: nothing is charged twice.
	if f.Memory == nil || *f.Memory != 203 {
		t.Fatalf("memory = %v, want 203 with nothing charged twice", f.Memory)
	}
}

// With per-VM usage the fit is an entitlement: our workers charged in full, the
// neighbours measured. 92.8 GHz total, 10 of them ours, 8 workers at 15.6 GHz is
// over the cluster, so nothing more fits.
func TestFitUsesTheEntitlementWhenUsageIsPerVM(t *testing.T) {
	p := fitPool(1)
	p.Spec.Flavor.CPUs = 6
	p.Status.Nodes = nil
	for i := 0; i < 8; i++ {
		p.Status.Nodes = append(p.Status.Nodes, infrav1.WorkerNodeStatus{
			Name: "w-" + itoa(i), Phase: infrav1.NodePhaseReady,
		})
	}
	p.Status.Capacity.OurCPUMHz = 10000
	p.Status.Capacity.OurMemoryMB = 200000
	p.Status.Capacity.CPUMHz = 92800 // 92.8 GHz measured in total

	f := poolFit(p)
	if f.CPU == nil || *f.CPU != 0 {
		t.Fatalf("cpu = %v, want 0: 8 workers at 1:1 already exceed the cluster", f.CPU)
	}
}

// itoa keeps the test free of a strconv import for one call.
func itoa(i int) string { return fmt.Sprintf("%d", i) }
