package controller

import (
	"context"
	"fmt"
	"strings"
	"testing"
	"time"

	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"

	infrav1 "github.com/k8s-nodescale/vcd-k8s-node-controller/api/v1alpha1"
	"github.com/k8s-nodescale/vcd-k8s-node-controller/internal/kube"
)

// stoppingPoint puts a worker into the middle of a resize, as a pass that was
// interrupted would have left it.
func stoppingPoint(t *testing.T, r *WorkerPoolReconciler, pool *infrav1.K8sWorkerPool, name string) *infrav1.WorkerNodeStatus {
	t.Helper()
	got := getPool(t, r, pool)
	n := nodeByName(got, name)
	if n == nil {
		t.Fatalf("%s is missing from the status", name)
	}
	started := metav1.NewTime(time.Now().Add(-time.Minute))
	n.Resize = &infrav1.ResizeStatus{Phase: resizePhaseStopped, StartedAt: &started}
	n.Phase = infrav1.NodePhaseResizing
	if err := r.Status().Update(context.Background(), got); err != nil {
		t.Fatalf("seed the resize status: %v", err)
	}
	return n
}

// I-10: a step that a status conflict re-drove finds the VM already resized.
// Reading the VM first must keep it from being powered off and resized again.
func TestResizeSkipsAWorkerThatAlreadyHasTheTargetShape(t *testing.T) {
	pool := testPool(1)
	fc := &fakeCloud{}
	fk := &fakeKube{nodes: readyNodes("pool-wrk-01")}
	r := newTestReconciler(t, fc, fk, pool)
	reconcileOnce(t, r, pool)

	updateSpec(t, r, pool, func(p *infrav1.K8sWorkerPool) { p.Spec.Flavor.CPUs = 4 })
	stoppingPoint(t, r, pool, "pool-wrk-01")
	fc.vms[0].CPU = 4
	fc.vms[0].PoweredOn = true

	offs, resizes := len(fc.poweredOff), len(fc.resizes)
	reconcileOnce(t, r, pool)

	n := nodeByName(getPool(t, r, pool), "pool-wrk-01")
	if n == nil || n.Resize == nil || n.Resize.Phase != resizePhaseBooting {
		t.Fatalf("pool-wrk-01 = %#v, want resize phase %s", n, resizePhaseBooting)
	}
	if len(fc.poweredOff) != offs || len(fc.resizes) != resizes {
		t.Fatalf("powered off %v and resized %v, want neither for a VM that already has the target shape",
			fc.poweredOff[offs:], fc.resizes[resizes:])
	}
}

// I-10: vCD resizes only a stopped VM. A VM that something started again is
// stopped and the pass retries, instead of the slot going Failed.
func TestResizeStopsARunningVMInsteadOfFailingTheSlot(t *testing.T) {
	pool := testPool(1)
	fc := &fakeCloud{}
	fk := &fakeKube{nodes: readyNodes("pool-wrk-01")}
	r := newTestReconciler(t, fc, fk, pool)
	reconcileOnce(t, r, pool)

	updateSpec(t, r, pool, func(p *infrav1.K8sWorkerPool) { p.Spec.Flavor.CPUs = 4 })
	stoppingPoint(t, r, pool, "pool-wrk-01")
	fc.vms[0].PoweredOn = true // something started it again

	offs, resizes := len(fc.poweredOff), len(fc.resizes)
	reconcileOnce(t, r, pool)

	n := nodeByName(getPool(t, r, pool), "pool-wrk-01")
	if n == nil || n.Resize == nil || n.Resize.Phase != resizePhaseStopped {
		t.Fatalf("pool-wrk-01 = %#v, want it back in phase %s", n, resizePhaseStopped)
	}
	if len(fc.poweredOff) != offs+1 {
		t.Fatalf("powered off %v, want one more", fc.poweredOff)
	}
	if len(fc.resizes) != resizes {
		t.Fatalf("resized %v, want no resize attempt on a running VM", fc.resizes[resizes:])
	}
	if fc.vms[0].PoweredOn {
		t.Fatal("the VM is still running")
	}
}

