package controller

import (
	"context"
	"net"
	"strings"
	"time"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/yaml"

	infrav1 "github.com/k8s-nodescale/vcd-k8s-node-controller/api/v1alpha1"
)

// capacityStatusTTL is how long a snapshot may stand before it is retaken even
// if nothing scaled. The page is read by humans deciding whether another worker
// fits, and VMs can appear or vanish outside the pool, so drift has to be
// caught eventually - but not on every pass, because a Capacity() call is a vCD
// round trip.
const capacityStatusTTL = 10 * time.Minute

// podsPerNode is what a kubelet is configured for by default and what the pod
// network has to be able to address: node count times this is the floor a pod
// CIDR must cover, and one more node costs another slice of it.
const podsPerNode = 110

// refreshCapacityStatus publishes the platform capacity and the pod network
// budget in status. It is deliberate about when it spends a vCD call: when
// there is no snapshot, when the pool's observed size differs from the one the
// snapshot was taken at (a scale operation completed), or when the snapshot is
// older than capacityStatusTTL.
func (r *WorkerPoolReconciler) refreshCapacityStatus(ctx context.Context, pool *infrav1.K8sWorkerPool) {
	// spec.capacity.preflight: false is an operator saying "do not read the
	// vDC capacity from this pool" - usually because the call is expensive or
	// unreliable there. The snapshot follows the same switch; the page then
	// says it has no numbers instead of quietly costing a call per pass.
	if !capacityPreflightEnabled(pool) {
		return
	}
	cur := pool.Status.Capacity
	if cur != nil &&
		cur.ObservedReplicas == pool.Status.ObservedReplicas &&
		time.Since(cur.At.Time) < capacityStatusTTL {
		return
	}

	snap := &infrav1.CapacityStatus{ObservedReplicas: pool.Status.ObservedReplicas}

	prov, err := r.platform(pool)
	if err == nil {
		if capacity, cerr := prov.Capacity(ctx); cerr == nil {
			snap.Model = capacity.Model
			snap.CPUMHz = capacity.CPU.Used
			snap.CPULimitMHz = capacity.CPU.Limit
			snap.MemoryMB = capacity.MemoryMB.Used
			snap.MemoryLimitMB = capacity.MemoryMB.Limit
			// One profile, never a sum: profiles have their own limits, and
			// adding them describes a quota that does not exist. The default
			// is the one a clone lands on.
			chosen := -1
			for i := range capacity.Storage {
				if capacity.Storage[i].Default {
					chosen = i
					break
				}
			}
			if chosen < 0 && len(capacity.Storage) > 0 {
				chosen = 0
			}
			if chosen >= 0 {
				snap.StorageName = capacity.Storage[chosen].Name
				snap.StorageMB = capacity.Storage[chosen].UsedMB
				snap.StorageLimitMB = capacity.Storage[chosen].LimitMB
			}
		}
	}

	budget := r.podBudget(ctx)
	snap.PodCIDR = budget.CIDR
	snap.PodIPs = budget.IPs
	snap.PodNodes = budget.Nodes
	snap.PodRemainingNodes = budget.RemainingNodes
	snap.PodBlocks = budget.Blocks
	snap.PodBlocksUsed = budget.BlocksUsed

	snap.At = metav1.Now()
	pool.Status.Capacity = snap
}

// podBudget answers the question the page asks: how many more nodes does the
// pod network still have room for.
//
// The arithmetic is the operator's, not the CNI's: every node is sized for
// podsPerNode pods, so the nodes the CIDR can carry are its address count
// divided by podsPerNode, and what is left after the nodes that exist (masters
// included, they run pods too) is how many may be added. It deliberately does
// not depend on how a particular CNI slices the range.
//
// Blocks is the finer-grained second opinion - the per-node blocks the nodes
// actually hold - because a range can have addresses left and still have no
// block to hand out.
type podBudget struct {
	CIDR           string
	IPs            int64
	Nodes          int32
	RemainingNodes int32
	Blocks         int32
	BlocksUsed     int32
}

func (r *WorkerPoolReconciler) podBudget(ctx context.Context) podBudget {
	var out podBudget

	cfg := &corev1.ConfigMap{}
	if err := r.Get(ctx, client.ObjectKey{Namespace: "kube-system", Name: "kubeadm-config"}, cfg); err != nil {
		return out
	}
	var kubeadm struct {
		Networking struct {
			PodSubnet string `json:"podSubnet"`
		} `json:"networking"`
	}
	if err := yaml.Unmarshal([]byte(cfg.Data["ClusterConfiguration"]), &kubeadm); err != nil {
		return out
	}
	subnet := strings.TrimSpace(kubeadm.Networking.PodSubnet)
	if subnet == "" {
		return out
	}
	out.CIDR = subnet

	_, network, err := net.ParseCIDR(subnet)
	if err != nil || network.IP.To4() == nil {
		return out
	}
	ones, bits := network.Mask.Size()
	if bits != 32 {
		return out
	}
	out.IPs = int64(1) << uint(32-ones)

	nodes := &corev1.NodeList{}
	if err := r.List(ctx, nodes); err != nil {
		return out
	}
	out.Nodes = int32(len(nodes.Items))

	// Masters run pods too, and the kubelet is sized the same, so they cost a
	// slice of the range exactly like a worker.
	capacityNodes := out.IPs / podsPerNode
	remaining := capacityNodes - int64(out.Nodes)
	if remaining < 0 {
		remaining = 0
	}
	out.RemainingNodes = int32(remaining)

	// The block view: what the CNI has already handed out, one block per node.
	blocks := map[string]bool{}
	blockSize := 0
	for i := range nodes.Items {
		n := &nodes.Items[i]
		add := func(cidr string) {
			if cidr == "" {
				return
			}
			_, block, err := net.ParseCIDR(cidr)
			if err != nil {
				return
			}
			blocks[block.String()] = true
			if b, _ := block.Mask.Size(); b > ones && b < 32 {
				blockSize = b
			}
		}
		add(n.Spec.PodCIDR)
		for _, cidr := range n.Spec.PodCIDRs {
			add(cidr)
		}
	}
	if blockSize > 0 {
		out.Blocks = int32(1) << uint(blockSize-ones)
		out.BlocksUsed = int32(len(blocks))
	}
	return out
}
