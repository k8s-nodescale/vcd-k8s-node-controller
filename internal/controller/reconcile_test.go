package controller

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	infrav1 "github.com/werton13/vcd-k8s-node-controller/api/v1alpha1"
	"github.com/werton13/vcd-k8s-node-controller/internal/cloud"
	"github.com/werton13/vcd-k8s-node-controller/internal/cloudinit"
	"github.com/werton13/vcd-k8s-node-controller/internal/kube"
)

type fakeCloud struct {
	vms             []cloud.VM
	created         []cloud.CreateVMSpec
	calls           int
	failAt          int
	failAfterCreate bool
}

func (f *fakeCloud) ListWorkerVMs(ctx context.Context, vapp string, selector map[string]string) ([]cloud.VM, error) {
	out := make([]cloud.VM, len(f.vms))
	copy(out, f.vms)
	return out, nil
}

func (f *fakeCloud) CreateVM(ctx context.Context, spec cloud.CreateVMSpec) (cloud.VM, error) {
	f.calls++
	if f.failAt > 0 && f.calls == f.failAt {
		if f.failAfterCreate {
			// Simulates a VM that survived a failed power-on/cleanup.
			f.vms = append(f.vms, cloud.VM{
				ID: "vm-" + spec.Name, Name: spec.Name, IP: spec.IP,
				CPU: spec.CPUs, MemoryMB: spec.MemoryMB,
			})
		}
		return cloud.VM{}, errors.New("injected create failure")
	}
	vm := cloud.VM{
		ID: "vm-" + spec.Name, Name: spec.Name, IP: spec.IP,
		CPU: spec.CPUs, MemoryMB: spec.MemoryMB, DiskGB: 10,
	}
	f.vms = append(f.vms, vm)
	f.created = append(f.created, spec)
	return vm, nil
}

func (f *fakeCloud) DeleteVM(ctx context.Context, id string) error { return nil }
func (f *fakeCloud) AddPoolMember(ctx context.Context, pool cloud.PoolRef, ip string) error {
	return nil
}
func (f *fakeCloud) RemovePoolMember(ctx context.Context, pool cloud.PoolRef, ip string) error {
	return nil
}

type fakeKube struct {
	nodes         []kube.NodeInfo
	taints        map[string][]corev1.Taint
	labels        map[string]map[string]string
	revokedTokens []string
}

func (f *fakeKube) CreateBootstrapToken(ctx context.Context, ttl time.Duration) (string, error) {
	return "abcdef.0123456789abcdef", nil
}
func (f *fakeKube) RevokeBootstrapToken(ctx context.Context, token string) error {
	f.revokedTokens = append(f.revokedTokens, token)
	return nil
}
func (f *fakeKube) CAHash(ctx context.Context) (string, error) { return "sha256:0011", nil }
func (f *fakeKube) ListNodes(ctx context.Context) ([]kube.NodeInfo, error) {
	return f.nodes, nil
}
func (f *fakeKube) WaitReady(ctx context.Context, name string, timeout time.Duration) error {
	return nil
}
func (f *fakeKube) Drain(ctx context.Context, name string, timeout time.Duration) error { return nil }
func (f *fakeKube) DeleteNode(ctx context.Context, name string) error                   { return nil }
func (f *fakeKube) SetTaints(ctx context.Context, name string, taints []corev1.Taint) error {
	if f.taints == nil {
		f.taints = map[string][]corev1.Taint{}
	}
	f.taints[name] = taints
	return nil
}

func (f *fakeKube) SetLabels(ctx context.Context, name string, labels map[string]string) error {
	if f.labels == nil {
		f.labels = map[string]map[string]string{}
	}
	f.labels[name] = labels
	return nil
}

func readyNodes(names ...string) []kube.NodeInfo {
	out := make([]kube.NodeInfo, 0, len(names))
	for _, name := range names {
		out = append(out, kube.NodeInfo{
			Name: name, Ready: true, KubeletVersion: "v1.35.9",
			CPU: 2000, MemoryBytes: 4 << 30,
		})
	}
	return out
}

