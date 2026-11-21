package controller

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"

	infrav1 "github.com/k8s-nodescale/vcd-k8s-node-controller/api/v1alpha1"
	"github.com/k8s-nodescale/vcd-k8s-node-controller/internal/cloud"
	"github.com/k8s-nodescale/vcd-k8s-node-controller/internal/kube"
)

// fullMemory is a vDC with no room for another test worker.
func fullMemory() cloud.PlatformCapacity {
	return cloud.PlatformCapacity{MemoryMB: cloud.ResourceCapacity{Limit: 4096, Used: 4096}}
}

func TestScaleUpQueuesWhenCapacityIsShort(t *testing.T) {
	ctx := context.Background()
	pool := testPool(2)
	fc := &fakeCloud{capacity: fullMemory()}
	r := newTestReconciler(t, fc, &fakeKube{}, pool)

	if _, err := r.Reconcile(ctx, requestFor(pool)); err != nil {
		t.Fatalf("reconcile: %v", err)
	}
	if len(fc.created) != 0 {
		t.Fatalf("created %d VMs, want 0: %#v", len(fc.created), fc.created)
	}
	got := getPool(t, r, pool)
	if got.Status.ObservedReplicas != 2 || len(got.Status.Nodes) != 2 {
		t.Fatalf("observed=%d nodes=%d, want 2 and 2", got.Status.ObservedReplicas, len(got.Status.Nodes))
	}
	for _, n := range got.Status.Nodes {
		if n.Phase != infrav1.NodePhaseQueued {
			t.Fatalf("%s phase = %s, want Queued", n.Name, n.Phase)
		}
		if n.ProviderID != "" {
			t.Fatalf("%s has providerID %q, want none", n.Name, n.ProviderID)
		}
		if !strings.Contains(n.Message, "memory") || !strings.Contains(n.Message, "4096 MB") {
			t.Fatalf("%s message = %q, want the memory shortfall", n.Name, n.Message)
		}
	}
	if len(got.Status.Ledger.Used) != 2 {
		t.Fatalf("ledger used = %v, want both addresses reserved", got.Status.Ledger.Used)
	}
	cond := metaReady(got)
	if cond == nil || cond.Status != metav1.ConditionFalse || cond.Reason != "NoResourceAvailable" {
		t.Fatalf("unexpected condition: %#v", cond)
	}
	if !strings.Contains(cond.Message, "queued for vDC capacity") {
		t.Fatalf("condition message = %q", cond.Message)
	}

	// A second pass must keep the slots queued: a slot without a VM is not a
	// worker that disappeared from vCD.
	if _, err := r.Reconcile(ctx, requestFor(pool)); err != nil {
		t.Fatalf("second reconcile: %v", err)
	}
	got = getPool(t, r, pool)
	if len(fc.created) != 0 {
		t.Fatalf("second pass created %d VMs, want 0", len(fc.created))
	}
	for _, n := range got.Status.Nodes {
		if n.Phase != infrav1.NodePhaseQueued {
			t.Fatalf("second pass: %s phase = %s, want Queued", n.Name, n.Phase)
		}
	}
}

func TestQueuedWorkerStartsWhenCapacityAppears(t *testing.T) {
	ctx := context.Background()
	pool := testPool(1)
	fc := &fakeCloud{capacity: fullMemory()}
	r := newTestReconciler(t, fc, &fakeKube{}, pool)

	if _, err := r.Reconcile(ctx, requestFor(pool)); err != nil {
		t.Fatalf("reconcile: %v", err)
	}
	if len(fc.created) != 0 {
		t.Fatalf("created %d VMs while the vDC was full", len(fc.created))
	}

	fc.capacity.MemoryMB.Used = 0
	if _, err := r.Reconcile(ctx, requestFor(pool)); err != nil {
		t.Fatalf("reconcile after freeing capacity: %v", err)
	}
	if len(fc.created) != 1 {
		t.Fatalf("created %d VMs, want 1", len(fc.created))
	}
	got := getPool(t, r, pool)
	n := got.Status.Nodes[0]
	if n.Phase != infrav1.NodePhaseProvisioning || n.ProviderID == "" {
		t.Fatalf("queued worker did not start: %#v", n)
	}
	if n.Message != "" {
		t.Fatalf("message = %q, want empty after the create", n.Message)
	}
	if n.ProvisioningStartedAt == nil {
		t.Fatal("the provisioning clock was not started")
	}
}

