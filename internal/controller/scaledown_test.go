package controller

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"

	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	infrav1 "github.com/k8s-nodescale/vcd-k8s-node-controller/api/v1alpha1"
	"github.com/k8s-nodescale/vcd-k8s-node-controller/internal/cloud"
	"github.com/k8s-nodescale/vcd-k8s-node-controller/internal/kube"
)

func workerVM(ordinal int, ip string) cloud.VM {
	name := fmt.Sprintf("pool-wrk-%02d", ordinal)
	return cloud.VM{
		ID:       "vm-" + name,
		Name:     name,
		IP:       ip,
		CPU:      2,
		MemoryMB: 4096,
		DiskGB:   60,
	}
}

func threeWorkers() []cloud.VM {
	return []cloud.VM{
		workerVM(1, "192.168.100.7"),
		workerVM(2, "192.168.100.8"),
		workerVM(3, "192.168.100.9"),
	}
}

func nodeNames(pool *infrav1.K8sWorkerPool) []string {
	out := make([]string, 0, len(pool.Status.Nodes))
	for _, node := range pool.Status.Nodes {
		out = append(out, node.Name)
	}
	return out
}

func reconcilePasses(t *testing.T, r *WorkerPoolReconciler, pool *infrav1.K8sWorkerPool, passes int) {
	t.Helper()
	for i := 0; i < passes; i++ {
		if _, err := r.Reconcile(context.Background(), requestFor(pool)); err != nil {
			t.Fatalf("pass %d: %v", i, err)
		}
	}
}

func TestReconcileDeletesNamedWorkerAndDecrementsReplicas(t *testing.T) {
	ctx := context.Background()
	pool := testPool(3)
	pool.Spec.ScaleDown = infrav1.ScaleDownPolicy{
		MinReplicas:   1,
		DrainTimeout:  metav1.Duration{Duration: time.Minute},
		DeleteWorkers: []string{"pool-wrk-02"},
	}
	fc := &fakeCloud{vms: threeWorkers()}
	fk := &fakeKube{nodes: readyNodes("pool-wrk-01", "pool-wrk-02", "pool-wrk-03")}
	r := newTestReconciler(t, fc, fk, pool)

	reconcilePasses(t, r, pool, 6)

	got := getPool(t, r, pool)
	names := nodeNames(got)
	if len(names) != 2 || names[0] != "pool-wrk-01" || names[1] != "pool-wrk-03" {
		t.Fatalf("remaining workers = %v, want pool-wrk-01 and pool-wrk-03", names)
	}
	if got.Spec.Replicas != 2 {
		t.Fatalf("spec.replicas = %d, want 2 after a named deletion", got.Spec.Replicas)
	}
	if len(got.Spec.ScaleDown.DeleteWorkers) != 0 {
		t.Fatalf("deleteWorkers not pruned: %v", got.Spec.ScaleDown.DeleteWorkers)
	}
	if len(fc.deleted) != 1 || fc.deleted[0] != "vm-pool-wrk-02" {
		t.Fatalf("deleted VMs = %v, want [vm-pool-wrk-02]", fc.deleted)
	}
	if len(fk.drained) != 1 || fk.drained[0] != "pool-wrk-02" {
		t.Fatalf("drained = %v, want [pool-wrk-02]", fk.drained)
	}
	if len(fk.deletedNodes) != 1 || fk.deletedNodes[0] != "pool-wrk-02" {
		t.Fatalf("deleted Node objects = %v, want [pool-wrk-02]", fk.deletedNodes)
	}
	if got.Status.ObservedReplicas != 2 {
		t.Fatalf("observedReplicas = %d, want 2", got.Status.ObservedReplicas)
	}
	if got.Status.Ledger.Used != nil {
		for _, ip := range got.Status.Ledger.Used {
			if ip == "192.168.100.8" {
				t.Fatalf("address of the deleted worker is still in use: %v", got.Status.Ledger.Used)
			}
		}
	}
	_ = ctx
}