// I-17: a resize that only half applies must not boot the worker. The slot
// parks as Failed with the VM stopped, so the next pass (or a human) sees the
// real shape instead of a worker that reports Ready with the wrong disk.
func TestResizeKeepsTheWorkerOffWhenTheShapeDoesNotTake(t *testing.T) {
	pool := testPool(1)
	fc := &fakeCloud{resizeIgnores: true}
	fk := &fakeKube{nodes: readyNodes("pool-wrk-01")}
	r := newTestReconciler(t, fc, fk, pool)
	reconcileOnce(t, r, pool)

	updateSpec(t, r, pool, func(p *infrav1.K8sWorkerPool) { p.Spec.Flavor.CPUs = 4 })
	reconcileOnce(t, r, pool) // cordon, drain, power off
	reconcileOnce(t, r, pool) // resize reports the old shape

	n := nodeByName(getPool(t, r, pool), "pool-wrk-01")
	if n == nil || n.Resize == nil || n.Resize.Phase != resizePhaseFailed {
		t.Fatalf("pool-wrk-01 = %#v, want a failed resize", n)
	}
	if fc.vms[0].PoweredOn {
		t.Fatal("the VM was powered on although the resize did not apply")
	}
	if len(fc.poweredOn) != 0 {
		t.Fatalf("powered on %v, want the VM to stay off", fc.poweredOn)
	}
}

// I-18: a restart in the middle of CreateVM leaves a powered-off VM with no
// Node and no recorded token. The next pass finishes the create instead of
// letting the provisioning watchdog park the slot.
func TestResumeInterruptedCreateFinishesTheVM(t *testing.T) {
	ctx := context.Background()
	pool := testPool(1)
	fc := &fakeCloud{}
	fk := &fakeKube{} // no Node: the create never got that far
	r := newTestReconciler(t, fc, fk, pool)
	reconcileOnce(t, r, pool)

	got := getPool(t, r, pool)
	n := nodeByName(got, "pool-wrk-01")
	if n == nil || n.ProviderID == "" {
		t.Fatalf("pool-wrk-01 = %#v, want a created VM", n)
	}
	// The create died after the VM existed and before the token was written
	// to the status.
	fc.vms[0].PoweredOn = false
	fc.vms[0].Metadata = map[string]string{"managed-by": "vcd-nodescale", "token-id": "oldtokenid"}
	n.BootstrapTokenID = ""
	n.NodeName = ""
	if err := r.Status().Update(ctx, got); err != nil {
		t.Fatal(err)
	}

	created := len(fc.created)
	reconcileOnce(t, r, pool)

	if len(fc.created) != created+1 {
		t.Fatalf("CreateVM ran %d times, want the interrupted create resumed", len(fc.created)-created)
	}
	if len(fc.vms) != 1 {
		t.Fatalf("%d VMs, want the same VM finished, not a second one", len(fc.vms))
	}
	if !fc.vms[0].PoweredOn {
		t.Fatal("the resumed create did not power the VM on")
	}
	n = nodeByName(getPool(t, r, pool), "pool-wrk-01")
	if n == nil || n.BootstrapTokenID == "" {
		t.Fatalf("pool-wrk-01 = %#v, want the token of the resumed create recorded", n)
	}
	found := false
	for _, id := range fk.revokedTokenIDs {
		if id == "oldtokenid" {
			found = true
		}
	}
	if !found {
		t.Fatalf("revoked token ids = %v, want the stale one revoked", fk.revokedTokenIDs)
	}
	// The pass returned before the readiness evaluation, so it has to state
	// what it is doing. Otherwise the pool keeps the previous pass's message -
	// in the lab, "worker wrk-02 (10.99.96.110) deleted" was still on screen
	// four minutes after that worker was gone and replacements were booting.
	cond := metaReady(getPool(t, r, pool))
	if cond == nil || cond.Reason != "Provisioning" {
		t.Fatalf("Ready condition = %#v, want Provisioning", cond)
	}
	if strings.Contains(cond.Message, "deleted") {
		t.Errorf("the condition still reports the previous step: %q", cond.Message)
	}
}