func testPool(replicas int32) *infrav1.K8sWorkerPool {
	return &infrav1.K8sWorkerPool{
		ObjectMeta: metav1.ObjectMeta{
			Name:       "default",
			Namespace:  "vcd-nodescale-system",
			Generation: 1,
		},
		Spec: infrav1.K8sWorkerPoolSpec{
			Replicas:   replicas,
			NamePrefix: "pool",
			Bootstrap: infrav1.BootstrapConfig{
				OSUser:       "ubuntu",
				NICName:      "ens192",
				DNS:          []string{"192.0.2.53"},
				K8sVersion:   "1.35.9-1.1",
				K8sMinorRepo: "v1.35",
			},
			Flavor: infrav1.NodeFlavor{
				CPUs:     2,
				MemoryMB: 4096,
				Template: "ubuntu",
				Disks:    []infrav1.DiskSpec{{SizeGB: 10, Mount: "/var", BusNumber: 1, UnitNumber: 0}},
			},
			VCD: infrav1.VCDRef{
				Org: "myorg", VDC: "My-VDC", VApp: "vapp",
				OrgNetwork: "net", Catalog: "Templates",
			},
			IPPlan: infrav1.IPPlan{
				CIDR: "192.168.100.0/24", MNodeOffset: 4, WNodeOffset: 7,
				DVMOffset: -3, KubeAPILB: 2, IngressLB: 3, MasterCount: 3,
			},
			Join: infrav1.JoinConfig{
				Endpoint: "192.168.100.2:6443",
				Labels:   map[string]string{"node-role.kubernetes.io/worker": ""},
			},
		},
	}
}

