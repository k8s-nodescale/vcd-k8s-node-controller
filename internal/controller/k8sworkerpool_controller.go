package controller

import (
	"context"
	"encoding/binary"
	"encoding/json"
	"errors"
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

	infrav1 "github.com/k8s-nodescale/vcd-k8s-node-controller/api/v1alpha1"
	"github.com/k8s-nodescale/vcd-k8s-node-controller/internal/cloud"
	"github.com/k8s-nodescale/vcd-k8s-node-controller/internal/cloudinit"
	"github.com/k8s-nodescale/vcd-k8s-node-controller/internal/ipam"
	"github.com/k8s-nodescale/vcd-k8s-node-controller/internal/kube"
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
	Cloud    cloud.Factory
	Kube     kube.Provider
	Renderer *cloudinit.Renderer
	Recorder record.EventRecorder

	// Org and VDC are the vCD org/VDC the vCD factory is authenticated
	// against (VCD_ORG/VCD_VDC). They are used to reject a CR whose
	// spec.vcd.org/vdc points somewhere else instead of silently ignoring it.
	// They are empty for another platform.
	Org string
	VDC string

	// PodName and PodNamespace identify this controller's Pod (downward API).
	// They let the last-worker guard refuse to delete the worker the pass
	// itself runs on.
	PodName      string
	PodNamespace string

	// ownNode caches the Node resolved from PodName, including an empty
	// result, so the guard costs one API call per process.
	ownNode *string
}