// I-19: status.nodes[] must show the slot while CreateVM runs, not only after
// the pass returns.
func TestScaleUpPublishesTheSlotBeforeTheVMCreate(t *testing.T) {
	ctx := context.Background()
	pool := testPool(1)
	fc := &fakeCloud{}
	fk := &fakeKube{}
	r := newTestReconciler(t, fc, fk, pool)

	var during *infrav1.K8sWorkerPool
	fc.createHook = func() {
		var p infrav1.K8sWorkerPool
		if err := r.Get(ctx, types.NamespacedName{Name: pool.Name, Namespace: pool.Namespace}, &p); err == nil {
			during = p.DeepCopy()
		}
	}
	reconcileOnce(t, r, pool)

	if during == nil {
		t.Fatal("CreateVM never ran")
	}
	if len(during.Status.Nodes) != 1 {
		t.Fatalf("status during the create = %#v, want the slot already published", during.Status.Nodes)
	}
	st := during.Status.Nodes[0]
	if st.Phase != infrav1.NodePhaseProvisioning || st.Message != "creating the VM" || st.ProviderID != "" {
		t.Fatalf("slot during the create = %#v, want Provisioning/creating the VM with no VM id yet", st)
	}

	// After the pass the slot carries the VM id, so a restart adopts it.
	n := nodeByName(getPool(t, r, pool), "pool-wrk-01")
	if n == nil || n.ProviderID == "" {
		t.Fatalf("pool-wrk-01 = %#v, want the VM id persisted", n)
	}
}

// I-21: a roll spends the same drain budget a scale-down does and then
// escalates, so a PodDisruptionBudget with no possible disruption cannot pin a
// compute change forever.
func TestRolloutEscalatesPastABlockingBudget(t *testing.T) {
	pool := testPool(1)
	fc := &fakeCloud{}
	fk := &fakeKube{
		nodes: readyNodes("pool-wrk-01"),
		drainErr: fmt.Errorf("%w: 1 pod(s) still on pool-wrk-01; blocked by PodDisruptionBudget(s): ingress-nginx-controller",
			kube.ErrDrainIncomplete),
		forceEvictPDBs: []string{"ingress-nginx-controller"},
	}
	r := newTestReconciler(t, fc, fk, pool)
	reconcileOnce(t, r, pool)

	updateSpec(t, r, pool, func(p *infrav1.K8sWorkerPool) { p.Spec.Flavor.CPUs = 4 })
	reconcileOnce(t, r, pool)

	n := nodeByName(getPool(t, r, pool), "pool-wrk-01")
	if n == nil || n.Resize == nil || n.Resize.DrainStartedAt == nil {
		t.Fatalf("the roll did not start a drain with a deadline: %#v", n)
	}
	if len(fk.forcedEvictions) != 0 {
		t.Fatalf("escalated before the drain budget was spent: %v", fk.forcedEvictions)
	}

	updateSpec(t, r, pool, func(p *infrav1.K8sWorkerPool) {
		p.Spec.ScaleDown.DrainTimeout = metav1.Duration{Duration: time.Millisecond}
	})
	reconcileOnce(t, r, pool)

	if len(fk.forcedEvictions) != 1 || fk.forcedEvictions[0] != "pool-wrk-01" {
		t.Fatalf("forced evictions = %v, want [pool-wrk-01]", fk.forcedEvictions)
	}
	n = nodeByName(getPool(t, r, pool), "pool-wrk-01")
	if n == nil || n.Resize == nil || n.Resize.Phase != resizePhaseCordoned {
		t.Fatalf("pool-wrk-01 = %#v, want it still cordoned with a fresh budget", n)
	}
	if !strings.Contains(n.Message, "bypassed PodDisruptionBudget(s): ingress-nginx-controller") {
		t.Fatalf("message does not name the bypassed budget: %q", n.Message)
	}
	if len(fc.poweredOff) != 0 {
		t.Fatalf("the VM was stopped before the pods left: %v", fc.poweredOff)
	}
}