func TestCapacityReadFailureDoesNotBlockCreates(t *testing.T) {
	ctx := context.Background()
	pool := testPool(1)
	fc := &fakeCloud{capacityErr: errors.New("vCD unavailable")}
	r := newTestReconciler(t, fc, &fakeKube{}, pool)

	if _, err := r.Reconcile(ctx, requestFor(pool)); err != nil {
		t.Fatalf("reconcile: %v", err)
	}
	if len(fc.created) != 1 {
		t.Fatalf("created %d VMs, want 1: a failed read must not stop the pool", len(fc.created))
	}
	if got := getPool(t, r, pool); got.Status.Nodes[0].Phase != infrav1.NodePhaseProvisioning {
		t.Fatalf("phase = %s, want Provisioning", got.Status.Nodes[0].Phase)
	}
}

func TestCapacityPreflightCanBeTurnedOff(t *testing.T) {
	ctx := context.Background()
	pool := testPool(1)
	off := false
	pool.Spec.Capacity.Preflight = &off
	fc := &fakeCloud{capacity: fullMemory()}
	r := newTestReconciler(t, fc, &fakeKube{}, pool)

	if _, err := r.Reconcile(ctx, requestFor(pool)); err != nil {
		t.Fatalf("reconcile: %v", err)
	}
	if len(fc.created) != 1 {
		t.Fatalf("created %d VMs, want 1 with capacity.preflight=false", len(fc.created))
	}
	if fc.capacityCalls != 0 {
		t.Fatalf("the vDC capacity was read %d times with preflight off", fc.capacityCalls)
	}
}

func TestScaleDownDropsQueuedSlots(t *testing.T) {
	ctx := context.Background()
	pool := testPool(0)
	pool.Spec.ScaleDown.MinReplicas = 0
	pool.Spec.ScaleDown.AllowLastWorker = true
	pool.Status.Nodes = []infrav1.WorkerNodeStatus{{
		Name: "pool-wrk-01", IP: "192.168.100.7", Phase: infrav1.NodePhaseQueued,
		Message: "waiting for vDC capacity: memory: need 4096 MB, 0 MB free",
	}}
	pool.Status.ObservedReplicas = 1
	pool.Status.Ledger = infrav1.IPLedger{Used: []string{"192.168.100.7"}}
	fc := &fakeCloud{capacity: fullMemory()}
	r := newTestReconciler(t, fc, &fakeKube{}, pool)

	if _, err := r.Reconcile(ctx, requestFor(pool)); err != nil {
		t.Fatalf("reconcile: %v", err)
	}
	got := getPool(t, r, pool)
	if len(got.Status.Nodes) != 0 || got.Status.ObservedReplicas != 0 {
		t.Fatalf("queued slot was not dropped: %#v", got.Status.Nodes)
	}
	if len(got.Status.Ledger.Used) != 0 {
		t.Fatalf("ledger used = %v, want the address released", got.Status.Ledger.Used)
	}
}

func rolloutPool(t *testing.T) (*infrav1.K8sWorkerPool, *fakeCloud, *fakeKube) {
	t.Helper()
	pool := testPool(1)
	pool.Spec.Flavor.CPUs = 4
	pool.Spec.Flavor.MemoryMB = 8192
	pool.Spec.Flavor.OSDiskGB = 20
	fc := &fakeCloud{vms: []cloud.VM{{
		ID: "vm-1", Name: "pool-wrk-01", IP: "192.168.100.7",
		CPU: 2, MemoryMB: 4096, OSDiskGB: 20, DiskGB: 30,
	}}}
	return pool, fc, &fakeKube{nodes: readyNodes("pool-wrk-01")}
}

