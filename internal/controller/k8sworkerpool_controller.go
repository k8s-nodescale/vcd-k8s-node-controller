package controller

import (
	"context"
	"encoding/binary"
	"encoding/json"
	"fmt"
	"net"
	"net/netip"
	"reflect"
	"sort"
	"strconv"
	"strings"
	"time"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	utilvalidation "k8s.io/apimachinery/pkg/util/validation"
	"k8s.io/client-go/tools/record"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/handler"
	"sigs.k8s.io/controller-runtime/pkg/log"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"

	infrav1 "github.com/werton13/vcd-k8s-node-controller/api/v1alpha1"
	"github.com/werton13/vcd-k8s-node-controller/internal/cloud"
	"github.com/werton13/vcd-k8s-node-controller/internal/cloudinit"
	"github.com/werton13/vcd-k8s-node-controller/internal/ipam"
	"github.com/werton13/vcd-k8s-node-controller/internal/kube"
)

const (
	// requeueConverged is the level-based resync interval once the pool matches.
	requeueConverged = 60 * time.Second
	// requeueProgressing is used while the pool is still converging.
	requeueProgressing = 15 * time.Second
	// requeueInvalidSpec avoids a hot loop while the user fixes the CR.
	requeueInvalidSpec = 10 * time.Minute
)

// WorkerPoolReconciler reconciles a K8sWorkerPool object.
type WorkerPoolReconciler struct {
	client.Client
	Scheme   *runtime.Scheme
	Cloud    cloud.Provider
	Kube     kube.Provider
	Renderer *cloudinit.Renderer
	Recorder record.EventRecorder

	// Org and VDC are the vCD org/VDC the cloud provider is authenticated
	// against (VCD_ORG/VCD_VDC). They are used to reject a CR whose
	// spec.vcd.org/vdc points somewhere else instead of silently ignoring it.
	Org string
	VDC string
}

// +kubebuilder:rbac:groups=infra.example.com,resources=k8sworkerpools,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups=infra.example.com,resources=k8sworkerpools/status,verbs=get;update;patch
// +kubebuilder:rbac:groups=infra.example.com,resources=k8sworkerpools/finalizers,verbs=update
// +kubebuilder:rbac:groups="",resources=nodes,verbs=get;list;watch;patch;delete
// +kubebuilder:rbac:groups="",resources=pods,verbs=get;list;watch
// +kubebuilder:rbac:groups="",resources=pods/eviction,verbs=create
// +kubebuilder:rbac:groups="",resources=secrets,verbs=get;list;watch;create;delete
// +kubebuilder:rbac:groups="",resources=configmaps,verbs=get;list;watch
// +kubebuilder:rbac:groups="",resources=events,verbs=create;patch
// +kubebuilder:rbac:groups=coordination.k8s.io,resources=leases,verbs=get;list;watch;create;update;patch;delete

// Reconcile drives the worker pool towards spec.replicas.
//
// Observed state is rebuilt from vCD on every pass: VMs are adopted into the IP
// ledger before anything is created, so a lost or partially written status can
// never cause a duplicate address.
//
// The pass computes the whole observed state first and persists it with exactly
// one status write at the end. Two writes per pass would make the controller
// race against its own write: the second update carries the resourceVersion from
// before the first one and is rejected with "the object has been modified", so
// the status would churn forever and never converge.
func (r *WorkerPoolReconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	logger := log.FromContext(ctx)

	var pool infrav1.K8sWorkerPool
	if err := r.Get(ctx, req.NamespacedName, &pool); err != nil {
		return ctrl.Result{}, client.IgnoreNotFound(err)
	}
	before := pool.DeepCopy()

	res, err := r.reconcile(ctx, &pool)

	// The single status write of this pass.
	if ferr := r.flush(ctx, before, &pool); ferr != nil {
		if apierrors.IsConflict(ferr) {
			// Our cache was one revision behind. The next pass recomputes
			// everything from the server state, so this is not an error.
			logger.V(1).Info("status update conflicted, retrying on the next pass",
				"pool", pool.Name)
			return ctrl.Result{Requeue: true}, nil
		}
		if err != nil {
			return ctrl.Result{}, err
		}
		return ctrl.Result{}, ferr
	}
	return res, err
}

