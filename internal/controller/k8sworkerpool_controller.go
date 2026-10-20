package controller

import (
	"context"
	"encoding/binary"
	"encoding/json"
	"fmt"
	"net/netip"
	"strconv"
	"strings"
	"time"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
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

// WorkerPoolReconciler reconciles a K8sWorkerPool object.
type WorkerPoolReconciler struct {
	client.Client
	Scheme   *runtime.Scheme
	Cloud    cloud.Provider
	Kube     kube.Provider
	Renderer *cloudinit.Renderer
	Recorder record.EventRecorder
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
func (r *WorkerPoolReconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	logger := log.FromContext(ctx)

	var pool infrav1.K8sWorkerPool
	if err := r.Get(ctx, req.NamespacedName, &pool); err != nil {
		return ctrl.Result{}, client.IgnoreNotFound(err)
	}

	actual, err := r.Cloud.ListWorkerVMs(ctx, pool.Spec.VCD.VApp, map[string]string{
		"managed-by": "vcd-nodescale",
		"pool":       pool.Name,
	})
	if err != nil {
		r.setNotReady(ctx, &pool, "CloudError", err.Error())
		return ctrl.Result{}, err
	}

	pool.Status.ObservedReplicas = int32(len(actual))
	desired := pool.Spec.Replicas

	switch {
	case desired > pool.Status.ObservedReplicas:
		if err := r.scaleUp(ctx, &pool, desired-pool.Status.ObservedReplicas); err != nil {
			logger.Error(err, "scale up failed")
			r.setNotReady(ctx, &pool, "ScaleUpFailed", err.Error())
			return ctrl.Result{}, err
		}
	case desired < pool.Status.ObservedReplicas:
		if err := r.scaleDown(ctx, &pool, pool.Status.ObservedReplicas-desired); err != nil {
			logger.Error(err, "scale down failed")
			r.setNotReady(ctx, &pool, "ScaleDownFailed", err.Error())
			return ctrl.Result{}, err
		}
	}

	meta.SetStatusCondition(&pool.Status.Conditions, metav1.Condition{
		Type:               "Ready",
		Status:             metav1.ConditionTrue,
		Reason:             "Converged",
		Message:            fmt.Sprintf("desired=%d observed=%d", desired, pool.Status.ObservedReplicas),
		ObservedGeneration: pool.Generation,
	})
	if err := r.Status().Update(ctx, &pool); err != nil {
		return ctrl.Result{}, err
	}
	return ctrl.Result{RequeueAfter: 30 * time.Second}, nil
}

func (r *WorkerPoolReconciler) setNotReady(ctx context.Context, pool *infrav1.K8sWorkerPool, reason, msg string) {
	meta.SetStatusCondition(&pool.Status.Conditions, metav1.Condition{
		Type:               "Ready",
		Status:             metav1.ConditionFalse,
		Reason:             reason,
		Message:            msg,
		ObservedGeneration: pool.Generation,
	})
	_ = r.Status().Update(ctx, pool)
}

// scaleUp provisions n workers. LB membership is added later, only after the
// node reports Ready (see reconcileNodeReadiness TODO).
func (r *WorkerPoolReconciler) scaleUp(ctx context.Context, pool *infrav1.K8sWorkerPool, n int32) error {
	alloc, err := ipam.New(toIPAMPlan(pool.Spec.IPPlan), toIPAMLedger(pool.Status.Ledger))
	if err != nil {
		return err
	}
	ttl := pool.Spec.Join.TokenTTL.Duration
	if ttl == 0 {
		ttl = time.Hour
	}
	dnsJSON, err := json.Marshal(pool.Spec.Bootstrap.DNS)
	if err != nil {
		return err
	}

	for i := int32(0); i < n; i++ {
		ip, err := alloc.Allocate()
		if err != nil {
			return err
		}
		token, err := r.Kube.CreateBootstrapToken(ctx, ttl)
		if err != nil {
			return err
		}
		caHash, err := r.Kube.CAHash(ctx)
		if err != nil {
			return err
		}

		name := fmt.Sprintf("%s-wrk-%02d", pool.Spec.NamePrefix, alloc.Ledger().NextIndex)
		join := fmt.Sprintf("kubeadm join %s --token %s --discovery-token-ca-cert-hash %s",
			pool.Spec.Join.Endpoint, token, caHash)

		cloudInitDisks, err := toCloudInitDisks(pool.Spec.Flavor.Disks)
		if err != nil {
			return err
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
			JoinCommand:      join,
			Disks:            cloudInitDisks,
		})
		if err != nil {
			return err
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
			},
			EnableUUID: true,
		})
		if err != nil {
			return err
		}

		pool.Status.Nodes = append(pool.Status.Nodes, infrav1.WorkerNodeStatus{
			Name:     name,
			IP:       ip,
			VMID:     vm.ID,
			Phase:    infrav1.NodePhaseProvisioning,
			CPU:      pool.Spec.Flavor.CPUs,
			MemoryMB: pool.Spec.Flavor.MemoryMB,
		})
	}

	pool.Status.Ledger = toStatusLedger(alloc.Ledger())
	return nil
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

func (r *WorkerPoolReconciler) mapNodeToPools(ctx context.Context, obj client.Object) []reconcile.Request {
	var pools infrav1.K8sWorkerPoolList
	if err := r.List(ctx, &pools); err != nil {
		return nil
	}
	out := make([]reconcile.Request, 0, len(pools.Items))
	for i := range pools.Items {
		out = append(out, reconcile.Request{NamespacedName: client.ObjectKeyFromObject(&pools.Items[i])})
	}
	return out
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
	return infrav1.IPLedger{NextIndex: l.NextIndex, Used: l.Used, Free: l.Free}
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
