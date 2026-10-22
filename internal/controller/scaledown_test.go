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

	infrav1 "github.com/werton13/vcd-k8s-node-controller/api/v1alpha1"
	"github.com/werton13/vcd-k8s-node-controller/internal/cloud"
	"github.com/werton13/vcd-k8s-node-controller/internal/kube"
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