// reconcile computes the observed state and moves the pool towards the spec.
// It never writes status; Reconcile does that once, after it returns.
func (r *WorkerPoolReconciler) reconcile(ctx context.Context, pool *infrav1.K8sWorkerPool) (ctrl.Result, error) {
	logger := log.FromContext(ctx)

	if msg := r.validateSpec(pool); msg != "" {
		r.setCondition(pool, metav1.ConditionFalse, "InvalidSpec", msg)
		logger.Info("worker pool spec is invalid", "reason", msg)
		return ctrl.Result{RequeueAfter: requeueInvalidSpec}, nil
	}

	// List the whole vApp and match by name prefix: VMs adopted from the
	// Terraform baseline have no controller metadata, and the vApp is shared
	// with the masters and DVM.
	all, err := r.Cloud.ListWorkerVMs(ctx, pool.Spec.VCD.VApp, nil)
	if err != nil {
		r.setCondition(pool, metav1.ConditionFalse, "CloudError", err.Error())
		return ctrl.Result{}, err
	}
	actual := ownedWorkerVMs(pool, all)

	alloc, err := ipam.New(toIPAMPlan(pool.Spec.IPPlan), toIPAMLedger(pool.Status.Ledger))
	if err != nil {
		r.setCondition(pool, metav1.ConditionFalse, "InvalidIPPlan", err.Error())
		return ctrl.Result{RequeueAfter: requeueInvalidSpec}, nil
	}

	nodes, err := mergeNodes(pool, actual, alloc)
	if err != nil {
		r.setCondition(pool, metav1.ConditionFalse, "InvalidIPPlan", err.Error())
		return ctrl.Result{RequeueAfter: requeueInvalidSpec}, nil
	}

	pool.Status.Nodes = nodes
	pool.Status.ObservedReplicas = int32(len(actual))
	pool.Status.Ledger = toStatusLedger(alloc.Ledger())

	desired := pool.Spec.Replicas
	switch {
	case desired > pool.Status.ObservedReplicas:
		if err := r.scaleUp(ctx, pool, alloc, desired-pool.Status.ObservedReplicas); err != nil {
			logger.Error(err, "scale up failed", "pool", pool.Name)
			r.setCondition(pool, metav1.ConditionFalse, "ScaleUpFailed", err.Error())
			return ctrl.Result{}, err
		}
	case desired < pool.Status.ObservedReplicas:
		if err := r.scaleDown(ctx, pool, pool.Status.ObservedReplicas-desired); err != nil {
			logger.Error(err, "scale down failed", "pool", pool.Name)
			r.setCondition(pool, metav1.ConditionFalse, "ScaleDownFailed", err.Error())
			return ctrl.Result{}, err
		}
	}

	if err := r.refreshReadiness(ctx, pool); err != nil {
		r.setCondition(pool, metav1.ConditionFalse, "NodeReadinessUnknown", err.Error())
		return ctrl.Result{RequeueAfter: requeueProgressing}, nil
	}

	condStatus, reason, msg := evaluatePoolCondition(pool)
	r.setCondition(pool, condStatus, reason, msg)

	if err := r.applyNodeAttributes(ctx, pool); err != nil {
		r.setCondition(pool, metav1.ConditionFalse, "NodePatchFailed", err.Error())
		return ctrl.Result{}, err
	}

	requeue := requeueConverged
	if condStatus != metav1.ConditionTrue {
		requeue = requeueProgressing
	}
	return ctrl.Result{RequeueAfter: requeue}, nil
}

