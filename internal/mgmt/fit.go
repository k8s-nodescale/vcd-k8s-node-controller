package mgmt

import (
	infrav1 "github.com/k8s-nodescale/vcd-k8s-node-controller/api/v1alpha1"
)

// poolFit answers the question the page is read for: how many more workers of
// this pool's flavour does the platform still have room for?
//
// Each dimension is charged the way it is actually limited, which is why the
// three numbers are not interchangeable:
//
//   - CPU is charged at the core speed the pre-flight uses, divided by the
//     pool's overcommit assumption (status carries both, so the page explains
//     the same arithmetic the gate applies). A vCPU can be oversubscribed;
//   - memory is charged the flavour's allocation in full, because a VM that
//     cannot get its pages stalls - there is no "busy but fine" here;
//   - storage is charged the provisioned disks (the OS disk plus the data
//     disks), not what the guest has written: thin disks only grow, and the
//     datastore does not shrink.
//
// A nil number means the dimension is not measurable or not limited; the page
// shows that as unknown instead of as "no room".
func poolFit(p *infrav1.K8sWorkerPool) *Fit {
	c := p.Status.Capacity
	if c == nil {
		return nil
	}
	out := &Fit{}

	// CPU
	if c.CPULimitMHz > 0 && c.CPUMHzPerCore > 0 && p.Spec.Flavor.CPUs > 0 {
		ratio := int64(perCoreFor(p, c))
		perCore := c.CPUMHzPerCore
		if ratio > 1 {
			// Rounded up: the charge is never optimistic.
			perCore = (perCore + ratio - 1) / ratio
		}
		per := int64(p.Spec.Flavor.CPUs) * perCore
		if per > 0 {
			out.CPUPerWorkerMHz = per
			if free := c.CPULimitMHz - c.CPUMHz; free >= 0 {
				n := int32(free / per)
				out.CPU = &n
			}
		}
	}

	// Memory
	if c.MemoryLimitMB > 0 && p.Spec.Flavor.MemoryMB > 0 {
		per := int64(p.Spec.Flavor.MemoryMB)
		out.MemoryPerWorkerMB = per
		if free := c.MemoryLimitMB - c.MemoryMB; free >= 0 {
			n := int32(free / per)
			out.Memory = &n
		}
	}

	// Storage: what a clone writes, worst case.
	if c.StorageLimitMB > 0 {
		gb := p.Spec.Flavor.OSDiskGB
		for _, d := range p.Spec.Flavor.Disks {
			gb += d.SizeGB
		}
		if gb > 0 {
			per := int64(gb) * 1024
			out.DiskPerWorkerMB = per
			if free := c.StorageLimitMB - c.StorageMB; free >= 0 {
				n := int32(free / per)
				out.Storage = &n
			}
		}
	}
	return out
}

// perCoreFor is the overcommit assumption in force, by the same rule the
// controller's pre-flight applies: a physical capacity may be overcommitted, a
// quota may not.
//
// It is read from the spec rather than from the snapshot because raising it must
// show on the next collection: the snapshot's own copy only refreshes when its
// TTL expires, and a switch that takes ten minutes to do anything is a switch
// that looks broken.
func perCoreFor(p *infrav1.K8sWorkerPool, c *infrav1.CapacityStatus) int32 {
	if c == nil || !c.Physical {
		return 1
	}
	if n := p.Spec.Capacity.CPUPerCore; n != nil && *n > 1 {
		return *n
	}
	return 1
}