// +kubebuilder:rbac:groups=k8s-nodescale.github.io,resources=k8sworkerpools,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups=k8s-nodescale.github.io,resources=k8sworkerpools/status,verbs=get;update;patch
// +kubebuilder:rbac:groups=k8s-nodescale.github.io,resources=k8sworkerpools/finalizers,verbs=update
// +kubebuilder:rbac:groups="",resources=nodes,verbs=get;list;watch;patch;delete
// +kubebuilder:rbac:groups="",resources=pods,verbs=get;list;watch;delete
// +kubebuilder:rbac:groups="",resources=pods/eviction,verbs=create
// +kubebuilder:rbac:groups="",resources=persistentvolumes,verbs=get;list;watch
// +kubebuilder:rbac:groups="",resources=persistentvolumeclaims,verbs=get;list;watch
// +kubebuilder:rbac:groups=storage.k8s.io,resources=volumeattachments,verbs=get;list;watch
// +kubebuilder:rbac:groups=policy,resources=poddisruptionbudgets,verbs=get;list;watch
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

	// A pool being deleted is not reconciled towards its spec: the finalizer
	// drains and removes every worker first, then releases the object.
	if !pool.DeletionTimestamp.IsZero() {
		res, err := r.reconcilePoolDeletion(ctx, &pool)
		// The final status write is expected to lose the object once the
		// finalizer is gone; that is not an error.
		if ferr := r.flush(ctx, before, &pool); ferr != nil && !apierrors.IsNotFound(ferr) {
			if apierrors.IsConflict(ferr) {
				// The cache was one revision behind (the delete request and the
				// status write race): the next pass recomputes from the server.
				logger.V(1).Info("status update conflicted during deletion, retrying",
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

	res, err := r.reconcile(ctx, before, &pool)

	// The write that closes the pass. A create may already have published its
	// progress (see scaleUp); this one records everything else.
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
// It writes status only from scaleUp, which publishes a slot before and after
// a create that can take minutes (I-19); everything else is written by
// Reconcile once, after this returns.
func (r *WorkerPoolReconciler) reconcile(ctx context.Context, before, pool *infrav1.K8sWorkerPool) (ctrl.Result, error) {
	logger := log.FromContext(ctx)

	// The finalizer is what turns "delete the CR" into "delete the workers".
	// It is added before anything is created, so a pool can never own a VM
	// without owning its own cleanup.
	if err := r.ensureFinalizer(ctx, pool); err != nil {
		r.setCondition(pool, metav1.ConditionFalse, "FinalizerFailed", err.Error())
		return ctrl.Result{}, err
	}

	if msg := r.validateSpec(pool); msg != "" {
		r.setCondition(pool, metav1.ConditionFalse, "InvalidSpec", msg)
		logger.Info("worker pool spec is invalid", "reason", msg)
		return ctrl.Result{RequeueAfter: requeueInvalidSpec}, nil
	}

	prov, perr := r.platform(pool)
	if perr != nil {
		r.setCondition(pool, metav1.ConditionFalse, "CloudError", perr.Error())
		return ctrl.Result{}, perr
	}

	// List the whole pool and match by name prefix: VMs adopted from the
	// Terraform baseline have no controller metadata, and the pool is shared
	// with the masters and DVM.
	all, err := prov.ListWorkerVMs(ctx, nil)
	if err != nil {
		r.setCondition(pool, metav1.ConditionFalse, "CloudError", err.Error())
		return ctrl.Result{}, err
	}
	actual := ownedWorkerVMs(pool, all)

	alloc, err := ipam.New(toIPAMPlan(pool.Spec.IPPlan, pool.Spec.VCD), toIPAMLedger(pool.Status.Ledger))
	if err != nil {
		r.setCondition(pool, metav1.ConditionFalse, "InvalidIPPlan", err.Error())
		return ctrl.Result{RequeueAfter: requeueInvalidSpec}, nil
	}

	nodes, adopted, err := mergeNodes(pool, actual, alloc)
	if err != nil {
		r.setCondition(pool, metav1.ConditionFalse, "InvalidIPPlan", err.Error())
		return ctrl.Result{RequeueAfter: requeueInvalidSpec}, nil
	}

	// A worker whose VM disappeared outside the loop keeps its slot: the
	// address stays reserved and the entry stays in status.nodes with phase
	// Missing (or as Failed once AutoHeal gives up). Without this the replica
	// arithmetic would read the loss as a shortfall and silently provision a
	// successor under a new name, which is what 0.3.x did.
	nodes, err = r.mergeMissingSlots(pool, nodes, alloc)
	if err != nil {
		r.setCondition(pool, metav1.ConditionFalse, "InvalidIPPlan", err.Error())
		return ctrl.Result{RequeueAfter: requeueInvalidSpec}, nil
	}

	pool.Status.Nodes = nodes
	pool.Status.ObservedReplicas = int32(len(nodes))
	pool.Status.Ledger = toStatusLedger(alloc.Ledger())

	// Terraform's bootstrap workers carry no controller metadata and are
	// adopted on the first pass. Make the ownership transfer visible, and do
	// not remove one in the same pass it was adopted in: an outside
	// terraform apply must not turn into "the controller deleted a worker".
	for _, name := range adopted {
		logger.Info("adopted worker", "node", name, "pool", pool.Name)
		r.eventf(pool, corev1.EventTypeNormal, "WorkerAdopted",
			fmt.Sprintf("worker %s is owned by pool %s from now on", name, pool.Name))
	}

	// A rollout owns the pool while it runs: taking a worker out of service
	// and removing another at the same time would spend the same disruption
	// budget twice.
	if rolloutBlocksRemoval(pool) {
		switch {
		case len(pool.Spec.ScaleDown.DeleteWorkers) > 0:
			msg := "scaleDown.deleteWorkers is paused: a rollout is in progress (RolloutInProgress)"
			r.setCondition(pool, metav1.ConditionFalse, "RolloutInProgress", msg)
			r.eventf(pool, corev1.EventTypeWarning, "RolloutInProgress", msg)
			return ctrl.Result{RequeueAfter: requeueProgressing}, nil
		case pool.Spec.Replicas < int32(len(pool.Status.Nodes)):
			msg := "scale-in is paused: a rollout is in progress (RolloutInProgress)"
			r.setCondition(pool, metav1.ConditionFalse, "RolloutInProgress", msg)
			r.eventf(pool, corev1.EventTypeWarning, "RolloutInProgress", msg)
			return ctrl.Result{RequeueAfter: requeueProgressing}, nil
		}
	}

	// A removal the operator no longer asks for is reverted before the deletion
	// pass: appendInFlightDeletions keeps a Draining worker alive
	// unconditionally, which would otherwise finish a removal that
	// spec.replicas has already taken back.
	if handled, res, err := r.reconcileWithdrawnDeletions(ctx, pool); handled {
		if err != nil {
			logger.Error(err, "could not revert a withdrawn removal", "pool", pool.Name)
			r.setCondition(pool, metav1.ConditionFalse, "ScaleDownFailed", err.Error())
			return res, err
		}
		return res, nil
	}

	// Named deletions (spec.scaleDown.deleteWorkers and the per-node
	// annotation) run before the replica arithmetic: deleting a specific
	// worker must not be read as a scale-up.
	if handled, res, err := r.reconcileDeletions(ctx, pool, alloc); handled {
		if err != nil {
			logger.Error(err, "worker deletion failed", "pool", pool.Name)
			r.setCondition(pool, metav1.ConditionFalse, "ScaleDownFailed", err.Error())
			return res, err
		}
		return res, nil
	}

	if handled, res, err := r.reconcileMissing(ctx, pool, alloc); handled {
		if err != nil {
			logger.Error(err, "AutoHeal failed", "pool", pool.Name)
			r.setCondition(pool, metav1.ConditionFalse, "AutoHealFailed", err.Error())
			return res, err
		}
		return res, nil
	}

	// A create that a restart interrupted leaves a powered-off VM with no Node
	// behind it. Finish it before the replica arithmetic reads the slot as
	// healthy; the watchdog would otherwise park it Failed (I-18).
	if handled, err := r.resumeInterruptedCreates(ctx, pool, alloc); handled {
		if err != nil {
			logger.Error(err, "could not resume an interrupted create", "pool", pool.Name)
			r.setCondition(pool, metav1.ConditionFalse, "CreateResumeFailed", err.Error())
			return ctrl.Result{}, err
		}
		return ctrl.Result{RequeueAfter: requeueProgressing}, nil
	}

	// A slot the capacity pre-flight parked is retried here, before the replica
	// arithmetic reads the queue as a shortfall.
	if handled, err := r.resumeQueuedCreates(ctx, before, pool, alloc); handled {
		if err != nil {
			logger.Error(err, "could not start a queued worker", "pool", pool.Name)
			r.setCondition(pool, metav1.ConditionFalse, "ScaleUpFailed", err.Error())
			return ctrl.Result{}, err
		}
		return ctrl.Result{RequeueAfter: requeueProgressing}, nil
	}

	desired := pool.Spec.Replicas
	switch {
	case desired > pool.Status.ObservedReplicas:
		if err := r.scaleUp(ctx, before, pool, alloc, desired-pool.Status.ObservedReplicas); err != nil {
			logger.Error(err, "scale up failed", "pool", pool.Name)
			r.setCondition(pool, metav1.ConditionFalse, "ScaleUpFailed", err.Error())
			return ctrl.Result{}, err
		}
	case desired < pool.Status.ObservedReplicas:
		handled, res, err := r.scaleDown(ctx, pool, alloc, pool.Status.ObservedReplicas-desired, adopted)
		if err != nil {
			logger.Error(err, "scale down failed", "pool", pool.Name)
			r.setCondition(pool, metav1.ConditionFalse, "ScaleDownFailed", err.Error())
			return res, err
		}
		if handled {
			return res, nil
		}
	}

	// The rollout runs after the replica arithmetic so a scale-up during a
	// roll is allowed; a scale-in is refused inside the rollout itself.
	if handled, res, err := r.reconcileRollout(ctx, pool, alloc); handled {
		if err != nil {
			logger.Error(err, "rollout failed", "pool", pool.Name)
			r.setCondition(pool, metav1.ConditionFalse, "RolloutFailed", err.Error())
			return res, err
		}
		return res, nil
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
// ensureIngressPool puts a worker that has just become Ready into the ingress
// pool. Creation does not do it on purpose: a clone is not a pool member until
// its kubelet serves the node port. An absent or unsupported pool is fine (the
// pool is optional), but a rejected update is reported so a Ready worker is not
// silently left out of the load balancer.
func (r *WorkerPoolReconciler) ensureIngressPool(ctx context.Context, pool *infrav1.K8sWorkerPool, st *infrav1.WorkerNodeStatus) {
	if pool.Spec.IngressPool.Name == "" || st.IP == "" {
		return
	}
	m, merr := r.membership(pool)
	if merr != nil {
		log.FromContext(ctx).Error(merr, "resolve the ingress pool provider", "worker", st.Name, "ip", st.IP)
		r.eventf(pool, corev1.EventTypeWarning, "IngressPoolFailed",
			fmt.Sprintf("worker %s (%s) is Ready but the ingress pool could not be resolved: %v", st.Name, st.IP, merr))
		return
	}
	if m == nil {
		return
	}
	if err := m.AddPoolMember(ctx, toPoolRef(pool), st.IP); err != nil {
		if errors.Is(err, cloud.ErrPoolNotConfigured) {
			return
		}
		log.FromContext(ctx).Error(err, "add the worker to the ingress pool", "worker", st.Name, "ip", st.IP)
		r.eventf(pool, corev1.EventTypeWarning, "IngressPoolFailed",
			fmt.Sprintf("worker %s (%s) is Ready but not in the ingress pool: %v", st.Name, st.IP, err))
		return
	}
	r.eventf(pool, corev1.EventTypeNormal, "IngressPoolMember",
		fmt.Sprintf("worker %s (%s) added to the ingress pool", st.Name, st.IP))
}

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
		// A worker that is being deleted keeps its phase: overwriting it with
		// Ready would restart the drain on every pass. A Missing or a parked
		// Failed slot has no VM to ask, and its phase is the report itself.
		if st.DeleteStartedAt != nil || st.Phase == infrav1.NodePhaseMissing ||
			st.Phase == infrav1.NodePhaseQueued || st.Resize != nil {
			continue
		}
		if st.ProviderID == "" && st.Phase == infrav1.NodePhaseFailed {
			continue
		}
		info, ok := byName[strings.ToLower(st.Name)]
		if ok {
			// Remember the real Node object name: status.name is the vCD name,
			// kubelet registers the Node in lower case.
			st.NodeName = info.Name
		}
		switch {
		case !ok && st.NodeName != "" && st.ProviderID != "":
			// The VM is alive but its Node object is gone: kubelet lost its
			// registration (stopped, certificates rotated away, or the Node was
			// deleted by hand). Only a reboot repairs that; Notify reports it
			// and leaves the worker alone.
			st.K8sReady = false
			st.Phase = infrav1.NodePhaseJoining
			st.Message = "lost its Node registration; the VM is still running"
			if policyFor(pool) == infrav1.ReconciliationAutoHeal {
				r.healLostNode(ctx, pool, st)
			}
		case !ok:
			// The VM exists but no Node has registered yet. The clock starts
			// on the first pass that sees this state, so a stuck cloud-init is
			// bounded by spec.provisioning.timeout instead of waiting forever.
			if st.ProvisioningStartedAt == nil {
				now := metav1.Now()
				st.ProvisioningStartedAt = &now
			}
			if st.Phase == infrav1.NodePhaseFailed {
				// Already parked by the watchdog: keep the report and the VM.
				// A late registration is still picked up by the ok branches.
				st.K8sReady = false
				break
			}
			// A slow guest must not find its token expired by the time it
			// reaches kubeadm join.
			r.renewBootstrapToken(ctx, pool, st)
			elapsed := time.Since(st.ProvisioningStartedAt.Time)
			if budget := provisioningTimeout(pool); elapsed >= budget {
				st.Phase = infrav1.NodePhaseFailed
				st.K8sReady = false
				st.Message = fmt.Sprintf("VM has run for %s without a Kubernetes Node (spec.provisioning.timeout=%s); the VM is kept for inspection",
					elapsed.Round(time.Minute), budget)
				r.eventf(pool, corev1.EventTypeWarning, "WorkerProvisioningTimeout",
					fmt.Sprintf("worker %s (%s): %s", st.Name, st.IP, st.Message))
				break
			}
			st.Phase = infrav1.NodePhaseProvisioning
			st.K8sReady = false
			st.Message = "waiting for the node to register"
		case info.Ready:
			becameReady := st.Phase != infrav1.NodePhaseReady
			st.Phase = infrav1.NodePhaseReady
			st.K8sReady = true
			st.Message = ""
			// A worker that came back is a healthy slot again: the AutoHeal
			// budget starts over.
			st.HealAttempts = 0
			st.HealLastAttempt = nil
			st.ProvisioningStartedAt = nil
			// The guest has joined: its token is a live credential for a node
			// that no longer needs one.
			r.revokeBootstrapToken(ctx, st)
			if info.KubeletVersion != "" {
				st.KubeletVersion = info.KubeletVersion
			}
			if becameReady {
				r.ensureIngressPool(ctx, pool, st)
			}
		default:
			st.Phase = infrav1.NodePhaseJoining
			st.K8sReady = false
			st.Message = "node registered, waiting for Ready"
			st.ProvisioningStartedAt = nil
			r.renewBootstrapToken(ctx, pool, st)
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
	var missing, failed, queued []string
	queuedMessage := ""
	for _, n := range pool.Status.Nodes {
		switch n.Phase {
		case infrav1.NodePhaseReady:
			ready++
		case infrav1.NodePhaseMissing:
			missing = append(missing, fmt.Sprintf("%s (%s)", n.Name, n.IP))
		case infrav1.NodePhaseFailed:
			failed = append(failed, fmt.Sprintf("%s (%s)", n.Name, n.IP))
		case infrav1.NodePhaseQueued:
			queued = append(queued, fmt.Sprintf("%s (%s)", n.Name, n.IP))
			if queuedMessage == "" {
				queuedMessage = n.Message
			}
		}
	}
	switch {
	case len(missing) > 0:
		// A slot whose VM disappeared is never "just progressing": name what is
		// gone, so an operator can recreate or write it off.
		return metav1.ConditionFalse, "WorkerMissing",
			fmt.Sprintf("%d worker(s) missing from vCD: %s", len(missing), strings.Join(missing, ", "))
	case len(failed) > 0:
		// A parked worker is not "still converging": name it so the operator
		// sees the slot that needs a decision.
		return metav1.ConditionFalse, "WorkerFailed",
			fmt.Sprintf("%d worker(s) parked as Failed: %s", len(failed), strings.Join(failed, ", "))
	case len(queued) > 0:
		// The request is accepted but the vDC has no room for it. The slots
		// are reserved and retried; the reason carries the numbers so an
		// operator knows what to grow.
		msg := fmt.Sprintf("%d worker(s) queued for vDC capacity: %s", len(queued), strings.Join(queued, ", "))
		if queuedMessage != "" {
			msg += " (" + queuedMessage + ")"
		}
		return metav1.ConditionFalse, "NoResourceAvailable", msg
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
	if reflect.DeepEqual(before.Spec, pool.Spec) {
		if reflect.DeepEqual(before.Status, pool.Status) {
			return nil
		}
		return r.Status().Update(ctx, pool)
	}

	// A completed named deletion prunes spec.scaleDown.deleteWorkers and
	// decrements spec.replicas. Merge patches carry no resourceVersion for an
	// unchanged object, so the spec write cannot conflict with the status
	// write that follows it.
	computed := pool.DeepCopy()
	spec := pool.DeepCopy()
	if err := r.Patch(ctx, spec, client.MergeFrom(before.DeepCopy())); err != nil && !apierrors.IsNotFound(err) {
		return err
	}
	computed.ResourceVersion = spec.ResourceVersion
	if reflect.DeepEqual(before.Status, computed.Status) {
		return nil
	}
	return r.Status().Update(ctx, computed)
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

// platform returns the provider for this pool, bound to its placement. The
// factory is the only thing the reconciler knows about the platform, so a pool
// that names another implementation is served by that implementation.
func (r *WorkerPoolReconciler) platform(pool *infrav1.K8sWorkerPool) (cloud.Provider, error) {
	return r.Cloud.For(placementFor(pool))
}

// membership returns the load-balancer half of the pool's provider. It is nil
// when the platform has no pool the controller can address, and callers then
// leave membership to the operator.
func (r *WorkerPoolReconciler) membership(pool *infrav1.K8sWorkerPool) (cloud.PoolMembership, error) {
	prov, err := r.platform(pool)
	if err != nil {
		return nil, err
	}
	m, _ := prov.(cloud.PoolMembership)
	return m, nil
}

// placementFor maps a pool's CR onto the provider-neutral placement the factory
// takes. An empty spec.provider means vcd, which is what the CRD defaults it to.
func placementFor(pool *infrav1.K8sWorkerPool) cloud.Placement {
	pl := cloud.Placement{Provider: strings.ToLower(strings.TrimSpace(pool.Spec.Provider))}
	if pl.Provider == "" {
		pl.Provider = cloud.ProviderVCD
	}
	if v := pool.Spec.VCD; v != nil {
		pl.VCD = &cloud.VCDPlacement{
			Org:        v.Org,
			VDC:        v.VDC,
			VApp:       v.VApp,
			OrgNetwork: v.OrgNetwork,
			Catalog:    v.Catalog,
			CatalogOrg: v.CatalogOrg,
		}
	}
	return pl
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
	case strings.TrimSpace(pool.Spec.Flavor.Template) == "":
		return "spec.flavor.template is required"
	case strings.TrimSpace(pool.Spec.Join.Endpoint) == "":
		return "spec.join.endpoint is required"
	}
	switch strings.ToLower(strings.TrimSpace(pool.Spec.Provider)) {
	case "", cloud.ProviderVCD:
		switch {
		case pool.Spec.VCD == nil:
			return "spec.vcd is required when spec.provider is vcd"
		case strings.TrimSpace(pool.Spec.VCD.VApp) == "":
			return "spec.vcd.vapp is required"
		case strings.TrimSpace(pool.Spec.VCD.OrgNetwork) == "":
			return "spec.vcd.orgNetwork is required"
		}
		if r.Org != "" && pool.Spec.VCD.Org != "" && !strings.EqualFold(r.Org, pool.Spec.VCD.Org) {
			return fmt.Sprintf("spec.vcd.org %q does not match the controller org %q (VCD_ORG)", pool.Spec.VCD.Org, r.Org)
		}
		if r.VDC != "" && pool.Spec.VCD.VDC != "" && !strings.EqualFold(r.VDC, pool.Spec.VCD.VDC) {
			return fmt.Sprintf("spec.vcd.vdc %q does not match the controller VDC %q (VCD_VDC)", pool.Spec.VCD.VDC, r.VDC)
		}
	case cloud.ProviderVSphere:
		return "spec.provider vsphere is a stub: the vSphere provider is not implemented yet, only vcd is"
	default:
		return fmt.Sprintf("spec.provider %q is not supported (vcd)", pool.Spec.Provider)
	}
	if pool.Spec.ScaleDown.AllowLastWorker && pool.Spec.ScaleDown.MinReplicas > 0 {
		return "spec.scaleDown.allowLastWorker requires spec.scaleDown.minReplicas: 0: the flag could never be honoured while the floor is higher"
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
func mergeNodes(pool *infrav1.K8sWorkerPool, vms []cloud.VM, alloc *ipam.Allocator) ([]infrav1.WorkerNodeStatus, []string, error) {
	previous := make(map[string]infrav1.WorkerNodeStatus, len(pool.Status.Nodes))
	for _, n := range pool.Status.Nodes {
		previous[n.Name] = n
	}

	var adopted []string
	out := make([]infrav1.WorkerNodeStatus, 0, len(vms))
	for _, vm := range vms {
		node, known := previous[vm.Name]
		if !known {
			// No status entry for this VM: a Terraform bootstrap worker on the
			// first pass, or a VM somebody created by hand. It is adopted into
			// the pool instead of being a VM the pool reports on but cannot
			// remove.
			adopted = append(adopted, vm.Name)
		}
		node.Name = vm.Name
		if vm.IP != "" {
			if err := alloc.Adopt(vm.IP); err != nil {
				return nil, nil, fmt.Errorf("VM %q: %w", vm.Name, err)
			}
			node.IP = vm.IP
		}
		node.ProviderID = vm.ID
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
		// What the VM actually runs with; spec.flavor is the desired side. A VM
		// resized by hand is noticed by the same comparison the rollout uses.
		node.Flavor = &infrav1.FlavorStatus{
			CPUs:     vm.CPU,
			MemoryMB: vm.MemoryMB,
			OSDiskGB: vm.OSDiskGB,
		}
		out = append(out, node)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	return out, adopted, nil
}

// scaleUp provisions n workers. The ledger is kept in memory and persisted by
// the single status write at the end of the pass; a crash in between is
// harmless because the next pass adopts the VMs that exist in vCD again. If a
// create fails, the address is kept reserved only when the VM still exists in
// vCD; otherwise it is released for reuse.
func (r *WorkerPoolReconciler) scaleUp(ctx context.Context, before, pool *infrav1.K8sWorkerPool, alloc *ipam.Allocator, n int32) error {
	logger := log.FromContext(ctx)

	for i := int32(0); i < n; i++ {
		// A create that cannot fit fails in vCD halfway through (after the
		// clone, on a disk grow or on the power-on) and is retried every
		// pass. Park the rest of the request instead: the slots keep their
		// name and address and resumeQueuedCreates starts them once the vDC
		// has room.
		if ok, why := r.capacityAllows(ctx, pool, workerCapacityRequest(pool, pool.Status.Nodes)); !ok {
			if err := r.queueSlots(ctx, pool, alloc, n-i, why); err != nil {
				return err
			}
			r.eventf(pool, corev1.EventTypeWarning, "WorkerQueued",
				fmt.Sprintf("%d worker(s) are queued: %s", n-i, why))
			return nil
		}

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

		// The slot is written before the VM exists, so a create that takes
		// minutes is visible instead of the pool reporting nothing until the
		// pass ends (I-19). A crash between this write and the VM is harmless:
		// the slot is reserved, and AutoHeal recreates it.
		now := metav1.Now()
		pool.Status.Nodes = append(pool.Status.Nodes, infrav1.WorkerNodeStatus{
			Name:                  name,
			IP:                    ip,
			Phase:                 infrav1.NodePhaseProvisioning,
			ProvisioningStartedAt: &now,
			Message:               "creating the VM",
			CPU:                   pool.Spec.Flavor.CPUs,
			MemoryMB:              pool.Spec.Flavor.MemoryMB,
			Flavor: &infrav1.FlavorStatus{
				CPUs:     pool.Spec.Flavor.CPUs,
				MemoryMB: pool.Spec.Flavor.MemoryMB,
				OSDiskGB: pool.Spec.Flavor.OSDiskGB,
			},
		})
		pool.Status.ObservedReplicas = int32(len(pool.Status.Nodes))
		pool.Status.Ledger = toStatusLedger(alloc.Ledger())
		if err := r.flush(ctx, before, pool); err != nil {
			return err
		}

		vm, tokID, err := r.createWorker(ctx, pool, alloc, name, ip)
		if err != nil {
			// The placeholder must not survive a failed create: it has no VM
			// and would read as a Missing slot.
			pool.Status.Nodes = pool.Status.Nodes[:len(pool.Status.Nodes)-1]
			pool.Status.ObservedReplicas = int32(len(pool.Status.Nodes))
			pool.Status.Ledger = toStatusLedger(alloc.Ledger())
			return err
		}

		cpu, memMB := vm.CPU, vm.MemoryMB
		if cpu == 0 {
			cpu = pool.Spec.Flavor.CPUs
		}
		if memMB == 0 {
			memMB = pool.Spec.Flavor.MemoryMB
		}
		st := &pool.Status.Nodes[len(pool.Status.Nodes)-1]
		st.ProviderID = vm.ID
		st.Phase = infrav1.NodePhaseProvisioning
		st.Message = ""
		st.CPU = cpu
		st.MemoryMB = memMB
		st.DiskGB = vm.DiskGB
		st.Flavor = &infrav1.FlavorStatus{CPUs: vm.CPU, MemoryMB: vm.MemoryMB, OSDiskGB: vm.OSDiskGB}
		st.BootstrapTokenID = tokID
		pool.Status.Ledger = toStatusLedger(alloc.Ledger())
		// Persist the VM id as soon as it exists: a restart after this point
		// adopts the VM instead of creating a second one.
		if err := r.flush(ctx, before, pool); err != nil {
			return err
		}
		logger.Info("created worker", "node", name, "ip", ip, "providerID", vm.ID)
	}
	return nil
}

// resumeInterruptedCreates finishes a create the controller did not reach the
// end of. A restart in the middle of CreateVM leaves a VM the controller made
// powered off, with no Node behind it and no bootstrap token recorded for it
// (I-18). Nothing else would ever start it, so the create is simply run again:
// CreateVM converges on the existing VM and every step is idempotent. A VM that
// is powered on had reached the last step of its create and is left alone, and
// so is anything the controller did not create itself.
func (r *WorkerPoolReconciler) resumeInterruptedCreates(ctx context.Context, pool *infrav1.K8sWorkerPool, alloc *ipam.Allocator) (bool, error) {
	logger := log.FromContext(ctx)
	prov, perr := r.platform(pool)
	if perr != nil {
		return false, perr
	}

	for i := range pool.Status.Nodes {
		n := &pool.Status.Nodes[i]
		if n.ProviderID == "" || n.Phase != infrav1.NodePhaseProvisioning ||
			n.Resize != nil || n.DeleteStartedAt != nil || n.NodeName != "" ||
			n.BootstrapTokenID != "" {
			continue
		}
		vm, err := prov.GetVM(ctx, n.ProviderID)
		if err != nil {
			if errors.Is(err, cloud.ErrVMNotFound) {
				// mergeMissingSlots and AutoHeal own a slot whose VM is gone.
				continue
			}
			return true, err
		}
		if vm.PoweredOn || vm.Metadata["managed-by"] != "vcd-nodescale" {
			continue
		}

		staleToken := vm.Metadata["token-id"]
		resumed, tokID, err := r.createWorker(ctx, pool, alloc, n.Name, n.IP)
		if err != nil {
			n.Message = "resuming the interrupted create failed: " + err.Error()
			logger.Error(err, "resume an interrupted create", "worker", n.Name)
			r.eventf(pool, corev1.EventTypeWarning, "WorkerCreateResumeFailed",
				fmt.Sprintf("worker %s (%s): %v", n.Name, n.IP, err))
			return true, err
		}
		if staleToken != "" && staleToken != tokID {
			if err := r.Kube.RevokeBootstrapTokenID(ctx, staleToken); err != nil {
				logger.Error(err, "revoke the token of the interrupted create", "worker", n.Name, "token", staleToken)
			}
		}
		now := metav1.Now()
		n.ProviderID = resumed.ID
		n.Phase = infrav1.NodePhaseProvisioning
		n.ProvisioningStartedAt = &now
		n.BootstrapTokenID = tokID
		n.Message = ""
		n.CPU = resumed.CPU
		n.MemoryMB = resumed.MemoryMB
		n.DiskGB = resumed.DiskGB
		n.Flavor = &infrav1.FlavorStatus{CPUs: resumed.CPU, MemoryMB: resumed.MemoryMB, OSDiskGB: resumed.OSDiskGB}
		pool.Status.Ledger = toStatusLedger(alloc.Ledger())
		logger.Info("resumed an interrupted create", "worker", n.Name, "providerID", resumed.ID)
		r.eventf(pool, corev1.EventTypeNormal, "WorkerCreateResumed",
			fmt.Sprintf("worker %s (%s) was left powered off by an interrupted create; the create was finished", n.Name, n.IP))
		return true, nil
	}
	return false, nil
}

// createWorker provisions one worker VM into the slot (name, ip). It owns the
// bootstrap token lifecycle and the address bookkeeping of a failed attempt:
// the token is revoked and the address released unless the VM itself survived,
// because a token that outlives a failed create is a live credential for a node
// that will never exist (customization-phases.md §5.4: 21 leaked tokens in one
// afternoon).
func (r *WorkerPoolReconciler) createWorker(ctx context.Context, pool *infrav1.K8sWorkerPool, alloc *ipam.Allocator, name, ip string) (cloud.VM, string, error) {
	logger := log.FromContext(ctx)
	prov, perr := r.platform(pool)
	if perr != nil {
		return cloud.VM{}, "", perr
	}

	ttl := pool.Spec.Join.TokenTTL.Duration
	if ttl <= 0 {
		ttl = time.Hour
	}
	dnsJSON, err := json.Marshal(pool.Spec.Bootstrap.DNS)
	if err != nil {
		return cloud.VM{}, "", fmt.Errorf("encode DNS servers: %w", err)
	}
	cloudInitDisks, err := toCloudInitDisks(pool.Spec.Flavor.Disks)
	if err != nil {
		return cloud.VM{}, "", err
	}

	var token string
	release := func(cause error) (cloud.VM, string, error) {
		_ = alloc.Free(ip)
		pool.Status.Ledger = toStatusLedger(alloc.Ledger())
		if token != "" {
			if err := r.Kube.RevokeBootstrapToken(ctx, token); err != nil {
				logger.Error(err, "revoke bootstrap token", "node", name, "token", tokenID(token))
			}
		}
		return cloud.VM{}, "", cause
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

	vm, err := prov.CreateVM(ctx, cloud.CreateVMSpec{
		Name:       name,
		IP:         ip,
		Template:   pool.Spec.Flavor.Template,
		CPUs:       pool.Spec.Flavor.CPUs,
		MemoryMB:   pool.Spec.Flavor.MemoryMB,
		OSDiskGB:   pool.Spec.Flavor.OSDiskGB,
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
		cause := fmt.Errorf("create worker %s: %w", name, err)
		_ = r.reserveUnlessGone(ctx, pool, alloc, ip, name, cause)
		return cloud.VM{}, "", cause
	}
	return vm, tokenID(token), nil
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
	prov, perr := r.platform(pool)
	if perr != nil {
		return perr
	}
	exists := true // assume the worst when the lookup fails
	if vms, err := prov.ListWorkerVMs(ctx, nil); err == nil {
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

// renewBootstrapToken keeps a worker's token alive while it installs. The
// secret half never enters status; the id is enough to extend the expiration.
func (r *WorkerPoolReconciler) renewBootstrapToken(ctx context.Context, pool *infrav1.K8sWorkerPool, st *infrav1.WorkerNodeStatus) {
	if st.BootstrapTokenID == "" {
		return
	}
	ttl := pool.Spec.Join.TokenTTL.Duration
	if ttl <= 0 {
		ttl = time.Hour
	}
	if err := r.Kube.RenewBootstrapTokenID(ctx, st.BootstrapTokenID, ttl); err != nil {
		log.FromContext(ctx).Error(err, "renew bootstrap token", "worker", st.Name, "token", st.BootstrapTokenID)
	}
}

// revokeBootstrapToken removes the credential once the node is Ready. A failed
// revoke stays in status and is retried on the next pass.
func (r *WorkerPoolReconciler) revokeBootstrapToken(ctx context.Context, st *infrav1.WorkerNodeStatus) {
	if st.BootstrapTokenID == "" {
		return
	}
	if err := r.Kube.RevokeBootstrapTokenID(ctx, st.BootstrapTokenID); err != nil {
		log.FromContext(ctx).Error(err, "revoke bootstrap token", "worker", st.Name, "token", st.BootstrapTokenID)
		return
	}
	st.BootstrapTokenID = ""
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

func toIPAMPlan(p infrav1.IPPlan, v *infrav1.VCDRef) ipam.Plan {
	dvm := 0
	if v != nil {
		dvm = v.DVMOffset
	}
	return ipam.Plan{
		CIDR:        p.CIDR,
		MNodeOffset: p.MNodeOffset,
		WNodeOffset: p.WNodeOffset,
		DVMOffset:   dvm,
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