// refreshReadiness maps the actual Kubernetes Nodes onto status.nodes.
func (r *WorkerPoolReconciler) refreshReadiness(ctx context.Context, pool *infrav1.K8sWorkerPool) error {
	nodes, err := r.Kube.ListNodes(ctx)
	if err != nil {
		return err
	}
	// kubelet registers Node names in lower case, while VM names keep the
	// configured prefix case, so match case-insensitively.
	byName := make(map[string]kube.NodeInfo, len(nodes))
	for _, n := range nodes {
		byName[strings.ToLower(n.Name)] = n
	}

	for i := range pool.Status.Nodes {
		st := &pool.Status.Nodes[i]
		info, ok := byName[strings.ToLower(st.Name)]
		switch {
		case !ok:
			st.Phase = infrav1.NodePhaseProvisioning
			st.K8sReady = false
			st.Message = "waiting for the node to register"
		case info.Ready:
			st.Phase = infrav1.NodePhaseReady
			st.K8sReady = true
			st.Message = ""
			if info.KubeletVersion != "" {
				st.KubeletVersion = info.KubeletVersion
			}
		default:
			st.Phase = infrav1.NodePhaseJoining
			st.K8sReady = false
			st.Message = "node registered, waiting for Ready"
		}
		if !ok {
			continue
		}
		// The Node is the source of truth for resources as soon as kubelet has
		// registered it, Ready or not. mergeNodes only fills these fields while
		// no Node exists, so the two paths cannot flip the value every pass.
		if info.CPU > 0 {
			st.CPU = int32((info.CPU + 999) / 1000) // millicores -> cores, rounded up
		}
		if info.MemoryBytes > 0 {
			st.MemoryMB = int32(info.MemoryBytes / (1024 * 1024))
		}
	}
	return nil
}

// evaluatePoolCondition makes Ready mean "the requested workers are Ready",
// not "the last API calls succeeded".
func evaluatePoolCondition(pool *infrav1.K8sWorkerPool) (metav1.ConditionStatus, string, string) {
	desired := pool.Spec.Replicas
	var ready int32
	for _, n := range pool.Status.Nodes {
		if n.Phase == infrav1.NodePhaseReady {
			ready++
		}
	}
	switch {
	case pool.Status.ObservedReplicas != desired:
		return metav1.ConditionFalse, "Scaling",
			fmt.Sprintf("desired=%d observed=%d ready=%d", desired, pool.Status.ObservedReplicas, ready)
	case ready != desired:
		return metav1.ConditionFalse, "Progressing",
			fmt.Sprintf("%d/%d workers Ready", ready, desired)
	default:
		return metav1.ConditionTrue, "Converged",
			fmt.Sprintf("%d/%d workers Ready", ready, desired)
	}
}

// applyNodeAttributes applies spec.join.labels and spec.join.taints to every
// Ready node. Names are lower-cased because that is how kubelet registers them.
func (r *WorkerPoolReconciler) applyNodeAttributes(ctx context.Context, pool *infrav1.K8sWorkerPool) error {
	var taints []corev1.Taint
	if len(pool.Spec.Join.Taints) > 0 {
		parsed, err := parseTaints(pool.Spec.Join.Taints)
		if err != nil {
			return err
		}
		taints = parsed
	}
	if len(pool.Spec.Join.Labels) == 0 && len(taints) == 0 {
		return nil
	}
	for _, n := range pool.Status.Nodes {
		if n.Phase != infrav1.NodePhaseReady {
			continue
		}
		name := strings.ToLower(n.Name)
		if len(pool.Spec.Join.Labels) > 0 {
			if err := r.Kube.SetLabels(ctx, name, pool.Spec.Join.Labels); err != nil {
				return fmt.Errorf("set labels on %s: %w", name, err)
			}
		}
		if len(taints) > 0 {
			if err := r.Kube.SetTaints(ctx, name, taints); err != nil {
				return fmt.Errorf("set taints on %s: %w", name, err)
			}
		}
	}
	return nil
}

