package controller

import (
	"context"
	"fmt"
	"strings"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/log"

	infrav1 "github.com/k8s-nodescale/vcd-k8s-node-controller/api/v1alpha1"
	"github.com/k8s-nodescale/vcd-k8s-node-controller/internal/cloud"
	"github.com/k8s-nodescale/vcd-k8s-node-controller/internal/ipam"
)

// defaultCPUMHzPerCore converts a vCPU into the MHz a vDC rates it at when
// neither the vDC nor spec.capacity says otherwise: 2 GHz per vCPU.
const defaultCPUMHzPerCore = 2000

// capacityPreflightEnabled reports whether the vDC capacity is read before a
// create or a resize. The zero value means on: a pool written before the field
// existed keeps the protection.
func capacityPreflightEnabled(pool *infrav1.K8sWorkerPool) bool {
	if p := pool.Spec.Capacity.Preflight; p != nil {
		return *p
	}
	return true
}

// cpuMHzPerCore is the speed a vCPU is rated at. The vDC's own setting wins,
// then the pool's assumption, then the usual 2 GHz.
func cpuMHzPerCore(pool *infrav1.K8sWorkerPool, capacity cloud.VDCCapacity) int64 {
	if capacity.CPUMHzPerCore > 0 {
		return capacity.CPUMHzPerCore
	}
	if n := pool.Spec.Capacity.CPUMHzPerCore; n > 0 {
		return n
	}
	return defaultCPUMHzPerCore
}

// capacityAllows runs the pre-flight for one request and returns why it does not
// fit when it does not.
//
// A capacity read that fails never blocks the pool: vCD is still the authority,
// and attempting the create (the behaviour before the pre-flight existed) is
// better than a pool that cannot scale because a read broke.
func (r *WorkerPoolReconciler) capacityAllows(ctx context.Context, pool *infrav1.K8sWorkerPool, req cloud.CapacityRequest) (bool, string) {
	if !capacityPreflightEnabled(pool) {
		return true, ""
	}
	capacity, err := r.Cloud.Capacity(ctx)
	if err != nil {
		log.FromContext(ctx).V(1).Info("capacity pre-flight skipped: the vDC capacity could not be read",
			"pool", pool.Name, "error", err)
		return true, ""
	}
	ok, short := capacity.Fits(req, cpuMHzPerCore(pool, capacity))
	if ok {
		return true, ""
	}
	return false, strings.Join(short, "; ")
}

// workerCapacityRequest is the capacity one new worker of this pool adds.
func workerCapacityRequest(pool *infrav1.K8sWorkerPool, nodes []infrav1.WorkerNodeStatus) cloud.CapacityRequest {
	req := cloud.CapacityRequest{CPUs: pool.Spec.Flavor.CPUs, MemoryMB: pool.Spec.Flavor.MemoryMB}
	osGB := pool.Spec.Flavor.OSDiskGB
	if osGB <= 0 {
		osGB = siblingOSDiskGB(nodes)
	}
	req.StorageMB = map[string]int64{"": int64(osGB) * 1024}
	for _, d := range pool.Spec.Flavor.Disks {
		req.StorageMB[d.StorageProfile] += int64(d.SizeGB) * 1024
	}
	return req
}

// siblingOSDiskGB estimates the system disk of a new worker from a worker that
// already exists. spec.flavor.osDiskGB is exact when it is set; when it is not,
// the clone keeps the template size, and a worker of the same pool and template
// is the closest number the controller has without another vCD call. Zero means
// the pool has no worker yet and the template disk is not counted.
func siblingOSDiskGB(nodes []infrav1.WorkerNodeStatus) int32 {
	var gb int32
	for i := range nodes {
		n := &nodes[i]
		if n.Flavor == nil || n.Flavor.OSDiskGB <= 0 ||
			n.Phase == infrav1.NodePhaseQueued || n.Phase == infrav1.NodePhaseMissing {
			continue
		}
		if n.Flavor.OSDiskGB > gb {
			gb = n.Flavor.OSDiskGB
		}
	}
	return gb
}

// resizeCapacityRequest is the extra capacity a worker needs to reach target.
// A resize only ever grows the system disk; the data disks are not part of it.
func resizeCapacityRequest(have, target *infrav1.FlavorStatus) cloud.CapacityRequest {
	var req cloud.CapacityRequest
	if have == nil || target == nil {
		return req
	}
	if target.CPUs > have.CPUs {
		req.CPUs = target.CPUs - have.CPUs
	}
	if target.MemoryMB > have.MemoryMB {
		req.MemoryMB = target.MemoryMB - have.MemoryMB
	}
	if d := target.OSDiskGB - have.OSDiskGB; d > 0 {
		req.StorageMB = map[string]int64{"": int64(d) * 1024}
	}
	return req
}

// addCapacityRequest sums two requests, so a batch can be checked as one.
func addCapacityRequest(a, b cloud.CapacityRequest) cloud.CapacityRequest {
	out := cloud.CapacityRequest{CPUs: a.CPUs + b.CPUs, MemoryMB: a.MemoryMB + b.MemoryMB}
	if len(a.StorageMB) == 0 && len(b.StorageMB) == 0 {
		return out
	}
	out.StorageMB = make(map[string]int64, len(a.StorageMB)+len(b.StorageMB))
	for k, v := range a.StorageMB {
		out.StorageMB[k] += v
	}
	for k, v := range b.StorageMB {
		out.StorageMB[k] += v
	}
	return out
}

