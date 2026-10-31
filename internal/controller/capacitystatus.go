package controller

import (
	"context"
	"net"
	"strings"
	"time"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"
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
	snap.PodRemainingNodes = budget.HighDensityNodes()
	snap.PodBlocks = budget.Blocks
	snap.PodBlocksUsed = budget.BlocksUsed
	snap.PodBlockSize = budget.BlockSize
	snap.PodUsablePerBlock = budget.Usable
	snap.PodMaxPerNode = budget.MaxPerNode

	snap.At = metav1.Now()
	pool.Status.Capacity = snap
}

// podBudget is the pod network as the CNI sees it.
//
// The dashboard offers two readings of the same pool, because "how many nodes
// fit" depends on what the operator intends to put on a node:
//
//   - current usage: blocks per node is what today's busiest block needs, so a
//     pool sized for the load that exists;
//   - high density: every node may run podsPerNode pods, so a node needs
//     ceil(podsPerNode / usable addresses) blocks.
//
// Both are exact once Blocks, Usable and BlockSize are known; only the blocks
// per node differs.
type podBudget struct {
	CIDR        string
	IPs         int64
	Nodes       int32
	BlockSize   int32
	Usable      int32
	Blocks      int32
	BlocksUsed  int32
	MaxPerNode  int32
	HasCalico   bool
	BlocksPerNo int32 // blocks a node needs at podsPerNode pods
}

// HighDensityNodes is how many nodes the pool carries when every node may run
// podsPerNode pods.
func (b podBudget) HighDensityNodes() int32 {
	if b.BlocksPerNo <= 0 || b.Blocks <= 0 {
		return 0
	}
	n := b.Blocks / b.BlocksPerNo
	if n < b.Nodes {
		return 0
	}
	return n - b.Nodes
}

func (r *WorkerPoolReconciler) podBudget(ctx context.Context) podBudget {
	var out podBudget

	// The cluster's own idea of the pod network: kubeadm's podSubnet. It is
	// the fallback when the CNI does not publish blocks.
	cfg := &corev1.ConfigMap{}
	if err := r.Get(ctx, client.ObjectKey{Namespace: "kube-system", Name: "kubeadm-config"}, cfg); err == nil {
		var kubeadm struct {
			Networking struct {
				PodSubnet string `json:"podSubnet"`
			} `json:"networking"`
		}
		if yaml.Unmarshal([]byte(cfg.Data["ClusterConfiguration"]), &kubeadm) == nil {
			out.CIDR = strings.TrimSpace(kubeadm.Networking.PodSubnet)
		}
	}
	if _, network, err := net.ParseCIDR(out.CIDR); err == nil && network.IP.To4() != nil {
		if ones, bits := network.Mask.Size(); bits == 32 {
			out.IPs = int64(1) << uint(32-ones)
		}
	}

	nodes := &corev1.NodeList{}
	if err := r.List(ctx, nodes); err == nil {
		out.Nodes = int32(len(nodes.Items))
	}

	if r.calicoBudget(ctx, &out) {
		return out
	}

	// No Calico blocks to read: the coarse estimate, whole blocks assumed to
	// pack perfectly. The page marks the number as an estimate in that case.
	if out.IPs > 0 {
		out.Usable = podsPerNode
		out.BlockSize = 0
		out.Blocks = int32(out.IPs / podsPerNode)
		out.BlocksUsed = out.Nodes
		out.BlocksPerNo = 1
	}
	return out
}

// calicoBudget reads the pool and the blocks Calico has handed out. It reports
// false when the CRDs are not there (another CNI, or no permission), and the
// caller falls back to the address arithmetic.
func (r *WorkerPoolReconciler) calicoBudget(ctx context.Context, out *podBudget) bool {
	pools := &unstructured.UnstructuredList{}
	pools.SetGroupVersionKind(schema.GroupVersionKind{
		Group: "crd.projectcalico.org", Version: "v1", Kind: "IPPoolList",
	})
	if err := r.List(ctx, pools); err != nil || len(pools.Items) == 0 {
		return false
	}

	// The pool that carries workload addresses. Several pools are legal (one
	// per tenant, or a second for a different encapsulation); the first that
	// contains the kubeadm podSubnet is the one nodes draw from, else the
	// first workload pool.
	var cidr string
	var blockSize, prefix int
	for i := range pools.Items {
		spec, _, _ := unstructured.NestedMap(pools.Items[i].Object, "spec")
		if spec == nil {
			continue
		}
		uses, _, _ := unstructured.NestedStringSlice(pools.Items[i].Object, "spec", "allowedUses")
		workload := len(uses) == 0
		for _, u := range uses {
			if u == "Workload" {
				workload = true
			}
		}
		if !workload {
			continue
		}
		c, _ := spec["cidr"].(string)
		bs, _ := spec["blockSize"].(int64)
		_, network, err := net.ParseCIDR(c)
		if err != nil || network.IP.To4() == nil {
			continue
		}
		ones, _ := network.Mask.Size()
		if out.CIDR != "" {
			if _, sub, err := net.ParseCIDR(out.CIDR); err == nil && !sub.Contains(network.IP) {
				continue
			}
		}
		cidr, blockSize, prefix = c, int(bs), ones
		break
	}
	if cidr == "" || blockSize <= 0 || blockSize >= 32 {
		return false
	}

	out.CIDR = cidr
	out.IPs = int64(1) << uint(32-prefix)
	out.BlockSize = int32(blockSize)
	out.Usable = int32(1)<<uint(32-blockSize) - 1 // Calico keeps one address per block
	out.Blocks = int32(1) << uint(blockSize-prefix)
	out.HasCalico = true

	blocks := &unstructured.UnstructuredList{}
	blocks.SetGroupVersionKind(schema.GroupVersionKind{
		Group: "crd.projectcalico.org", Version: "v1", Kind: "IPAMBlockList",
	})
	if err := r.List(ctx, blocks); err == nil {
		out.BlocksUsed = int32(len(blocks.Items))
		for i := range blocks.Items {
			allocs, _, _ := unstructured.NestedSlice(blocks.Items[i].Object, "spec", "allocations")
			var used int32
			for _, a := range allocs {
				if a != nil {
					used++
				}
			}
			// The block's first address is marked by Calico itself, so the
			// pods it holds is the count minus that one.
			if used > 0 {
				used--
			}
			if used > out.MaxPerNode {
				out.MaxPerNode = used
			}
		}
	}

	if out.Usable > 0 {
		out.BlocksPerNo = (podsPerNode + out.Usable - 1) / out.Usable
		if out.BlocksPerNo < 1 {
			out.BlocksPerNo = 1
		}
	}
	return true
}