func TestRolloutQueuesWhenCapacityIsShort(t *testing.T) {
	ctx := context.Background()
	pool, fc, fk := rolloutPool(t)
	fc.capacity = cloud.PlatformCapacity{MemoryMB: cloud.ResourceCapacity{Limit: 8192, Used: 8192}}
	r := newTestReconciler(t, fc, fk, pool)

	if _, err := r.Reconcile(ctx, requestFor(pool)); err != nil {
		t.Fatalf("reconcile: %v", err)
	}
	if len(fk.cordoned) != 0 {
		t.Fatalf("cordoned %v, want nothing taken out of service", fk.cordoned)
	}
	got := getPool(t, r, pool)
	if got.Status.Rollout.Phase != infrav1.RolloutPhaseQueued {
		t.Fatalf("rollout phase = %q, want Queued", got.Status.Rollout.Phase)
	}
	if got.Status.Nodes[0].Resize != nil {
		t.Fatalf("the worker was taken into the rollout: %#v", got.Status.Nodes[0].Resize)
	}
	if !strings.Contains(got.Status.Rollout.Message, "memory") {
		t.Fatalf("rollout message = %q, want the memory shortfall", got.Status.Rollout.Message)
	}
	cond := metaReady(got)
	if cond == nil || cond.Reason != "NoResourceAvailable" {
		t.Fatalf("unexpected condition: %#v", cond)
	}
}

func TestRolloutChecksTheDeltaNotTheWholeFlavor(t *testing.T) {
	ctx := context.Background()
	pool, fc, fk := rolloutPool(t)
	// Free memory covers the 4096 MB the resize adds, but not another 8192.
	fc.capacity = cloud.PlatformCapacity{MemoryMB: cloud.ResourceCapacity{Limit: 12288, Used: 8192}}
	r := newTestReconciler(t, fc, fk, pool)

	if _, err := r.Reconcile(ctx, requestFor(pool)); err != nil {
		t.Fatalf("reconcile: %v", err)
	}
	got := getPool(t, r, pool)
	if got.Status.Rollout.Phase == infrav1.RolloutPhaseQueued {
		t.Fatalf("the roll was queued although the delta fits: %q", got.Status.Rollout.Message)
	}
	if got.Status.Nodes[0].Resize == nil {
		t.Fatal("the worker was not taken into the rollout")
	}
	if len(fk.cordoned) != 1 {
		t.Fatalf("cordoned %v, want the worker cordoned", fk.cordoned)
	}
}

func TestResizeCapacityRequest(t *testing.T) {
	have := &infrav1.FlavorStatus{CPUs: 2, MemoryMB: 4096, OSDiskGB: 20}
	want := &infrav1.FlavorStatus{CPUs: 4, MemoryMB: 8192, OSDiskGB: 25}
	req := resizeCapacityRequest(have, want)
	if req.CPUs != 2 || req.MemoryMB != 4096 || req.StorageMB[""] != 5*1024 {
		t.Fatalf("unexpected delta: %#v", req)
	}
	// A shrink is not a resize: it asks for nothing.
	if req := resizeCapacityRequest(want, have); req.CPUs != 0 || req.MemoryMB != 0 || len(req.StorageMB) != 0 {
		t.Fatalf("unexpected delta for a shrink: %#v", req)
	}
	_ = kube.NodeInfo{}
}