// parseTaints accepts the kubectl notation key[=value]:Effect.
func parseTaints(in []string) ([]corev1.Taint, error) {
	out := make([]corev1.Taint, 0, len(in))
	for _, raw := range in {
		spec := strings.TrimSpace(raw)
		if spec == "" {
			continue
		}
		i := strings.LastIndex(spec, ":")
		if i < 0 {
			return nil, fmt.Errorf("taint %q must end with :Effect", raw)
		}
		effect := corev1.TaintEffect(spec[i+1:])
		spec = spec[:i]
		switch effect {
		case corev1.TaintEffectNoSchedule, corev1.TaintEffectPreferNoSchedule, corev1.TaintEffectNoExecute:
		default:
			return nil, fmt.Errorf("taint %q has invalid effect %q", raw, effect)
		}

		key, value := spec, ""
		if j := strings.Index(spec, "="); j >= 0 {
			key, value = spec[:j], spec[j+1:]
		}
		if errs := utilvalidation.IsQualifiedName(key); len(errs) > 0 {
			return nil, fmt.Errorf("taint %q has an invalid key: %s", raw, strings.Join(errs, "; "))
		}
		if value != "" {
			if errs := utilvalidation.IsValidLabelValue(value); len(errs) > 0 {
				return nil, fmt.Errorf("taint %q has an invalid value: %s", raw, strings.Join(errs, "; "))
			}
		}
		out = append(out, corev1.Taint{Key: key, Value: value, Effect: effect})
	}
	return out, nil
}

// flush writes status only when it changed. Writing an identical status would
// bump resourceVersion and re-trigger the reconcile loop.
func (r *WorkerPoolReconciler) flush(ctx context.Context, before, pool *infrav1.K8sWorkerPool) error {
	if reflect.DeepEqual(before.Status, pool.Status) {
		return nil
	}
	return r.Status().Update(ctx, pool)
}

func (r *WorkerPoolReconciler) setCondition(pool *infrav1.K8sWorkerPool, status metav1.ConditionStatus, reason, msg string) {
	meta.SetStatusCondition(&pool.Status.Conditions, metav1.Condition{
		Type:               "Ready",
		Status:             status,
		Reason:             reason,
		Message:            msg,
		ObservedGeneration: pool.Generation,
	})
}

// validateSpec rejects values the controller would otherwise silently ignore
// (org/vdc come from the process environment) or that cannot produce a working
// worker (no template, no join endpoint).
func (r *WorkerPoolReconciler) validateSpec(pool *infrav1.K8sWorkerPool) string {
	switch {
	case strings.TrimSpace(pool.Spec.NamePrefix) == "":
		return "spec.namePrefix is required"
	case strings.TrimSpace(pool.Spec.Bootstrap.OSUser) == "":
		return "spec.bootstrap.osUser is required"
	case strings.TrimSpace(pool.Spec.Bootstrap.K8sMinorRepo) == "":
		return "spec.bootstrap.k8sMinorRepo is required"
	case strings.TrimSpace(pool.Spec.VCD.VApp) == "":
		return "spec.vcd.vapp is required"
	case strings.TrimSpace(pool.Spec.VCD.OrgNetwork) == "":
		return "spec.vcd.orgNetwork is required"
	case strings.TrimSpace(pool.Spec.Flavor.Template) == "":
		return "spec.flavor.template is required"
	case strings.TrimSpace(pool.Spec.Join.Endpoint) == "":
		return "spec.join.endpoint is required"
	}
	if r.Org != "" && pool.Spec.VCD.Org != "" && !strings.EqualFold(r.Org, pool.Spec.VCD.Org) {
		return fmt.Sprintf("spec.vcd.org %q does not match the controller org %q (VCD_ORG)", pool.Spec.VCD.Org, r.Org)
	}
	if r.VDC != "" && pool.Spec.VCD.VDC != "" && !strings.EqualFold(r.VDC, pool.Spec.VCD.VDC) {
		return fmt.Sprintf("spec.vcd.vdc %q does not match the controller VDC %q (VCD_VDC)", pool.Spec.VCD.VDC, r.VDC)
	}
	for k, v := range pool.Spec.Join.Labels {
		if errs := utilvalidation.IsQualifiedName(k); len(errs) > 0 {
			return fmt.Sprintf("spec.join.labels key %q is invalid: %s", k, strings.Join(errs, "; "))
		}
		if errs := utilvalidation.IsValidLabelValue(v); len(errs) > 0 {
			return fmt.Sprintf("spec.join.labels value %q for %q is invalid: %s", v, k, strings.Join(errs, "; "))
		}
	}
	if _, err := parseTaints(pool.Spec.Join.Taints); err != nil {
		return err.Error()
	}
	return ""
}