func TestReconcileScaleDownRemovesHighestOrdinal(t *testing.T) {
	pool := testPool(2)
	fc := &fakeCloud{vms: threeWorkers()}
	fk := &fakeKube{nodes: readyNodes("pool-wrk-01", "pool-wrk-02", "pool-wrk-03")}
	r := newTestReconciler(t, fc, fk, pool)

	reconcilePasses(t, r, pool, 6)

	got := getPool(t, r, pool)
	names := nodeNames(got)
	if len(names) != 2 || names[0] != "pool-wrk-01" || names[1] != "pool-wrk-02" {
		t.Fatalf("remaining workers = %v, want the two lowest ordinals", names)
	}
	if len(fc.deleted) != 1 || fc.deleted[0] != "vm-pool-wrk-03" {
		t.Fatalf("deleted VMs = %v, want [vm-pool-wrk-03]", fc.deleted)
	}
	if got.Spec.Replicas != 2 {
		t.Fatalf("spec.replicas = %d, want 2 (scale-down must not touch it)", got.Spec.Replicas)
	}
}

func TestReconcileScaleDownStopsAtMinReplicas(t *testing.T) {
	pool := testPool(1)
	pool.Spec.ScaleDown = infrav1.ScaleDownPolicy{MinReplicas: 2}
	fc := &fakeCloud{vms: threeWorkers()[:2]}
	fk := &fakeKube{nodes: readyNodes("pool-wrk-01", "pool-wrk-02")}
	r := newTestReconciler(t, fc, fk, pool)

	reconcilePasses(t, r, pool, 2)

	got := getPool(t, r, pool)
	if len(got.Status.Nodes) != 2 {
		t.Fatalf("workers = %d, want 2 (minReplicas must stop the scale-down)", len(got.Status.Nodes))
	}
	if len(fc.deleted) != 0 {
		t.Fatalf("deleted VMs = %v, want none", fc.deleted)
	}
	cond := meta.FindStatusCondition(got.Status.Conditions, "Ready")
	if cond == nil || cond.Reason != "MinReplicasProtected" {
		t.Fatalf("condition = %#v, want MinReplicasProtected", cond)
	}
}

func TestReconcileRefusesToDeleteNodeLocalVolumes(t *testing.T) {
	pool := testPool(3)
	pool.Spec.ScaleDown = infrav1.ScaleDownPolicy{MinReplicas: 1, DeleteWorkers: []string{"pool-wrk-03"}}
	fc := &fakeCloud{vms: threeWorkers()}
	fk := &fakeKube{
		nodes:        readyNodes("pool-wrk-01", "pool-wrk-02", "pool-wrk-03"),
		localVolumes: map[string][]string{"pool-wrk-03": {"pv-local-data"}},
	}
	r := newTestReconciler(t, fc, fk, pool)

	reconcilePasses(t, r, pool, 2)

	got := getPool(t, r, pool)
	if len(got.Status.Nodes) != 3 || len(fc.deleted) != 0 {
		t.Fatalf("worker was deleted despite node-local data (deleted=%v)", fc.deleted)
	}
	cond := meta.FindStatusCondition(got.Status.Conditions, "Ready")
	if cond == nil || cond.Reason != "VolumeDataAtRisk" {
		t.Fatalf("condition = %#v, want VolumeDataAtRisk", cond)
	}
	if len(fk.cordoned) != 0 {
		t.Fatalf("node was cordoned before the data-loss guard: %v", fk.cordoned)
	}
}

func TestReconcileForceDeletesNodeWithLocalVolumes(t *testing.T) {
	pool := testPool(3)
	pool.Spec.ScaleDown = infrav1.ScaleDownPolicy{
		MinReplicas:   1,
		Force:         true,
		DeleteWorkers: []string{"pool-wrk-03"},
	}
	fc := &fakeCloud{vms: threeWorkers()}
	fk := &fakeKube{
		nodes:        readyNodes("pool-wrk-01", "pool-wrk-02", "pool-wrk-03"),
		localVolumes: map[string][]string{"pool-wrk-03": {"pv-local-data"}},
	}
	r := newTestReconciler(t, fc, fk, pool)

	reconcilePasses(t, r, pool, 6)

	if len(fc.deleted) != 1 || fc.deleted[0] != "vm-pool-wrk-03" {
		t.Fatalf("deleted VMs = %v, want [vm-pool-wrk-03]", fc.deleted)
	}
}

