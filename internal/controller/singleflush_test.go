package controller

import (
	"context"
	"errors"
	"testing"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"

	infrav1 "github.com/werton13/vcd-k8s-node-controller/api/v1alpha1"
	"github.com/werton13/vcd-k8s-node-controller/internal/cloud"
	"github.com/werton13/vcd-k8s-node-controller/internal/cloudinit"
	"github.com/werton13/vcd-k8s-node-controller/internal/ipam"
	"github.com/werton13/vcd-k8s-node-controller/internal/kube"
)

// newTestReconcilerWithFuncs is newTestReconciler plus client interceptors, so a
// test can count or fail status writes.
func newTestReconcilerWithFuncs(t *testing.T, fc *fakeCloud, fk *fakeKube, funcs interceptor.Funcs, pools ...*infrav1.K8sWorkerPool) *WorkerPoolReconciler {
	t.Helper()
	scheme := runtime.NewScheme()
	if err := clientgoscheme.AddToScheme(scheme); err != nil {
		t.Fatal(err)
	}
	if err := infrav1.AddToScheme(scheme); err != nil {
		t.Fatal(err)
	}
	objs := make([]runtime.Object, 0, len(pools))
	for _, p := range pools {
		objs = append(objs, p)
	}
	builder := fake.NewClientBuilder().
		WithScheme(scheme).
		WithStatusSubresource(&infrav1.K8sWorkerPool{}).
		WithInterceptorFuncs(funcs).
		WithRuntimeObjects(objs...)

	renderer, err := cloudinit.New()
	if err != nil {
		t.Fatal(err)
	}
	return &WorkerPoolReconciler{
		Client:   builder.Build(),
		Scheme:   scheme,
		Cloud:    fc,
		Kube:     fk,
		Renderer: renderer,
		Org:      "myorg",
		VDC:      "My-VDC",
	}
}

// TestReconcileWritesStatusOncePerPass guards the single-flush rule: one write
// on a pass that changes something, none on a steady-state pass. Two writes in
// one pass made the controller race against its own resourceVersion and retry
// forever with "the object has been modified".
func TestReconcileWritesStatusOncePerPass(t *testing.T) {
	ctx := context.Background()
	pool := testPool(2)
	// kubelet reports less than the vCD flavor (kube-reserved), which is what
	// used to make mergeNodes and refreshReadiness flip the value every pass.
	fk := &fakeKube{nodes: []kube.NodeInfo{
		{Name: "pool-wrk-01", Ready: true, KubeletVersion: "v1.35.9", CPU: 1900, MemoryBytes: 3800 << 20},
		{Name: "pool-wrk-02", Ready: true, KubeletVersion: "v1.35.9", CPU: 1900, MemoryBytes: 3800 << 20},
	}}
	fc := &fakeCloud{}
	writes := 0
	funcs := interceptor.Funcs{
		SubResourceUpdate: func(ctx context.Context, c client.Client, sub string, obj client.Object, opts ...client.SubResourceUpdateOption) error {
			writes++
			return c.SubResource(sub).Update(ctx, obj, opts...)
		},
	}
	r := newTestReconcilerWithFuncs(t, fc, fk, funcs, pool)

	if _, err := r.Reconcile(ctx, requestFor(pool)); err != nil {
		t.Fatalf("reconcile: %v", err)
	}
	if writes != 1 {
		t.Fatalf("a pass with changes wrote status %d times, want exactly 1", writes)
	}
	got := getPool(t, r, pool)
	for _, n := range got.Status.Nodes {
		if n.MemoryMB != 3800 {
			t.Fatalf("%s memoryMB = %d, want the Node allocatable 3800, not the 4096 flavor", n.Name, n.MemoryMB)
		}
		if n.CPU != 2 {
			t.Fatalf("%s cpu = %d, want 2 (1900m rounded up)", n.Name, n.CPU)
		}
	}

	rv := got.ResourceVersion
	writes = 0
	if _, err := r.Reconcile(ctx, requestFor(pool)); err != nil {
		t.Fatalf("second reconcile: %v", err)
	}
	if writes != 0 {
		t.Fatalf("steady-state pass wrote status %d times, want 0", writes)
	}
	if after := getPool(t, r, pool); after.ResourceVersion != rv {
		t.Fatalf("status was rewritten without changes (rv %s -> %s)", rv, after.ResourceVersion)
	}
}

// TestReconcileRetriesStatusConflictQuietly covers the losing side of a write
// race: a conflict is not an error, the next pass recomputes from the server.
func TestReconcileRetriesStatusConflictQuietly(t *testing.T) {
	ctx := context.Background()
	pool := testPool(2)
	fk := &fakeKube{nodes: readyNodes("pool-wrk-01", "pool-wrk-02")}
	fc := &fakeCloud{}
	conflict := apierrors.NewConflict(
		schema.GroupResource{Group: "infra.example.com", Resource: "k8sworkerpools"},
		pool.Name, errors.New("the object has been modified"))
	funcs := interceptor.Funcs{
		SubResourceUpdate: func(ctx context.Context, c client.Client, sub string, obj client.Object, opts ...client.SubResourceUpdateOption) error {
			return conflict
		},
	}
	r := newTestReconcilerWithFuncs(t, fc, fk, funcs, pool)

	res, err := r.Reconcile(ctx, requestFor(pool))
	if err != nil {
		t.Fatalf("a status conflict must not be reported as a reconcile error: %v", err)
	}
	if !res.Requeue {
		t.Fatalf("a status conflict must requeue, got %#v", res)
	}
}

// TestMergeNodesKeepsNodeReportedResources pins the single source of truth:
// once a Node exists, its allocatable values must not be overwritten by the
// vCD flavor on the next pass.
func TestMergeNodesKeepsNodeReportedResources(t *testing.T) {
	pool := testPool(1)
	pool.Status.Nodes = []infrav1.WorkerNodeStatus{{
		Name: "pool-wrk-01", IP: "192.168.100.7", Phase: infrav1.NodePhaseReady,
		CPU: 2, MemoryMB: 7841, K8sReady: true,
	}}
	alloc, err := ipam.New(toIPAMPlan(pool.Spec.IPPlan), toIPAMLedger(pool.Status.Ledger))
	if err != nil {
		t.Fatal(err)
	}
	nodes, err := mergeNodes(pool, []cloud.VM{{
		Name: "pool-wrk-01", IP: "192.168.100.7", CPU: 2, MemoryMB: 8192, DiskGB: 50,
	}}, alloc)
	if err != nil {
		t.Fatal(err)
	}
	if nodes[0].MemoryMB != 7841 {
		t.Fatalf("memoryMB = %d, want the Node value 7841, not the flavor 8192", nodes[0].MemoryMB)
	}
	if nodes[0].CPU != 2 {
		t.Fatalf("cpu = %d, want 2", nodes[0].CPU)
	}
	if nodes[0].DiskGB != 50 {
		t.Fatalf("diskGB = %d, want 50 from vCD", nodes[0].DiskGB)
	}
	if !nodes[0].K8sReady || nodes[0].Phase != infrav1.NodePhaseReady {
		t.Fatalf("readiness state was lost by mergeNodes: %#v", nodes[0])
	}
}