// I-22: a dequeued slot must not keep reporting the shortfall that parked it
// while the VM is being created, which takes minutes.
func TestQueuedWorkerPublishesProgressBeforeTheVMCreate(t *testing.T) {
	ctx := context.Background()
	pool := testPool(1)
	fc := &fakeCloud{capacity: fullMemory()}
	r := newTestReconciler(t, fc, &fakeKube{}, pool)

	if _, err := r.Reconcile(ctx, requestFor(pool)); err != nil {
		t.Fatalf("reconcile: %v", err)
	}

	fc.capacity.MemoryMB.Used = 0
	var during *infrav1.K8sWorkerPool
	fc.createHook = func() {
		var p infrav1.K8sWorkerPool
		if err := r.Get(ctx, types.NamespacedName{Name: pool.Name, Namespace: pool.Namespace}, &p); err == nil {
			during = p.DeepCopy()
		}
	}
	if _, err := r.Reconcile(ctx, requestFor(pool)); err != nil {
		t.Fatalf("reconcile after freeing capacity: %v", err)
	}

	if during == nil {
		t.Fatal("CreateVM never ran")
	}
	st := during.Status.Nodes[0]
	if st.Phase != infrav1.NodePhaseQueued {
		t.Fatalf("phase during the create = %s, want Queued until the VM exists", st.Phase)
	}
	if !strings.Contains(st.Message, "creating the VM") {
		t.Fatalf("message during the create = %q, want the progress message", st.Message)
	}

	n := getPool(t, r, pool).Status.Nodes[0]
	if n.Phase != infrav1.NodePhaseProvisioning || n.ProviderID == "" {
		t.Fatalf("slot after the create = %#v, want Provisioning with a VM id", n)
	}
}

// A create that fails after a dequeue leaves the slot queued, with the failure
// reported, so the next pass retries it instead of parking it Failed.
func TestQueuedWorkerStaysQueuedWhenTheCreateFails(t *testing.T) {
	ctx := context.Background()
	pool := testPool(1)
	fc := &fakeCloud{capacity: fullMemory()}
	r := newTestReconciler(t, fc, &fakeKube{}, pool)

	if _, err := r.Reconcile(ctx, requestFor(pool)); err != nil {
		t.Fatalf("reconcile: %v", err)
	}

	fc.capacity.MemoryMB.Used = 0
	fc.failAt = 1
	if _, err := r.Reconcile(ctx, requestFor(pool)); err == nil {
		t.Fatal("the reconcile reported success after a failed create")
	}

	n := getPool(t, r, pool).Status.Nodes[0]
	if n.Phase != infrav1.NodePhaseQueued {
		t.Fatalf("phase after a failed create = %s, want Queued", n.Phase)
	}
	if !strings.Contains(n.Message, "the create failed") {
		t.Fatalf("message after a failed create = %q", n.Message)
	}
}

// A scale-out is one batch, and the pre-flight has to charge its own admissions.
// The platform reports measured usage, so a worker admitted a second ago has not
// changed the free figure yet; without counting the batch, every one-worker check
// sees the same free capacity and the whole request is created - which is how
// four workers were started on 2026-11-20 while the page said two fit.
func TestScaleUpChargesTheWholeBatch(t *testing.T) {
	ctx := context.Background()
	pool := testPool(4)
	// Room for exactly three test workers (2 vCPU x 2600 MHz each).
	// A measured platform: the created workers are not booked, exactly like the
	// host usage vSphere reports.
	fc := &fakeCloud{noConsume: true, capacity: cloud.PlatformCapacity{
		Model:         cloud.CapacityPhysical,
		CPUMHzPerCore: 2600,
		CPU:           cloud.ResourceCapacity{Limit: 3 * 2 * 2600, Used: 0},
		MemoryMB:      cloud.ResourceCapacity{Limit: 1 << 20, Used: 0},
	}}
	r := newTestReconciler(t, fc, &fakeKube{}, pool)

	if _, err := r.Reconcile(ctx, requestFor(pool)); err != nil {
		t.Fatalf("reconcile: %v", err)
	}
	got := getPool(t, r, pool)
	if len(fc.created) != 3 {
		for _, n := range got.Status.Nodes {
			t.Logf("node %s %s %q", n.Name, n.Phase, n.Message)
		}
		t.Fatalf("created %d VMs, want 3: the fourth must be queued", len(fc.created))
	}
	var provisioning, queued int
	for _, n := range got.Status.Nodes {
		switch n.Phase {
		case infrav1.NodePhaseProvisioning:
			provisioning++
		case infrav1.NodePhaseQueued:
			queued++
		}
	}
	if provisioning != 3 || queued != 1 {
		t.Fatalf("phases: %d provisioning, %d queued, want 3 and 1", provisioning, queued)
	}
}