func TestReconcileDrainTimeoutFailsWithoutDeletingVM(t *testing.T) {
	pool := testPool(3)
	pool.Spec.ScaleDown = infrav1.ScaleDownPolicy{
		MinReplicas:   1,
		DrainTimeout:  metav1.Duration{Duration: time.Millisecond},
		WaitForPDB:    true,
		DeleteWorkers: []string{"pool-wrk-03"},
	}
	fc := &fakeCloud{vms: threeWorkers()}
	fk := &fakeKube{
		nodes:    readyNodes("pool-wrk-01", "pool-wrk-02", "pool-wrk-03"),
		drainErr: fmt.Errorf("%w: 1 pod(s) still on pool-wrk-03; blocked by PodDisruptionBudget(s): web-pdb", kube.ErrDrainIncomplete),
	}
	r := newTestReconciler(t, fc, fk, pool)

	// Pass 1 starts the removal, pass 2 hits the (1ms) drain budget.
	reconcilePasses(t, r, pool, 2)

	got := getPool(t, r, pool)
	if len(fc.deleted) != 0 {
		t.Fatalf("VM deleted after a failed drain: %v", fc.deleted)
	}
	var failed *infrav1.WorkerNodeStatus
	for i := range got.Status.Nodes {
		if got.Status.Nodes[i].Name == "pool-wrk-03" {
			failed = &got.Status.Nodes[i]
		}
	}
	if failed == nil {
		t.Fatal("worker disappeared after a failed drain")
	}
	if failed.Phase != infrav1.NodePhaseFailed {
		t.Fatalf("phase = %q, want Failed", failed.Phase)
	}
	if !strings.Contains(failed.Message, "drain did not finish") {
		t.Fatalf("message does not explain the timeout: %q", failed.Message)
	}
	cond := meta.FindStatusCondition(got.Status.Conditions, "Ready")
	if cond == nil || cond.Reason != "ScaleDownFailed" {
		t.Fatalf("condition = %#v, want ScaleDownFailed", cond)
	}

	// A standing request retries with a fresh budget instead of staying stuck.
	reconcilePasses(t, r, pool, 1)
	got = getPool(t, r, pool)
	for _, node := range got.Status.Nodes {
		if node.Name == "pool-wrk-03" && node.Phase != infrav1.NodePhaseDraining {
			t.Fatalf("phase after retry = %q, want Draining", node.Phase)
		}
	}
}

func TestReconcileDeletesWorkerByNodeAnnotation(t *testing.T) {
	pool := testPool(3)
	pool.Spec.ScaleDown = infrav1.ScaleDownPolicy{MinReplicas: 1}
	fc := &fakeCloud{vms: threeWorkers()}
	nodes := readyNodes("pool-wrk-01", "pool-wrk-02", "pool-wrk-03")
	nodes[2].Annotations = map[string]string{DeleteWorkerAnnotation: "true"}
	fk := &fakeKube{nodes: nodes}
	r := newTestReconciler(t, fc, fk, pool)

	reconcilePasses(t, r, pool, 6)

	got := getPool(t, r, pool)
	if len(fc.deleted) != 1 || fc.deleted[0] != "vm-pool-wrk-03" {
		t.Fatalf("deleted VMs = %v, want [vm-pool-wrk-03]", fc.deleted)
	}
	// The annotation is a rolling replacement: replicas stays at 3, so the
	// pool is expected to create a new worker afterwards.
	if got.Spec.Replicas != 3 {
		t.Fatalf("spec.replicas = %d, want 3", got.Spec.Replicas)
	}
}

func TestWorkerOrdinal(t *testing.T) {
	cases := map[string]int{
		"pool-wrk-01": 1,
		"pool-wrk-12": 12,
		"pool-wrk":    -1,
		"pool":        -1,
	}
	for name, want := range cases {
		if got := workerOrdinal(name); got != want {
			t.Errorf("workerOrdinal(%q) = %d, want %d", name, got, want)
		}
	}
}

func TestToPoolRefCarriesEdge(t *testing.T) {
	pool := testPool(1)
	pool.Spec.IngressPool = infrav1.IngressPool{Type: "nsxv", Name: "p", Port: 30080, Edge: "edge-1"}
	ref := toPoolRef(pool)
	if ref.Edge != "edge-1" || ref.Port != 30080 {
		t.Fatalf("unexpected pool ref: %#v", ref)
	}
}

