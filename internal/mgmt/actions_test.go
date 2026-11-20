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
