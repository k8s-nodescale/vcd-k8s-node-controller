package mgmt

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"

	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"

	infrav1 "github.com/k8s-nodescale/vcd-k8s-node-controller/api/v1alpha1"
)

// TestActionsWriteANarrowPatch is the regression for the defect the UI exposed
// on 2026-11-20: every action replaced the whole object, and because
// spec.scaleDown.minReplicas: 0 was dropped by `omitempty`, the CRD default (1)
// came back. With allowLastWorker set the pool then read InvalidSpec and
// stopped reconciling after a single click.
func TestActionsWriteANarrowPatch(t *testing.T) {
	var updates, patches int
	c := fake.NewClientBuilder().
		WithScheme(testScheme(t)).
		WithObjects(testPool(3)).
		WithStatusSubresource(&infrav1.K8sWorkerPool{}).
		WithInterceptorFuncs(interceptor.Funcs{
			Update: func(ctx context.Context, cl client.WithWatch, obj client.Object, opts ...client.UpdateOption) error {
				if _, ok := obj.(*infrav1.K8sWorkerPool); ok {
					updates++
					return errors.New("a pool must be patched, not replaced")
				}
				return cl.Update(ctx, obj, opts...)
			},
			Patch: func(ctx context.Context, cl client.WithWatch, obj client.Object, patch client.Patch, opts ...client.PatchOption) error {
				patches++
				return cl.Patch(ctx, obj, patch, opts...)
			},
		}).
		Build()

	s := &Server{Client: c}
	if err := s.SetReplicas(context.Background(), "kion-web-dev", 4); err != nil {
		t.Fatalf("set replicas: %v", err)
	}
	if updates != 0 {
		t.Fatalf("an action replaced the object %d time(s)", updates)
	}
	if patches == 0 {
		t.Fatal("an action wrote nothing")
	}

	got := &infrav1.K8sWorkerPool{}
	if err := c.Get(context.Background(), client.ObjectKey{Name: "kion-web-dev"}, got); err != nil {
		t.Fatal(err)
	}
	if got.Spec.Replicas != 4 {
		t.Fatalf("replicas = %d, want 4", got.Spec.Replicas)
	}
	if got.Spec.ScaleDown.MinReplicas != 0 || !got.Spec.ScaleDown.AllowLastWorker {
		t.Fatalf("the action rewrote scaleDown: %#v", got.Spec.ScaleDown)
	}
}

// TestMinReplicasZeroIsSerialised pins the second half of the same defect: a
// zero floor has to survive a JSON round trip, or any write path that
// serialises the spec hands the API server an absent field and gets the
// default 1 back.
func TestMinReplicasZeroIsSerialised(t *testing.T) {
	p := testPool(3)
	raw, err := json.Marshal(p.Spec)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(raw), `"minReplicas":0`) {
		t.Fatalf("an empty pool floor must be in the payload, got %s", raw)
	}
}

// A create that is running is left alone: the watchdog bounds it
// (spec.provisioning.timeout), and deleting a half-finished clone from under the
// controller is not something a stray click should do.
func TestDeleteOfWorkersInProvisioningStaysLocked(t *testing.T) {
	p := testPool(2)
	p.Status.Nodes = []infrav1.WorkerNodeStatus{
		{Name: "kion-web-dev-wrk-01", Phase: infrav1.NodePhaseReady},
		{Name: "kion-web-dev-wrk-02", Phase: infrav1.NodePhaseProvisioning},
	}
	c := testClient(t, p)
	if why := busyReason(p); why == "" {
		t.Fatal("the pool is not busy; the test proves nothing")
	}
	err := (&Server{Client: c}).DeleteWorkers(context.Background(), "kion-web-dev",
		[]string{"kion-web-dev-wrk-02"})
	if !errors.Is(err, ErrInvalid) {
		t.Fatalf("err = %v, want a refused request while the create runs", err)
	}
}

// The states a bounded create ends in are the way out of a worker that never came
// up. Provisioning and Joining are in flight and stay locked: the watchdog bounds
// both.
func TestDeleteOfWorkersIsBypassedByTheLockForTerminalPhases(t *testing.T) {
	for _, phase := range []infrav1.NodePhase{
		infrav1.NodePhaseFailed, infrav1.NodePhaseQueued, infrav1.NodePhaseMissing,
	} {
		t.Run(string(phase), func(t *testing.T) {
			p := testPool(2)
			p.Status.Nodes = []infrav1.WorkerNodeStatus{
				{Name: "kion-web-dev-wrk-01", Phase: infrav1.NodePhaseProvisioning},
				{Name: "kion-web-dev-wrk-02", Phase: phase},
			}
			c := testClient(t, p)
			if why := busyReason(p); why == "" {
				t.Fatal("the pool is not busy; the test proves nothing")
			}
			if err := (&Server{Client: c}).DeleteWorkers(context.Background(), "kion-web-dev",
				[]string{"kion-web-dev-wrk-02"}); err != nil {
				t.Fatalf("deleting a %s worker while busy must be allowed: %v", phase, err)
			}
		})
	}
}

// A Ready worker is a member of the pool: removing it drains, spends the budget,
// and stays behind the lock while something else is running.
func TestDeleteWorkersStaysLockedForAServingWorker(t *testing.T) {
	p := testPool(2)
	p.Status.Nodes = []infrav1.WorkerNodeStatus{
		{Name: "kion-web-dev-wrk-01", Phase: infrav1.NodePhaseReady},
		{Name: "kion-web-dev-wrk-02", Phase: infrav1.NodePhaseProvisioning},
	}
	s := &Server{Client: testClient(t, p)}
	err := s.DeleteWorkers(context.Background(), "kion-web-dev", []string{"kion-web-dev-wrk-01"})
	if !errors.Is(err, ErrInvalid) {
		t.Fatalf("err = %v, want a refused request", err)
	}
}

// A Node that registered but is not Ready is still in flight: the watchdog bounds
// it (spec.provisioning.timeout), so the delete stays locked like any other
// converging worker.
func TestDeleteOfWorkersInJoiningStaysLocked(t *testing.T) {
	p := testPool(2)
	p.Status.Nodes = []infrav1.WorkerNodeStatus{
		{Name: "kion-web-dev-wrk-01", Phase: infrav1.NodePhaseProvisioning},
		{Name: "kion-web-dev-wrk-02", Phase: infrav1.NodePhaseJoining},
	}
	err := (&Server{Client: testClient(t, p)}).DeleteWorkers(context.Background(), "kion-web-dev",
		[]string{"kion-web-dev-wrk-02"})
	if !errors.Is(err, ErrInvalid) {
		t.Fatalf("err = %v, want a refused request while the node is converging", err)
	}
}