// I-21 opt-out: scaleDown.waitForPDB asks the budget to win, so the roll pauses
// with the worker untouched and names the switch instead of evicting past it.
func TestRolloutWaitForPDBPausesInsteadOfEvicting(t *testing.T) {
	pool := testPool(1)
	pool.Spec.ScaleDown.WaitForPDB = true
	fc := &fakeCloud{}
	fk := &fakeKube{
		nodes: readyNodes("pool-wrk-01"),
		drainErr: fmt.Errorf("%w: 1 pod(s) still on pool-wrk-01; blocked by PodDisruptionBudget(s): ingress-nginx-controller",
			kube.ErrDrainIncomplete),
	}
	r := newTestReconciler(t, fc, fk, pool)
	reconcileOnce(t, r, pool)

	updateSpec(t, r, pool, func(p *infrav1.K8sWorkerPool) { p.Spec.Flavor.CPUs = 4 })
	reconcileOnce(t, r, pool)

	updateSpec(t, r, pool, func(p *infrav1.K8sWorkerPool) {
		p.Spec.ScaleDown.DrainTimeout = metav1.Duration{Duration: time.Millisecond}
	})
	reconcileOnce(t, r, pool)

	if len(fk.forcedEvictions) != 0 {
		t.Fatalf("evicted past a budget the pool asked to keep: %v", fk.forcedEvictions)
	}
	got := getPool(t, r, pool)
	if got.Status.Rollout.Phase != infrav1.RolloutPhasePaused {
		t.Fatalf("rollout phase = %q, want Paused", got.Status.Rollout.Phase)
	}
	n := nodeByName(got, "pool-wrk-01")
	if n == nil || n.Resize == nil || n.Resize.Phase != resizePhaseFailed {
		t.Fatalf("worker = %#v, want a failed resize step", n)
	}
	if !strings.Contains(n.Resize.Message, "waitForPDB") {
		t.Fatalf("message does not name the switch: %q", n.Resize.Message)
	}
	if len(fc.poweredOff) != 0 {
		t.Fatalf("the VM was touched: %v", fc.poweredOff)
	}
}

// A create takes minutes, and the slot is published before the VM exists. Until
// the condition is written the published status still carried the previous
// pass's message: on vSphere the pool said "Converged 1/1" while desired was 2
// and the second clone was already running.
func TestScaleUpPublishesProvisioningWithTheSlot(t *testing.T) {
	pool := testPool(1)
	fc := &fakeCloud{}
	fk := &fakeKube{}
	r := newTestReconciler(t, fc, fk, pool)

	var seen *metav1.Condition
	fc.createHook = func() {
		got := getPool(t, r, pool)
		for i := range got.Status.Conditions {
			if got.Status.Conditions[i].Type == "Ready" {
				c := got.Status.Conditions[i]
				seen = &c
			}
		}
	}
	reconcileOnce(t, r, pool)

	if seen == nil {
		t.Fatal("the pool carries no Ready condition while the VM is being created")
	}
	if seen.Status != metav1.ConditionFalse || seen.Reason != "Provisioning" {
		t.Fatalf("condition during the create = %s/%s, want False/Provisioning", seen.Status, seen.Reason)
	}
	if !strings.Contains(seen.Message, "0/1 workers Ready") || !strings.Contains(seen.Message, "pool-wrk-01") {
		t.Errorf("message = %q, want the counts and the worker being created", seen.Message)
	}
}