// A worker admitted by an earlier pass is still Provisioning and consumes almost
// nothing, so the platform's free figure has not moved. Its charge must be spent
// before the next worker is admitted, or a scale-out keeps adding VMs pass after
// pass while the pane says there is no room.
func TestScaleUpChargesTheWorkersAlreadyStarting(t *testing.T) {
	ctx := context.Background()
	pool := testPool(3)
	// One worker exists and is still starting.
	pool.Status.Nodes = []infrav1.WorkerNodeStatus{{
		Name: "pool-wrk-01", IP: "192.168.100.7", Phase: infrav1.NodePhaseProvisioning,
		ProviderID: "vm-1", ProvisioningStartedAt: &metav1.Time{Time: time.Now()},
	}}
	pool.Status.ObservedReplicas = 1
	fc := &fakeCloud{
		vms:       []cloud.VM{{ID: "vm-1", Name: "pool-wrk-01", IP: "192.168.100.7", CPU: 2, MemoryMB: 4096}},
		noConsume: true,
		capacity: cloud.PlatformCapacity{
			Model: cloud.CapacityPhysical, CPUMHzPerCore: 2600,
			// Room for two charges in total, one of which the starting worker holds.
			CPU:      cloud.ResourceCapacity{Limit: 2 * 2 * 2600, Used: 0},
			MemoryMB: cloud.ResourceCapacity{Limit: 1 << 20, Used: 0},
		},
	}
	r := newTestReconciler(t, fc, &fakeKube{}, pool)

	if _, err := r.Reconcile(ctx, requestFor(pool)); err != nil {
		t.Fatalf("reconcile: %v", err)
	}
	if len(fc.created) != 1 {
		t.Fatalf("created %d VMs, want 1: the starting worker's charge leaves room for one", len(fc.created))
	}
	got := getPool(t, r, pool)
	var provisioning, queued int
	for _, n := range got.Status.Nodes {
		switch n.Phase {
		case infrav1.NodePhaseProvisioning:
			provisioning++
		case infrav1.NodePhaseQueued:
			queued++
		}
	}
	if provisioning != 2 || queued != 1 {
		t.Fatalf("%d provisioning, %d queued, want 2 and 1", provisioning, queued)
	}
}

// A quota books a worker the moment it is created, so the pre-flight charges one
// worker per admission: the workers that exist are already inside the used
// figure. Rebuilding a batch there would charge them twice and halve a scale-out
// the platform can take.
func TestQuotaChargesOneWorkerPerAdmission(t *testing.T) {
	ctx := context.Background()
	pool := testPool(2)
	// Room for exactly two test workers (2 vCPU x 2600 MHz each); the fake books
	// every created VM against the capacity, exactly like a vDC.
	fc := &fakeCloud{capacity: cloud.PlatformCapacity{
		Model:         cloud.CapacityQuota,
		CPUMHzPerCore: 2600,
		CPU:           cloud.ResourceCapacity{Limit: 2 * 2 * 2600},
		MemoryMB:      cloud.ResourceCapacity{Limit: 1 << 20},
	}}
	r := newTestReconciler(t, fc, &fakeKube{}, pool)

	if _, err := r.Reconcile(ctx, requestFor(pool)); err != nil {
		t.Fatalf("reconcile: %v", err)
	}
	if len(fc.created) != 2 {
		t.Fatalf("created %d VMs against a quota with room for two, want 2: "+
			"the platform already booked the first", len(fc.created))
	}
}