var _ = errors.Is

// A vCD VM name keeps the prefix case while kubelet registers the Node object
// in lower case. The removal path must resolve the Node name: before the fix it
// passed the VM name to the Kubernetes API, every call answered 404 or matched
// nothing, and the worker was removed with its guest still running.
func TestReconcileDeletionUsesNodeNameAndStopsGuestFirst(t *testing.T) {
	pool := testPool(2)
	pool.Spec.NamePrefix = "POOL"
	vms := []cloud.VM{
		{ID: "vm-POOL-wrk-01", Name: "POOL-wrk-01", IP: "192.168.100.7", CPU: 2, MemoryMB: 4096, DiskGB: 60},
		{ID: "vm-POOL-wrk-02", Name: "POOL-wrk-02", IP: "192.168.100.8", CPU: 2, MemoryMB: 4096, DiskGB: 60},
		{ID: "vm-POOL-wrk-03", Name: "POOL-wrk-03", IP: "192.168.100.9", CPU: 2, MemoryMB: 4096, DiskGB: 60},
	}
	var order []string
	fc := &fakeCloud{vms: vms, events: &order}
	fk := &fakeKube{
		nodes:  readyNodes("pool-wrk-01", "pool-wrk-02", "pool-wrk-03"),
		events: &order,
	}
	r := newTestReconciler(t, fc, fk, pool)

	reconcilePasses(t, r, pool, 6)

	got := getPool(t, r, pool)
	if names := nodeNames(got); len(names) != 2 || got.Spec.Replicas != 2 {
		t.Fatalf("workers = %v, replicas = %d; want 2 and 2", names, got.Spec.Replicas)
	}
	for _, want := range []struct {
		what  string
		calls []string
		node  string
	}{
		{"cordon", fk.cordoned, "pool-wrk-03"},
		{"drain", fk.drained, "pool-wrk-03"},
		{"node delete", fk.deletedNodes, "pool-wrk-03"},
	} {
		if len(want.calls) != 1 || want.calls[0] != want.node {
			t.Fatalf("%s called with %v, want [%s] (the Kubernetes Node name, not the vCD VM name)",
				want.what, want.calls, want.node)
		}
	}
	if len(fc.poweredOff) != 1 || fc.poweredOff[0] != "vm-POOL-wrk-03" {
		t.Fatalf("powered off = %v, want [vm-POOL-wrk-03]", fc.poweredOff)
	}

	// The guest must be off before its Node object goes away, and the VM
	// removal comes last: a Node deleted under a live kubelet re-registers
	// within seconds and is left behind as a NotReady corpse.
	var seq []string
	for _, e := range order {
		if strings.Contains(e, "wrk-03") {
			seq = append(seq, e)
		}
	}
	want := []string{
		"cloud:poweroff vm-POOL-wrk-03",
		"kube:deletenode pool-wrk-03",
		"cloud:deletevm vm-POOL-wrk-03",
	}
	if len(seq) != len(want) {
		t.Fatalf("removal sequence = %v, want %v", seq, want)
	}
	for i := range want {
		if seq[i] != want[i] {
			t.Fatalf("removal sequence = %v, want %v", seq, want)
		}
	}

	// A converged pool must not touch the worker again.
	reconcilePasses(t, r, pool, 2)
	if len(fk.cordoned) != 1 || len(fk.deletedNodes) != 1 || len(fc.poweredOff) != 1 {
		t.Fatalf("removal repeated on a converged pool: cordon=%v nodes=%v off=%v",
			fk.cordoned, fk.deletedNodes, fc.poweredOff)
	}
}