func newTestReconciler(t *testing.T, fc *fakeCloud, fk *fakeKube, pools ...*infrav1.K8sWorkerPool) *WorkerPoolReconciler {
	t.Helper()
	scheme := runtime.NewScheme()
	if err := clientgoscheme.AddToScheme(scheme); err != nil {
		t.Fatal(err)
	}
	if err := infrav1.AddToScheme(scheme); err != nil {
		t.Fatal(err)
	}
	builder := fake.NewClientBuilder().
		WithScheme(scheme).
		WithStatusSubresource(&infrav1.K8sWorkerPool{})
	objs := make([]runtime.Object, 0, len(pools))
	for _, p := range pools {
		objs = append(objs, p)
	}
	builder = builder.WithRuntimeObjects(objs...)

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

func requestFor(pool *infrav1.K8sWorkerPool) ctrl.Request {
	return ctrl.Request{NamespacedName: types.NamespacedName{Name: pool.Name, Namespace: pool.Namespace}}
}

func getPool(t *testing.T, r *WorkerPoolReconciler, pool *infrav1.K8sWorkerPool) *infrav1.K8sWorkerPool {
	t.Helper()
	var got infrav1.K8sWorkerPool
	if err := r.Get(context.Background(), types.NamespacedName{Name: pool.Name, Namespace: pool.Namespace}, &got); err != nil {
		t.Fatal(err)
	}
	return &got
}

func TestReconcileScalesUpAndIsIdempotent(t *testing.T) {
	ctx := context.Background()
	pool := testPool(2)
	fc := &fakeCloud{}
	fk := &fakeKube{nodes: readyNodes("pool-wrk-01", "pool-wrk-02")}
	r := newTestReconciler(t, fc, fk, pool)

	if _, err := r.Reconcile(ctx, requestFor(pool)); err != nil {
		t.Fatalf("reconcile: %v", err)
	}
	if len(fc.created) != 2 {
		t.Fatalf("created %d VMs, want 2", len(fc.created))
	}
	if got := fc.created[0].Name; got != "pool-wrk-01" {
		t.Fatalf("first VM name = %q, want pool-wrk-01", got)
	}
	if got := fc.created[1].Name; got != "pool-wrk-02" {
		t.Fatalf("second VM name = %q, want pool-wrk-02", got)
	}
	if got := fc.created[0].IP; got != "192.168.100.7" {
		t.Fatalf("first IP = %q, want 192.168.100.7", got)
	}
	if strings.Contains(fc.created[0].UserData, "--node-labels") {
		t.Error("user-data must not pass --node-labels to kubeadm join")
	}
	if !strings.Contains(fc.created[0].UserData, "kubeadm join") {
		t.Error("user-data is missing the join command")
	}

	got := getPool(t, r, pool)
	if got.Status.ObservedReplicas != 2 || len(got.Status.Nodes) != 2 {
		t.Fatalf("observed=%d nodes=%d, want 2/2", got.Status.ObservedReplicas, len(got.Status.Nodes))
	}
	if len(got.Status.Ledger.Used) != 2 {
		t.Fatalf("ledger has %d used addresses, want 2", len(got.Status.Ledger.Used))
	}
	cond := metaReady(got)
	if cond == nil || cond.Status != metav1.ConditionTrue || cond.Reason != "Converged" {
		t.Fatalf("unexpected Ready condition: %#v", cond)
	}
	if got.Status.Nodes[0].Phase != infrav1.NodePhaseReady || !got.Status.Nodes[0].K8sReady {
		t.Fatalf("node was not marked Ready: %#v", got.Status.Nodes[0])
	}
	if got.Status.Nodes[0].KubeletVersion != "v1.35.9" {
		t.Fatalf("kubelet version not populated: %#v", got.Status.Nodes[0])
	}
	if got.Status.Nodes[0].CPU != 2 || got.Status.Nodes[0].MemoryMB != 4096 {
		t.Fatalf("resources not taken from the Node: %#v", got.Status.Nodes[0])
	}

	version := got.ResourceVersion
	if _, err := r.Reconcile(ctx, requestFor(pool)); err != nil {
		t.Fatalf("second reconcile: %v", err)
	}
	if len(fc.created) != 2 {
		t.Fatalf("second reconcile created %d more VMs", len(fc.created)-2)
	}
	if after := getPool(t, r, pool); after.ResourceVersion != version {
		t.Fatalf("status was rewritten without changes (rv %s -> %s)", version, after.ResourceVersion)
	}
}

func TestReconcileReleasesAddressWhenCreateFailed(t *testing.T) {
	ctx := context.Background()
	pool := testPool(2)
	fc := &fakeCloud{failAt: 2}
	r := newTestReconciler(t, fc, &fakeKube{}, pool)

	if _, err := r.Reconcile(ctx, requestFor(pool)); err == nil {
		t.Fatal("expected the injected create failure")
	}
	got := getPool(t, r, pool)
	if got.Status.ObservedReplicas != 1 {
		t.Fatalf("observed = %d, want 1", got.Status.ObservedReplicas)
	}
	if len(got.Status.Ledger.Used) != 1 {
		t.Fatalf("ledger kept %d addresses, want 1 (the failed address must be released)", len(got.Status.Ledger.Used))
	}

	fc.failAt = 0
	if _, err := r.Reconcile(ctx, requestFor(pool)); err != nil {
		t.Fatalf("retry: %v", err)
	}
	if len(fc.created) != 2 {
		t.Fatalf("created %d VMs, want 2", len(fc.created))
	}
	if name := fc.created[1].Name; name != "pool-wrk-02" {
		t.Fatalf("retry created %q, want pool-wrk-02", name)
	}
}

func TestReconcileReservesAddressWhenFailedVMExists(t *testing.T) {
	ctx := context.Background()
	pool := testPool(2)
	fc := &fakeCloud{failAt: 1, failAfterCreate: true}
	r := newTestReconciler(t, fc, &fakeKube{}, pool)

	if _, err := r.Reconcile(ctx, requestFor(pool)); err == nil {
		t.Fatal("expected the injected create failure")
	}
	got := getPool(t, r, pool)
	if len(got.Status.Ledger.Used) != 1 {
		t.Fatalf("ledger has %d addresses, want the failed VM's address to stay reserved", len(got.Status.Ledger.Used))
	}

	fc.failAt = 0
	if _, err := r.Reconcile(ctx, requestFor(pool)); err != nil {
		t.Fatalf("reconcile after failure: %v", err)
	}
	// The orphaned VM is adopted, so only one new VM is created and the
	// reserved address is never handed out again.
	if len(fc.created) != 1 {
		t.Fatalf("created %d VMs after adopting the orphan, want 1", len(fc.created))
	}
	if name := fc.created[0].Name; name != "pool-wrk-02" {
		t.Fatalf("created %q, want pool-wrk-02", name)
	}
	got = getPool(t, r, pool)
	if got.Status.ObservedReplicas != 2 {
		t.Fatalf("observed = %d, want 2", got.Status.ObservedReplicas)
	}
	seen := map[string]bool{}
	for _, n := range got.Status.Nodes {
		if seen[n.IP] {
			t.Fatalf("duplicate address %s in status", n.IP)
		}
		seen[n.IP] = true
	}
}

func TestReconcileRejectsMismatchedOrg(t *testing.T) {
	ctx := context.Background()
	pool := testPool(1)
	pool.Spec.VCD.Org = "someone-elses-org"
	fc := &fakeCloud{}
	r := newTestReconciler(t, fc, &fakeKube{}, pool)

	if _, err := r.Reconcile(ctx, requestFor(pool)); err != nil {
		t.Fatalf("reconcile: %v", err)
	}
	if len(fc.created) != 0 {
		t.Fatalf("created %d VMs for an invalid spec", len(fc.created))
	}
	cond := metaReady(getPool(t, r, pool))
	if cond == nil || cond.Reason != "InvalidSpec" {
		t.Fatalf("unexpected Ready condition: %#v", cond)
	}
}

func TestBuildJoinCommand(t *testing.T) {
	cmd, err := buildJoinCommand("192.168.100.2:6443", "tok", "sha256:aa")
	if err != nil {
		t.Fatal(err)
	}
	want := "kubeadm join 192.168.100.2:6443 --token tok --discovery-token-ca-cert-hash sha256:aa"
	if cmd != want {
		t.Fatalf("got %q, want %q", cmd, want)
	}
	// kubeadm join has no --node-labels flag in 1.35; labels are patched later.
	if strings.Contains(cmd, "--node-labels") {
		t.Fatalf("join command must not use --node-labels: %q", cmd)
	}

	for _, tc := range []struct{ name, endpoint string }{
		{"no port", "192.168.100.2"},
		{"non-numeric port", "192.168.100.2:kube"},
		{"empty", ""},
	} {
		if _, err := buildJoinCommand(tc.endpoint, "tok", "sha256:aa"); err == nil {
			t.Errorf("%s: expected an error", tc.name)
		}
	}
}

func TestWorkerName(t *testing.T) {
	for _, tc := range []struct {
		ordinal int
		want    string
	}{{1, "pool-wrk-01"}, {12, "pool-wrk-12"}, {100, "pool-wrk-100"}} {
		if got := workerName("pool", tc.ordinal); got != tc.want {
			t.Fatalf("workerName(%d) = %q, want %q", tc.ordinal, got, tc.want)
		}
	}
}

func TestMapNodeToPoolsFiltersByPrefix(t *testing.T) {
	ctx := context.Background()
	pool := testPool(0)
	other := testPool(0)
	other.Name = "other"
	other.Namespace = "default"
	other.Spec.NamePrefix = "other"

	r := newTestReconciler(t, &fakeCloud{}, &fakeKube{}, pool, other)

	reqs := r.mapNodeToPools(ctx, &corev1.Node{ObjectMeta: metav1.ObjectMeta{Name: "pool-wrk-03"}})
	if len(reqs) != 1 || reqs[0].Name != "default" {
		t.Fatalf("unexpected requests for pool node: %#v", reqs)
	}
	if reqs := r.mapNodeToPools(ctx, &corev1.Node{ObjectMeta: metav1.ObjectMeta{Name: "unrelated-node"}}); len(reqs) != 0 {
		t.Fatalf("unrelated node enqueued %d pools", len(reqs))
	}
}

func metaReady(pool *infrav1.K8sWorkerPool) *metav1.Condition {
	for i := range pool.Status.Conditions {
		if pool.Status.Conditions[i].Type == "Ready" {
			return &pool.Status.Conditions[i]
		}
	}
	return nil
}

func TestReconcileTracksNodeReadiness(t *testing.T) {
	ctx := context.Background()
	pool := testPool(2)
	fc := &fakeCloud{vms: []cloud.VM{
		{ID: "vm-1", Name: "pool-wrk-01", IP: "192.168.100.7", CPU: 2, MemoryMB: 4096},
		{ID: "vm-2", Name: "pool-wrk-02", IP: "192.168.100.8", CPU: 2, MemoryMB: 4096},
	}}
	fk := &fakeKube{nodes: []kube.NodeInfo{
		{Name: "pool-wrk-01", Ready: true, KubeletVersion: "v1.35.9", CPU: 3500, MemoryBytes: 8 << 30},
		{Name: "pool-wrk-02", Ready: false},
	}}
	r := newTestReconciler(t, fc, fk, pool)

	res, err := r.Reconcile(ctx, requestFor(pool))
	if err != nil {
		t.Fatalf("reconcile: %v", err)
	}
	if len(fc.created) != 0 {
		t.Fatalf("created %d VMs, want 0", len(fc.created))
	}
	got := getPool(t, r, pool)
	if got.Status.Nodes[0].Phase != infrav1.NodePhaseReady || !got.Status.Nodes[0].K8sReady {
		t.Fatalf("wrk-01 phase = %s, want Ready", got.Status.Nodes[0].Phase)
	}
	if got.Status.Nodes[1].Phase != infrav1.NodePhaseJoining || got.Status.Nodes[1].K8sReady {
		t.Fatalf("wrk-02 phase = %s, want Joining", got.Status.Nodes[1].Phase)
	}
	if got.Status.Nodes[0].CPU != 4 || got.Status.Nodes[0].MemoryMB != 8192 {
		t.Fatalf("allocatable resources not copied: %#v", got.Status.Nodes[0])
	}
	cond := metaReady(got)
	if cond == nil || cond.Status != metav1.ConditionFalse || cond.Reason != "Progressing" {
		t.Fatalf("unexpected condition: %#v", cond)
	}
	if cond.Message != "1/2 workers Ready" {
		t.Fatalf("message = %q", cond.Message)
	}
	if res.RequeueAfter != requeueProgressing {
		t.Fatalf("requeue = %s, want %s", res.RequeueAfter, requeueProgressing)
	}

	fk.nodes[1].Ready = true
	fk.nodes[1].KubeletVersion = "v1.35.9"
	if _, err := r.Reconcile(ctx, requestFor(pool)); err != nil {
		t.Fatalf("second reconcile: %v", err)
	}
	got = getPool(t, r, pool)
	if cond := metaReady(got); cond == nil || cond.Status != metav1.ConditionTrue || cond.Reason != "Converged" {
		t.Fatalf("unexpected condition after both nodes are Ready: %#v", cond)
	}
}

func TestReconcileAppliesTaintsOnlyToReadyNodes(t *testing.T) {
	ctx := context.Background()
	pool := testPool(2)
	pool.Spec.Join.Taints = []string{"dedicated=workers:NoSchedule"}
	fc := &fakeCloud{vms: []cloud.VM{
		{ID: "vm-1", Name: "pool-wrk-01", IP: "192.168.100.7"},
		{ID: "vm-2", Name: "pool-wrk-02", IP: "192.168.100.8"},
	}}
	fk := &fakeKube{nodes: []kube.NodeInfo{
		{Name: "pool-wrk-01", Ready: true},
		{Name: "pool-wrk-02", Ready: false},
	}}
	r := newTestReconciler(t, fc, fk, pool)

	if _, err := r.Reconcile(ctx, requestFor(pool)); err != nil {
		t.Fatalf("reconcile: %v", err)
	}
	taints := fk.taints["pool-wrk-01"]
	if len(taints) != 1 || taints[0].Key != "dedicated" || taints[0].Value != "workers" || taints[0].Effect != corev1.TaintEffectNoSchedule {
		t.Fatalf("unexpected taints on the Ready node: %#v", taints)
	}
	if _, ok := fk.taints["pool-wrk-02"]; ok {
		t.Fatal("taints applied to a node that is not Ready")
	}
}

func TestValidateSpecRejectsInvalidLabels(t *testing.T) {
	pool := testPool(1)
	pool.Spec.Join.Labels = map[string]string{"bad key": "v"}
	r := &WorkerPoolReconciler{}
	if msg := r.validateSpec(pool); msg == "" {
		t.Fatal("expected an error for an invalid label key")
	}
	pool.Spec.Join.Labels = map[string]string{"ok": "x'y"}
	if msg := r.validateSpec(pool); msg == "" {
		t.Fatal("expected an error for an invalid label value")
	}
}

func TestParseTaints(t *testing.T) {
	got, err := parseTaints([]string{"dedicated=workers:NoSchedule", "node-role.kubernetes.io/worker:NoExecute", ""})
	if err != nil {
		t.Fatalf("parseTaints: %v", err)
	}
	if len(got) != 2 {
		t.Fatalf("got %d taints, want 2", len(got))
	}
	if got[0].Key != "dedicated" || got[0].Value != "workers" || got[0].Effect != corev1.TaintEffectNoSchedule {
		t.Fatalf("unexpected first taint: %#v", got[0])
	}
	if got[1].Key != "node-role.kubernetes.io/worker" || got[1].Effect != corev1.TaintEffectNoExecute {
		t.Fatalf("unexpected second taint: %#v", got[1])
	}

	for _, bad := range []string{"missing-effect", "k:Sometimes", ":NoSchedule", "bad key=v:NoSchedule"} {
		if _, err := parseTaints([]string{bad}); err == nil {
			t.Errorf("parseTaints(%q) expected an error", bad)
		}
	}
}

func TestReconcileIgnoresVMsOutsideTheNamePrefix(t *testing.T) {
	ctx := context.Background()
	pool := testPool(2)
	fc := &fakeCloud{vms: []cloud.VM{
		{ID: "m1", Name: "pool-mst-01", IP: "192.168.100.4"},
		{ID: "w1", Name: "pool-wrk-01", IP: "192.168.100.7"},
		{ID: "x1", Name: "other-wrk-01", IP: "192.168.100.9"},
	}}
	fk := &fakeKube{nodes: readyNodes("pool-wrk-01", "pool-wrk-02")}
	r := newTestReconciler(t, fc, fk, pool)

	if _, err := r.Reconcile(ctx, requestFor(pool)); err != nil {
		t.Fatalf("reconcile: %v", err)
	}
	if len(fc.created) != 1 {
		t.Fatalf("created %d VMs, want 1 (the missing second worker)", len(fc.created))
	}
	if fc.created[0].Name != "pool-wrk-02" {
		t.Fatalf("created %q, want pool-wrk-02", fc.created[0].Name)
	}
	got := getPool(t, r, pool)
	if len(got.Status.Nodes) != 2 {
		t.Fatalf("status has %d nodes, want 2: %#v", len(got.Status.Nodes), got.Status.Nodes)
	}
	for _, n := range got.Status.Nodes {
		if n.Name == "pool-mst-01" || n.Name == "other-wrk-01" {
			t.Fatalf("adopted a VM outside the name prefix: %s", n.Name)
		}
	}
}

func TestReconcileMatchesNodeNamesCaseInsensitively(t *testing.T) {
	ctx := context.Background()
	pool := testPool(1)
	pool.Spec.NamePrefix = "KION-WEB-DEV"
	pool.Spec.Join.Labels = map[string]string{"node-role.kubernetes.io/worker": ""}
	fc := &fakeCloud{vms: []cloud.VM{
		{ID: "vm-1", Name: "KION-WEB-DEV-wrk-01", IP: "192.168.100.7"},
	}}
	// kubelet registers node names in lower case.
	fk := &fakeKube{nodes: []kube.NodeInfo{{Name: "kion-web-dev-wrk-01", Ready: true}}}
	r := newTestReconciler(t, fc, fk, pool)

	if _, err := r.Reconcile(ctx, requestFor(pool)); err != nil {
		t.Fatalf("reconcile: %v", err)
	}
	got := getPool(t, r, pool)
	if got.Status.Nodes[0].Phase != infrav1.NodePhaseReady {
		t.Fatalf("phase = %s, want Ready", got.Status.Nodes[0].Phase)
	}
	if cond := metaReady(got); cond == nil || cond.Status != metav1.ConditionTrue {
		t.Fatalf("unexpected condition: %#v", cond)
	}
	if _, ok := fk.labels["kion-web-dev-wrk-01"]; !ok {
		t.Fatalf("labels were not applied to the lower-case node name: %#v", fk.labels)
	}
}