// applyCreatedWorker records a VM that now exists in a slot. The flavor falls
// back to the spec when vCD has not reported the shape yet.
func applyCreatedWorker(n *infrav1.WorkerNodeStatus, vm cloud.VM, tokID string, flavor infrav1.NodeFlavor) {
	cpu, memMB := vm.CPU, vm.MemoryMB
	if cpu == 0 {
		cpu = flavor.CPUs
	}
	if memMB == 0 {
		memMB = flavor.MemoryMB
	}
	n.ProviderID = vm.ID
	n.Phase = infrav1.NodePhaseProvisioning
	n.Message = ""
	n.CPU = cpu
	n.MemoryMB = memMB
	n.DiskGB = vm.DiskGB
	n.Flavor = &infrav1.FlavorStatus{CPUs: vm.CPU, MemoryMB: vm.MemoryMB, OSDiskGB: vm.OSDiskGB}
	n.BootstrapTokenID = tokID
}

// queueSlots parks count slots as Queued. They have no VM, so the address and
// the name stay reserved and the ledger is published with them: the pool keeps
// reporting the request instead of silently reading the shortfall as "nothing
// to do".
func (r *WorkerPoolReconciler) queueSlots(ctx context.Context, pool *infrav1.K8sWorkerPool, alloc *ipam.Allocator, count int32, why string) error {
	for i := int32(0); i < count; i++ {
		ip, err := alloc.Allocate()
		if err != nil {
			return err
		}
		ordinal, err := alloc.Ordinal(ip)
		if err != nil {
			_ = alloc.Free(ip)
			return err
		}
		pool.Status.Nodes = append(pool.Status.Nodes, infrav1.WorkerNodeStatus{
			Name:     workerName(pool.Spec.NamePrefix, ordinal),
			IP:       ip,
			Phase:    infrav1.NodePhaseQueued,
			Message:  "waiting for vDC capacity: " + why,
			CPU:      pool.Spec.Flavor.CPUs,
			MemoryMB: pool.Spec.Flavor.MemoryMB,
			Flavor: &infrav1.FlavorStatus{
				CPUs:     pool.Spec.Flavor.CPUs,
				MemoryMB: pool.Spec.Flavor.MemoryMB,
				OSDiskGB: pool.Spec.Flavor.OSDiskGB,
			},
		})
	}
	pool.Status.ObservedReplicas = int32(len(pool.Status.Nodes))
	pool.Status.Ledger = toStatusLedger(alloc.Ledger())
	log.FromContext(ctx).Info("workers queued for vDC capacity", "pool", pool.Name, "queued", count, "reason", why)
	return nil
}

// resumeQueuedCreates starts the slots the capacity pre-flight parked as Queued.
// A queued slot has no VM, no Node and no token, so it is simply retried: the
// same name and address are used, and nothing about the slot changes until the
// vDC has room. It returns handled=true once a create was attempted, which keeps
// the pass from also running the replica arithmetic on the queue.
func (r *WorkerPoolReconciler) resumeQueuedCreates(ctx context.Context, before, pool *infrav1.K8sWorkerPool, alloc *ipam.Allocator) (bool, error) {
	logger := log.FromContext(ctx)
	handled := false
	for i := range pool.Status.Nodes {
		n := &pool.Status.Nodes[i]
		if n.Phase != infrav1.NodePhaseQueued || n.DeleteStartedAt != nil {
			continue
		}
		if ok, why := r.capacityAllows(ctx, pool, workerCapacityRequest(pool, pool.Status.Nodes)); !ok {
			// One read per pass is enough: every queued slot asks for the same
			// worker shape, so a shortfall here is a shortfall for the rest of
			// the queue too. Refresh their report and wait.
			msg := "waiting for vDC capacity: " + why
			for j := i; j < len(pool.Status.Nodes); j++ {
				if q := &pool.Status.Nodes[j]; q.Phase == infrav1.NodePhaseQueued && q.DeleteStartedAt == nil {
					q.Message = msg
				}
			}
			return handled, nil
		}
		// A queued slot is published as creating before the VM exists: the
		// create takes minutes, and the slot would otherwise keep reporting
		// the shortfall that parked it. The phase stays Queued, so a crash
		// mid-create is retried by the next pass instead of reading Missing.
		n.Message = "capacity is available; creating the VM"
		if err := r.flush(ctx, before, pool); err != nil {
			return true, err
		}
		// The write above replaced status.nodes, so the pointer into it is
		// stale: take it again before recording the VM.
		n = &pool.Status.Nodes[i]

		vm, tokID, err := r.createWorker(ctx, pool, alloc, n.Name, n.IP)
		if err != nil {
			n.Message = "the create failed: " + err.Error()
			logger.Error(err, "start a queued worker", "worker", n.Name)
			r.eventf(pool, corev1.EventTypeWarning, "WorkerQueuedCreateFailed",
				fmt.Sprintf("worker %s (%s): %v", n.Name, n.IP, err))
			return true, err
		}
		now := metav1.Now()
		n.ProvisioningStartedAt = &now
		applyCreatedWorker(n, vm, tokID, pool.Spec.Flavor)
		pool.Status.Ledger = toStatusLedger(alloc.Ledger())
		// Publish the VM id before the next create: a restart adopts the VM
		// instead of creating a second one in the same slot.
		if err := r.flush(ctx, before, pool); err != nil {
			return true, err
		}
		logger.Info("started a queued worker", "worker", n.Name, "ip", n.IP, "providerID", vm.ID)
		r.eventf(pool, corev1.EventTypeNormal, "WorkerDequeued",
			fmt.Sprintf("vDC capacity is available again; worker %s (%s) is being created", n.Name, n.IP))
		handled = true
	}
	return handled, nil
}