// A count-based removal that the operator withdraws after the worker was
// cordoned has to bring the worker back: raising spec.replicas again must not
// leave a cordoned, Failed slot that the pool counts as observed and then
// replaces with a fresh worker.
func TestReconcileWithdrawnScaleDownReturnsTheWorker(t *testing.T) {
	pool := testPool(3)
	pool.Spec.ScaleDown = infrav1.ScaleDownPolicy{
		MinReplicas:  1,
		DrainTimeout: metav1.Duration{Duration: time.Millisecond},
		WaitForPDB:   true,
	}
	fc := &fakeCloud{vms: threeWorkers()}
	fk := &fakeKube{
		nodes:    readyNodes("pool-wrk-01", "pool-wrk-02", "pool-wrk-03"),
		drainErr: fmt.Errorf("%w: 1 pod(s) still on pool-wrk-03; blocked by PodDisruptionBudget(s): web-pdb", kube.ErrDrainIncomplete),
	}
	r := newTestReconciler(t, fc, fk, pool)

	// Scale in to two: wrk-03 is cordoned and its drain runs into the PDB.
	updateSpec(t, r, pool, func(p *infrav1.K8sWorkerPool) { p.Spec.Replicas = 2 })
	reconcilePasses(t, r, pool, 3)

	got := getPool(t, r, pool)
	n := nodeByName(got, "pool-wrk-03")
	if n == nil || n.Phase != infrav1.NodePhaseFailed {
		t.Fatalf("wrk-03 = %#v, want a Failed drain", n)
	}
	if len(fc.deleted) != 0 {
		t.Fatalf("deleted VMs = %v, want none after a failed drain", fc.deleted)
	}
	if len(fk.cordoned) != 1 {
		t.Fatalf("cordoned = %v, want the drain to have started", fk.cordoned)
	}

	// Withdraw the removal: the parked worker must serve again instead of being
	// replaced by a new one.
	updateSpec(t, r, pool, func(p *infrav1.K8sWorkerPool) { p.Spec.Replicas = 3 })
	reconcileOnce(t, r, pool)

	got = getPool(t, r, pool)
	n = nodeByName(got, "pool-wrk-03")
	if n == nil {
		t.Fatal("the withdrawn worker disappeared")
	}
	if n.Phase != infrav1.NodePhaseReady || n.DeleteStartedAt != nil {
		t.Fatalf("phase=%q deleteStartedAt=%v, want Ready and no removal state", n.Phase, n.DeleteStartedAt)
	}
	if len(fk.uncordoned) != 1 || fk.uncordoned[0] != "pool-wrk-03" {
		t.Fatalf("uncordoned = %v, want [pool-wrk-03]", fk.uncordoned)
	}

	reconcileOnce(t, r, pool)
	got = getPool(t, r, pool)
	if len(got.Status.Nodes) != 3 {
		t.Fatalf("workers = %d, want the three original slots", len(got.Status.Nodes))
	}
	if len(fc.created) != 0 {
		t.Fatalf("created = %v, want no replacement worker", fc.created)
	}
	if cond := metaReady(got); cond == nil || cond.Status != metav1.ConditionTrue {
		t.Fatalf("Ready condition = %#v, want True", cond)
	}
}

// The count is a standing request: once the blocker is gone the parked worker
// is retried without the operator withdrawing and re-asking for the scale-in.
func TestReconcileRetriesAFailedDrainWhileTheCountStillAsksForIt(t *testing.T) {
	pool := testPool(3)
	pool.Spec.ScaleDown = infrav1.ScaleDownPolicy{
		MinReplicas:  1,
		DrainTimeout: metav1.Duration{Duration: time.Millisecond},
		WaitForPDB:   true,
	}
	fc := &fakeCloud{vms: threeWorkers()}
	fk := &fakeKube{
		nodes:    readyNodes("pool-wrk-01", "pool-wrk-02", "pool-wrk-03"),
		drainErr: fmt.Errorf("%w: 1 pod(s) still on pool-wrk-03; blocked by PodDisruptionBudget(s): web-pdb", kube.ErrDrainIncomplete),
	}
	r := newTestReconciler(t, fc, fk, pool)

	updateSpec(t, r, pool, func(p *infrav1.K8sWorkerPool) { p.Spec.Replicas = 2 })
	reconcilePasses(t, r, pool, 3)
	if n := nodeByName(getPool(t, r, pool), "pool-wrk-03"); n == nil || n.Phase != infrav1.NodePhaseFailed {
		t.Fatalf("wrk-03 = %#v, want a Failed drain", n)
	}

	// The PDB is gone; nobody has to touch the pool for it to converge.
	fk.drainErr = nil
	reconcilePasses(t, r, pool, 8)

	got := getPool(t, r, pool)
	if len(fc.deleted) != 1 || fc.deleted[0] != "vm-pool-wrk-03" {
		t.Fatalf("deleted VMs = %v, want [vm-pool-wrk-03]", fc.deleted)
	}
	if got.Spec.Replicas != 2 || len(got.Status.Nodes) != 2 {
		t.Fatalf("replicas=%d nodes=%d, want 2 and 2", got.Spec.Replicas, len(got.Status.Nodes))
	}
	if cond := metaReady(got); cond == nil || cond.Status != metav1.ConditionTrue {
		t.Fatalf("Ready condition = %#v, want True", cond)
	}
}