// mergeNodes rebuilds status.nodes from the VMs that actually exist and adopts
// their addresses into the ledger. Per-node phase and Kubernetes readiness are
// carried over by name.
func mergeNodes(pool *infrav1.K8sWorkerPool, vms []cloud.VM, alloc *ipam.Allocator) ([]infrav1.WorkerNodeStatus, error) {
	previous := make(map[string]infrav1.WorkerNodeStatus, len(pool.Status.Nodes))
	for _, n := range pool.Status.Nodes {
		previous[n.Name] = n
	}

	out := make([]infrav1.WorkerNodeStatus, 0, len(vms))
	for _, vm := range vms {
		node := previous[vm.Name]
		node.Name = vm.Name
		if vm.IP != "" {
			if err := alloc.Adopt(vm.IP); err != nil {
				return nil, fmt.Errorf("VM %q: %w", vm.Name, err)
			}
			node.IP = vm.IP
		}
		node.VMID = vm.ID
		if node.Phase == "" {
			node.Phase = infrav1.NodePhaseProvisioning
		}
		// CPU and memory have two sources: the vCD flavor and the Node
		// allocatable. The Node wins as soon as kubelet has registered (see
		// refreshReadiness), and the flavor only fills the gap before that.
		// Overwriting unconditionally here would make both paths flip the
		// value on every pass (8192 MB flavor vs 7841 MiB allocatable) and
		// the status would never converge.
		if node.CPU == 0 && vm.CPU > 0 {
			node.CPU = vm.CPU
		}
		if node.MemoryMB == 0 && vm.MemoryMB > 0 {
			node.MemoryMB = vm.MemoryMB
		}
		if vm.DiskGB > 0 {
			node.DiskGB = vm.DiskGB
		}
		out = append(out, node)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	return out, nil
}

// scaleUp provisions n workers. The ledger is kept in memory and persisted by
// the single status write at the end of the pass; a crash in between is
// harmless because the next pass adopts the VMs that exist in vCD again. If a
// create fails, the address is kept reserved only when the VM still exists in
// vCD; otherwise it is released for reuse.
func (r *WorkerPoolReconciler) scaleUp(ctx context.Context, pool *infrav1.K8sWorkerPool, alloc *ipam.Allocator, n int32) error {
	logger := log.FromContext(ctx)

	ttl := pool.Spec.Join.TokenTTL.Duration
	if ttl <= 0 {
		ttl = time.Hour
	}
	dnsJSON, err := json.Marshal(pool.Spec.Bootstrap.DNS)
	if err != nil {
		return fmt.Errorf("encode DNS servers: %w", err)
	}
	cloudInitDisks, err := toCloudInitDisks(pool.Spec.Flavor.Disks)
	if err != nil {
		return err
	}

	syncLedger := func() {
		pool.Status.Ledger = toStatusLedger(alloc.Ledger())
	}

	for i := int32(0); i < n; i++ {
		ip, err := alloc.Allocate()
		if err != nil {
			return err
		}
		ordinal, err := alloc.Ordinal(ip)
		if err != nil {
			_ = alloc.Free(ip)
			return err
		}
		name := workerName(pool.Spec.NamePrefix, ordinal)

		// token is set once a bootstrap token exists for this attempt, so release
		// can revoke it: a token that outlives a failed create is a live
		// credential for a node that will never exist (customization-phases.md
		// §5.4 - observed: 21 leaked tokens in one afternoon).
		var token string
		// release frees the address and revokes the token when nothing was created.
		release := func(cause error) error {
			_ = alloc.Free(ip)
			if token != "" {
				if err := r.Kube.RevokeBootstrapToken(ctx, token); err != nil {
					logger.Error(err, "revoke bootstrap token", "node", name, "token", tokenID(token))
				}
			}
			syncLedger()
			return cause
		}
		token, err = r.Kube.CreateBootstrapToken(ctx, ttl)
		if err != nil {
			return release(fmt.Errorf("create bootstrap token for %s: %w", name, err))
		}
		caHash, err := r.Kube.CAHash(ctx)
		if err != nil {
			return release(fmt.Errorf("compute CA hash for %s: %w", name, err))
		}
		join, err := buildJoinCommand(pool.Spec.Join.Endpoint, token, caHash)
		if err != nil {
			return release(fmt.Errorf("build join command for %s: %w", name, err))
		}

		userData, initscript, err := r.Renderer.Render(cloudinit.Params{
			NodeName:         name,
			NodeIP:           ip,
			CIDRPrefix:       cidrPrefix(pool.Spec.IPPlan.CIDR),
			GatewayIP:        gatewayIP(pool.Spec.IPPlan.CIDR),
			NICName:          pool.Spec.Bootstrap.NICName,
			DNSServersJSON:   string(dnsJSON),
			OSUser:           pool.Spec.Bootstrap.OSUser,
			OSPasswordHash:   pool.Spec.Bootstrap.OSPasswordHash,
			SSHAuthorizedKey: pool.Spec.Bootstrap.SSHAuthorizedKey,
			K8sVersion:       pool.Spec.Bootstrap.K8sVersion,
			K8sMinorRepo:     pool.Spec.Bootstrap.K8sMinorRepo,
			JoinEndpoint:     pool.Spec.Join.Endpoint,
			JoinCommand:      join,
			Disks:            cloudInitDisks,
		})
		if err != nil {
			return release(fmt.Errorf("render bootstrap for %s: %w", name, err))
		}

		vm, err := r.Cloud.CreateVM(ctx, cloud.CreateVMSpec{
			Name:       name,
			IP:         ip,
			VApp:       pool.Spec.VCD.VApp,
			OrgNetwork: pool.Spec.VCD.OrgNetwork,
			Catalog:    pool.Spec.VCD.Catalog,
			CatalogOrg: pool.Spec.VCD.CatalogOrg,
			Template:   pool.Spec.Flavor.Template,
			CPUs:       pool.Spec.Flavor.CPUs,
			MemoryMB:   pool.Spec.Flavor.MemoryMB,
			Disks:      toCloudDisks(pool.Spec.Flavor.Disks),
			Initscript: initscript,
			UserData:   userData,
			Metadata: map[string]string{
				"managed-by": "vcd-nodescale",
				"pool":       pool.Name,
				"node-name":  name,
				// Which build baked this VM, and which bootstrap token it holds:
				// without these a stuck guest cannot be traced or cleaned up
				// (customization-phases.md §5.1, §5.4).
				"render":   r.Renderer.Stamp(),
				"token-id": tokenID(token),
			},
			EnableUUID: true,
		})
		if err != nil {
			return r.reserveUnlessGone(ctx, pool, alloc, ip, name,
				fmt.Errorf("create worker %s: %w", name, err))
		}

		cpu, memMB, diskGB := vm.CPU, vm.MemoryMB, vm.DiskGB
		if cpu == 0 {
			cpu = pool.Spec.Flavor.CPUs
		}
		if memMB == 0 {
			memMB = pool.Spec.Flavor.MemoryMB
		}
		pool.Status.Nodes = append(pool.Status.Nodes, infrav1.WorkerNodeStatus{
			Name:     name,
			IP:       ip,
			VMID:     vm.ID,
			Phase:    infrav1.NodePhaseProvisioning,
			CPU:      cpu,
			MemoryMB: memMB,
			DiskGB:   diskGB,
		})
		pool.Status.ObservedReplicas = int32(len(pool.Status.Nodes))
		syncLedger()
		logger.Info("created worker", "node", name, "ip", ip, "vmID", vm.ID)
	}
	return nil
}

// ownedWorkerVMs keeps the VMs that belong to the pool. Ownership is decided by
// the name prefix, not by metadata: workers that already existed before the
// controller was installed carry no labels, and adopting them is what prevents
// duplicate names and addresses.
func ownedWorkerVMs(pool *infrav1.K8sWorkerPool, vms []cloud.VM) []cloud.VM {
	prefix := pool.Spec.NamePrefix + "-wrk-"
	out := make([]cloud.VM, 0, len(vms))
	for _, vm := range vms {
		if strings.HasPrefix(vm.Name, prefix) {
			out = append(out, vm)
		}
	}
	return out
}

// reserveUnlessGone keeps the address when a VM survived a failed create and
// frees it otherwise, so repeated failures cannot leak the worker range.
func (r *WorkerPoolReconciler) reserveUnlessGone(ctx context.Context, pool *infrav1.K8sWorkerPool, alloc *ipam.Allocator, ip, name string, cause error) error {
	exists := true // assume the worst when the lookup fails
	if vms, err := r.Cloud.ListWorkerVMs(ctx, pool.Spec.VCD.VApp, nil); err == nil {
		exists = false
		for _, vm := range vms {
			if vm.Name == name {
				exists = true
				break
			}
		}
	}
	if !exists {
		_ = alloc.Free(ip)
	}
	pool.Status.Ledger = toStatusLedger(alloc.Ledger())
	return cause
}

// scaleDown removes n workers. TODO: cordon -> drain (PDB-aware) -> remove from
// ingress LB pool -> kubeadm reset -> delete VM + disks -> free IP -> delete Node.
func (r *WorkerPoolReconciler) scaleDown(ctx context.Context, pool *infrav1.K8sWorkerPool, n int32) error {
	// Intentionally a no-op until kube.Provider.Drain is implemented.
	return nil
}

// SetupWithManager registers the controller and watches Nodes to trigger reconciliation.
func (r *WorkerPoolReconciler) SetupWithManager(mgr ctrl.Manager) error {
	return ctrl.NewControllerManagedBy(mgr).
		For(&infrav1.K8sWorkerPool{}).
		Watches(&corev1.Node{}, handler.EnqueueRequestsFromMapFunc(r.mapNodeToPools)).
		Complete(r)
}

// mapNodeToPools enqueues only the pools whose name prefix owns the node.
// Enqueueing every pool for every Node event caused a reconcile storm.
func (r *WorkerPoolReconciler) mapNodeToPools(ctx context.Context, obj client.Object) []reconcile.Request {
	node, ok := obj.(*corev1.Node)
	if !ok {
		return nil
	}
	var pools infrav1.K8sWorkerPoolList
	if err := r.List(ctx, &pools); err != nil {
		return nil
	}
	out := make([]reconcile.Request, 0, 2)
	for i := range pools.Items {
		prefix := strings.ToLower(strings.TrimSpace(pools.Items[i].Spec.NamePrefix))
		if prefix == "" || !strings.HasPrefix(strings.ToLower(node.Name), prefix+"-wrk-") {
			continue
		}
		out = append(out, reconcile.Request{NamespacedName: client.ObjectKeyFromObject(&pools.Items[i])})
	}
	return out
}

// workerName ties the node name to the IP ordinal, so a reused address keeps
// the name it was originally created with instead of shifting indices.
// tokenID returns the id half of a bootstrap token ("<id>.<secret>"), which is
// also the suffix of the Secret and of the kube-public JWS entry.
func tokenID(token string) string {
	id, _, _ := strings.Cut(strings.TrimSpace(token), ".")
	return id
}

func workerName(prefix string, ordinal int) string {
	return fmt.Sprintf("%s-wrk-%02d", prefix, ordinal)
}

// buildJoinCommand renders a shell-safe kubeadm join command with validated
// node labels.
func buildJoinCommand(endpoint, token, caHash string) (string, error) {
	endpoint = strings.TrimSpace(endpoint)
	if endpoint == "" {
		return "", fmt.Errorf("join.endpoint is empty")
	}
	host, port, err := net.SplitHostPort(endpoint)
	if err != nil || strings.TrimSpace(host) == "" {
		return "", fmt.Errorf("join.endpoint %q must be host:port", endpoint)
	}
	if _, err := strconv.Atoi(port); err != nil {
		return "", fmt.Errorf("join.endpoint %q has a non-numeric port", endpoint)
	}
	if token == "" || caHash == "" {
		return "", fmt.Errorf("bootstrap token and CA hash are required")
	}

	// Node labels from spec.join.labels are patched onto the Node once it is
	// Ready (applyNodeAttributes), so the join command stays minimal.
	return fmt.Sprintf("kubeadm join %s --token %s --discovery-token-ca-cert-hash %s", endpoint, token, caHash), nil
}

func toIPAMPlan(p infrav1.IPPlan) ipam.Plan {
	return ipam.Plan{
		CIDR:        p.CIDR,
		MNodeOffset: p.MNodeOffset,
		WNodeOffset: p.WNodeOffset,
		DVMOffset:   p.DVMOffset,
		KubeAPILB:   p.KubeAPILB,
		IngressLB:   p.IngressLB,
		MasterCount: p.MasterCount,
		Reserved:    p.Reserved,
	}
}

func toIPAMLedger(l infrav1.IPLedger) ipam.Ledger {
	return ipam.Ledger{NextIndex: l.NextIndex, Used: l.Used, Free: l.Free}
}

func toStatusLedger(l ipam.Ledger) infrav1.IPLedger {
	return infrav1.IPLedger{
		NextIndex: l.NextIndex,
		Used:      append([]string(nil), l.Used...),
		Free:      append([]string(nil), l.Free...),
	}
}

func toCloudInitDisks(in []infrav1.DiskSpec) ([]cloudinit.DiskParam, error) {
	if len(in) == 0 {
		return nil, nil
	}

	// Use the same placement algorithm as the vCD provider so the guest device
	// order (sdb, sdc, ...) matches the order in which AddInternalDisk attaches
	// the disks. The template OS disk occupies bus 0/unit 0.
	placements, err := cloud.PlanDiskPlacements(toCloudDisks(in), []cloud.DiskPlacement{
		{Index: -1, BusNumber: 0, UnitNumber: 0},
	})
	if err != nil {
		return nil, err
	}

	out := make([]cloudinit.DiskParam, 0, len(placements))
	for i, placement := range placements {
		tempMount := fmt.Sprintf("/data%d", i+1)
		mount := placement.Spec.Mount
		if mount == "" {
			mount = tempMount
		}
		if !strings.HasPrefix(mount, "/") {
			return nil, fmt.Errorf("disk %d: mount %q must be an absolute path", placement.Index, mount)
		}

		var excludes []string
		for j, other := range placements {
			if j == i {
				continue
			}
			otherMount := other.Spec.Mount
			if otherMount == "" {
				otherMount = fmt.Sprintf("/data%d", j+1)
			}
			if strings.HasPrefix(otherMount, mount+"/") {
				excludes = append(excludes, strings.TrimPrefix(otherMount, mount))
			}
		}

		out = append(out, cloudinit.DiskParam{
			Device:        fmt.Sprintf("/dev/sd%c", 'b'+i),
			VG:            fmt.Sprintf("data%d_vg", i+1),
			LV:            fmt.Sprintf("data%d_lv", i+1),
			TempMount:     tempMount,
			Mount:         mount,
			FSType:        "ext4",
			SizeGB:        placement.Spec.SizeGB,
			RsyncExcludes: excludes,
		})
	}
	return out, nil
}

func toCloudDisks(in []infrav1.DiskSpec) []cloud.DiskSpec {
	out := make([]cloud.DiskSpec, 0, len(in))
	for _, d := range in {
		out = append(out, cloud.DiskSpec{
			SizeGB:         d.SizeGB,
			Mount:          d.Mount,
			BusType:        d.BusType,
			BusNumber:      d.BusNumber,
			UnitNumber:     d.UnitNumber,
			StorageProfile: d.StorageProfile,
		})
	}
	return out
}

func cidrPrefix(cidr string) string {
	p, err := netip.ParsePrefix(cidr)
	if err != nil {
		return ""
	}
	return strconv.Itoa(p.Bits())
}

func gatewayIP(cidr string) string {
	p, err := netip.ParsePrefix(cidr)
	if err != nil {
		return ""
	}
	b := p.Masked().Addr().As4()
	v := binary.BigEndian.Uint32(b[:]) + 1
	var out [4]byte
	binary.BigEndian.PutUint32(out[:], v)
	return netip.AddrFrom4(out).String()
}
