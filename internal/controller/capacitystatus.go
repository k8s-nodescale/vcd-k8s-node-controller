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

// podSubnetMask is the size of the block kube-controller-manager hands to one
// node, i.e. how many /24s the pod network holds. kubeadm's default
// (node-cidr-mask-size) is 24 for IPv4.
const podSubnetMask = 24

// refreshCapacityStatus publishes the platform capacity and the pod subnet
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

	snap.PodSubnet, snap.PodSubnetsTotal, snap.PodSubnetsUsed = r.podSubnetBudget(ctx)
	snap.At = metav1.Now()
	pool.Status.Capacity = snap
}

// podSubnetBudget answers "can another worker join at all". The cluster's pod
// network comes from the kubeadm ConfigMap, and the blocks already handed out
// are the podCIDRs on the Nodes - one per node for every CNI that uses
// kube-controller-manager's allocator, which is what a kubeadm cluster does.
func (r *WorkerPoolReconciler) podSubnetBudget(ctx context.Context) (string, int32, int32) {
	cfg := &corev1.ConfigMap{}
	if err := r.Get(ctx, client.ObjectKey{Namespace: "kube-system", Name: "kubeadm-config"}, cfg); err != nil {
		return "", 0, 0
	}
	var kubeadm struct {
		Networking struct {
			PodSubnet string `json:"podSubnet"`
		} `json:"networking"`
	}
	if err := yaml.Unmarshal([]byte(cfg.Data["ClusterConfiguration"]), &kubeadm); err != nil {
		return "", 0, 0
	}
	subnet := strings.TrimSpace(kubeadm.Networking.PodSubnet)
	if subnet == "" {
		return "", 0, 0
	}
	_, network, err := net.ParseCIDR(subnet)
	if err != nil || network.IP.To4() == nil {
		return subnet, 0, 0
	}
	ones, bits := network.Mask.Size()
	if bits != 32 || ones > podSubnetMask {
		return subnet, 1, 1
	}
	total := int32(1) << uint(podSubnetMask-ones)

	nodes := &corev1.NodeList{}
	if err := r.List(ctx, nodes); err != nil {
		return subnet, total, 0
	}
	seen := map[string]bool{}
	for i := range nodes.Items {
		n := &nodes.Items[i]
		add := func(cidr string) {
			if cidr == "" {
				return
			}
			if _, block, err := net.ParseCIDR(cidr); err == nil {
				seen[block.String()] = true
			}
		}
		add(n.Spec.PodCIDR)
		for _, cidr := range n.Spec.PodCIDRs {
			add(cidr)
		}
	}
	return subnet, total, int32(len(seen))
}