// The entitlement rule: our workers are charged in full (their 1:1 entitlement),
// everyone else is measured. Here the neighbours already hold two charges' worth
// of the cluster and our one worker holds the third, so a second worker must be
// queued even though the cluster looks half idle.
func TestEntitlementChargesOurWorkersAgainstTheNeighboursLoad(t *testing.T) {
	ctx := context.Background()
	pool := testPool(2)
	pool.Status.Nodes = []infrav1.WorkerNodeStatus{{
		Name: "pool-wrk-01", IP: "192.168.100.7", Phase: infrav1.NodePhaseReady,
		ProviderID: "vm-1", K8sReady: true,
	}}
	pool.Status.ObservedReplicas = 1
	fc := &fakeCloud{
		vms: []cloud.VM{{
			ID: "vm-1", Name: "pool-wrk-01", IP: "192.168.100.7",
			CPU: 2, MemoryMB: 4096, PoweredOn: true,
			UsedCPUMHz: 1000, UsedMemoryMB: 1024,
		}},
		capacity: cloud.PlatformCapacity{
			Model: cloud.CapacityPhysical, CPUMHzPerCore: 2600,
			// 10.4 GHz of neighbours + 1 GHz of ours measured = 11.4 GHz used.
			CPU:      cloud.ResourceCapacity{Limit: 3 * 2 * 2600, Used: 11400},
			MemoryMB: cloud.ResourceCapacity{Limit: 1 << 20, Used: 1<<20 - 1},
		},
	}
	r := newTestReconciler(t, fc, &fakeKube{nodes: readyNodes("pool-wrk-01")}, pool)

	if _, err := r.Reconcile(ctx, requestFor(pool)); err != nil {
		t.Fatalf("reconcile: %v", err)
	}
	got := getPool(t, r, pool)
	for _, n := range got.Status.Nodes {
		if n.Name == "pool-wrk-02" && n.Phase != infrav1.NodePhaseQueued {
			t.Fatalf("pool-wrk-02 = %s, want Queued: others 10.4 GHz + 2 charges > 15.6 GHz", n.Phase)
		}
	}
	if len(fc.created) != 0 {
		t.Fatalf("created %d VMs, want none: the cluster is already spoken for", len(fc.created))
	}
}

// Seen live on 2026-11-20: the entitlement said no room, and a third worker was
// created anyway. The check charged the new worker plus the ones still *starting*,
// so an already-Ready worker cost nothing - exactly the case that mattered.
func TestEntitlementChargesTheWorkersThatAreAlreadyReady(t *testing.T) {
	ctx := context.Background()
	pool := testPool(3)
	pool.Spec.Flavor.CPUs = 2
	pool.Status.Nodes = []infrav1.WorkerNodeStatus{
		{Name: "pool-wrk-01", IP: "192.168.100.7", Phase: infrav1.NodePhaseReady, ProviderID: "vm-1", K8sReady: true},
		{Name: "pool-wrk-02", IP: "192.168.100.8", Phase: infrav1.NodePhaseReady, ProviderID: "vm-2", K8sReady: true},
	}
	pool.Status.ObservedReplicas = 2
	fc := &fakeCloud{
		vms: []cloud.VM{
			{ID: "vm-1", Name: "pool-wrk-01", IP: "192.168.100.7", CPU: 2, MemoryMB: 4096, PoweredOn: true, UsedCPUMHz: 500, UsedMemoryMB: 512},
			{ID: "vm-2", Name: "pool-wrk-02", IP: "192.168.100.8", CPU: 2, MemoryMB: 4096, PoweredOn: true, UsedCPUMHz: 500, UsedMemoryMB: 512},
		},
		capacity: cloud.PlatformCapacity{
			Model: cloud.CapacityPhysical, CPUMHzPerCore: 2600,
			// Neighbours hold 10.4 GHz, ours measure 1 GHz: room for one more
			// charge (15.6) but not for a third worker's (twice that).
			CPU:      cloud.ResourceCapacity{Limit: 3 * 2 * 2600, Used: 11400},
			MemoryMB: cloud.ResourceCapacity{Limit: 1 << 20, Used: 1 << 19},
		},
	}
	r := newTestReconciler(t, fc, &fakeKube{nodes: readyNodes("pool-wrk-01", "pool-wrk-02")}, pool)

	if _, err := r.Reconcile(ctx, requestFor(pool)); err != nil {
		t.Fatalf("reconcile: %v", err)
	}
	got := getPool(t, r, pool)
	for _, n := range got.Status.Nodes {
		if n.Name == "pool-wrk-03" && n.Phase != infrav1.NodePhaseQueued {
			t.Fatalf("pool-wrk-03 = %s, want Queued: two Ready workers already hold two charges", n.Phase)
		}
	}
	if len(fc.created) != 0 {
		t.Fatalf("created %d VMs, want none", len(fc.created))
	}
}