// Ready must not say "Converged" while status.rollout says Progressing: a
// worker being replaced stays Ready for most of the roll, so counting Ready
// alone reported a converged pool during a rollout (seen live on vSphere).
func TestPoolConditionReportsARolloutInProgress(t *testing.T) {
	pool := testPool(1)
	pool.Status.ObservedReplicas = 1
	pool.Status.Nodes = []infrav1.WorkerNodeStatus{{
		Name: "pool-wrk-01", IP: "10.0.0.10", Phase: infrav1.NodePhaseResizing,
		Resize: &infrav1.ResizeStatus{Phase: "Cordoned"},
	}}
	pool.Status.Rollout = infrav1.RolloutStatus{Phase: infrav1.RolloutPhaseProgressing, CurrentWorker: "pool-wrk-01"}

	status, reason, msg := evaluatePoolCondition(pool)
	if status != metav1.ConditionFalse || reason != "RollingOut" {
		t.Fatalf("condition = %s/%s, want False/RollingOut", status, reason)
	}
	if !strings.Contains(msg, "rollout in progress") || !strings.Contains(msg, "pool-wrk-01") {
		t.Errorf("message = %q, want the rollout and the worker", msg)
	}

	pool.Status.Rollout.Phase = infrav1.RolloutPhaseIdle
	pool.Status.Nodes[0].Phase = infrav1.NodePhaseReady
	pool.Status.Nodes[0].Resize = nil
	if status, reason, _ := evaluatePoolCondition(pool); status != metav1.ConditionTrue || reason != "Converged" {
		t.Fatalf("condition after the rollout = %s/%s, want True/Converged", status, reason)
	}
}

// I-33: a passing roll returns before the readiness evaluation, so it has to
// write the condition itself. Before this the pool published the message from
// the pass before the roll - live on vSphere it read "Converged 3/3" through a
// whole replacement while status.rollout said Progressing.
func TestARolloutPassPublishesRollingOut(t *testing.T) {
	pool := testPool(1)
	fc := &fakeCloud{}
	fk := &fakeKube{nodes: readyNodes("pool-wrk-01")}
	r := newTestReconciler(t, fc, fk, pool)

	reconcileOnce(t, r, pool)
	if c := meta.FindStatusCondition(getPool(t, r, pool).Status.Conditions, "Ready"); c == nil || c.Reason != "Converged" {
		t.Fatalf("condition before the roll = %#v, want Converged", c)
	}

	updateSpec(t, r, pool, func(p *infrav1.K8sWorkerPool) { p.Spec.Flavor.CPUs = 4 })
	reconcileOnce(t, r, pool) // takes the worker out of service

	got := getPool(t, r, pool)
	c := meta.FindStatusCondition(got.Status.Conditions, "Ready")
	if c == nil {
		t.Fatal("the pool carries no Ready condition during the roll")
	}
	if c.Status != metav1.ConditionFalse || c.Reason != "RollingOut" {
		t.Fatalf("condition during the roll = %s/%s (%q), want False/RollingOut", c.Status, c.Reason, c.Message)
	}
	if !strings.Contains(c.Message, "pool-wrk-01") || !strings.Contains(c.Message, "rollout in progress") {
		t.Errorf("message = %q, want the worker and the roll", c.Message)
	}
	if got.Status.Rollout.Phase != infrav1.RolloutPhaseProgressing {
		t.Fatalf("rollout phase = %q, want Progressing", got.Status.Rollout.Phase)
	}

	// The pass that closes the roll also has to say so: it returns before the
	// readiness evaluation and would otherwise leave "RollingOut" behind
	// for the whole requeue interval.
	reconcileOnce(t, r, pool) // resize and boot
	reconcileOnce(t, r, pool) // back in service
	reconcileOnce(t, r, pool) // roll closes

	got = getPool(t, r, pool)
	c = meta.FindStatusCondition(got.Status.Conditions, "Ready")
	if got.Status.Rollout.Phase != infrav1.RolloutPhaseIdle {
		t.Fatalf("rollout phase = %q, want Idle", got.Status.Rollout.Phase)
	}
	if c == nil || c.Status != metav1.ConditionTrue || c.Reason != "Converged" {
		t.Fatalf("condition after the roll = %#v, want True/Converged", c)
	}
}