// A budget that can never allow a disruption must not pin a worker forever.
// Once drainTimeout is spent the pods left on the node are deleted through the
// API, the budgets behind them are named in the status, and the removal goes
// on with a fresh budget. Seen on the dev cluster with ingress-nginx (one
// replica, minAvailable 1) on the last worker of the pool.
func TestReconcileDrainTimeoutEvictsPastABlockingBudget(t *testing.T) {
	pool := testPool(3)
	pool.Spec.ScaleDown = infrav1.ScaleDownPolicy{
		MinReplicas:   1,
		DrainTimeout:  metav1.Duration{Duration: time.Millisecond},
		DeleteWorkers: []string{"pool-wrk-03"},
	}
	fc := &fakeCloud{vms: threeWorkers()}
	fk := &fakeKube{
		nodes:          readyNodes("pool-wrk-01", "pool-wrk-02", "pool-wrk-03"),
		drainErr:       fmt.Errorf("%w: 1 pod(s) still on pool-wrk-03; blocked by PodDisruptionBudget(s): ingress-nginx-controller", kube.ErrDrainIncomplete),
		forceEvictPDBs: []string{"ingress-nginx-controller"},
	}
	r := newTestReconciler(t, fc, fk, pool)

	// Pass 1 starts the removal, pass 2 spends the (1ms) drain budget.
	reconcilePasses(t, r, pool, 2)

	if len(fk.forcedEvictions) != 1 || fk.forcedEvictions[0] != "pool-wrk-03" {
		t.Fatalf("forced evictions = %v, want [pool-wrk-03]", fk.forcedEvictions)
	}
	got := getPool(t, r, pool)
	n := nodeByName(got, "pool-wrk-03")
	if n == nil || n.Phase != infrav1.NodePhaseDraining {
		t.Fatalf("wrk-03 = %#v, want Draining with a fresh budget", n)
	}
	if n.DeleteStartedAt == nil {
		t.Fatal("the escalated drain did not restart the budget")
	}
	if !strings.Contains(n.Message, "bypassed PodDisruptionBudget(s): ingress-nginx-controller") {
		t.Fatalf("message does not name the bypassed budget: %q", n.Message)
	}
	if len(fc.deleted) != 0 {
		t.Fatalf("VM deleted before the pods left: %v", fc.deleted)
	}
}

// When the escalation finds nothing left to delete the pass must fall through
// to the regular drain instead of restarting the budget forever.
func TestReconcileDrainEscalationWithNothingLeftMovesOn(t *testing.T) {
	pool := testPool(3)
	pool.Spec.ScaleDown = infrav1.ScaleDownPolicy{
		MinReplicas:   1,
		DrainTimeout:  metav1.Duration{Duration: time.Millisecond},
		DeleteWorkers: []string{"pool-wrk-03"},
	}
	fc := &fakeCloud{vms: threeWorkers()}
	fk := &fakeKube{
		nodes:          readyNodes("pool-wrk-01", "pool-wrk-02", "pool-wrk-03"),
		forceEvictPods: []string{},
	}
	r := newTestReconciler(t, fc, fk, pool)

	reconcilePasses(t, r, pool, 2)

	if len(fk.forcedEvictions) != 1 {
		t.Fatalf("forced evictions = %v, want the pass to try", fk.forcedEvictions)
	}
	if len(fk.drained) != 1 || fk.drained[0] != "pool-wrk-03" {
		t.Fatalf("drained = %v, want [pool-wrk-03]", fk.drained)
	}
	n := nodeByName(getPool(t, r, pool), "pool-wrk-03")
	if n == nil || n.Phase != infrav1.NodePhaseDeleting {
		t.Fatalf("wrk-03 = %#v, want Deleting", n)
	}
}
