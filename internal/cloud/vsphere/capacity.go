package vsphere

import (
	"context"
	"fmt"
	"sort"

	"github.com/vmware/govmomi/vim25/mo"
	"github.com/vmware/govmomi/vim25/types"

	"github.com/k8s-nodescale/vcd-k8s-node-controller/internal/cloud"
)

// Capacity reads what the placement still has free, in the terms of the same
// configuration block the create uses:
//
//   - compute is the named compute cluster. Its hosts are the pool capacity;
//     a host of another cluster is not a candidate for a worker, so counting
//     it would report room the pool cannot use. Limit is the summed physical
//     MHz and MB of the usable hosts, Used is what vCenter reports those hosts
//     consuming right now.
//   - storage is the named datastore, the one the clone writes the disks to.
//     It is reported as the default profile, because that is what a request
//     without a profile name means.
//
// A host in maintenance mode or disconnected is left out of both totals: it
// cannot take a clone. A cluster with no usable host is an error, not a zero
// capacity, so the caller parks the request instead of reading "no room".
func (p *Provider) Capacity(ctx context.Context) (cloud.PlatformCapacity, error) {
	ctx, cancel := p.client.opCtx(ctx)
	defer cancel()
	if err := ctx.Err(); err != nil {
		return cloud.PlatformCapacity{}, err
	}

	cl, f, err := p.base(ctx)
	if err != nil {
		return cloud.PlatformCapacity{}, err
	}
	cluster, err := p.cluster(ctx, f)
	if err != nil {
		return cloud.PlatformCapacity{}, err
	}
	ds, err := p.datastore(ctx, f)
	if err != nil {
		return cloud.PlatformCapacity{}, err
	}

	hosts, err := cluster.Hosts(ctx)
	if err != nil {
		return cloud.PlatformCapacity{}, fmt.Errorf("vsphere: hosts of cluster %q: %w", p.placement.Cluster, err)
	}
	model := "vSphere " + p.placement.Cluster
	if p.placement.Host != "" {
		// A pinned host is the only host a worker can use, so it is the only
		// host whose room is real for this pool. Counting the whole cluster
		// would tell the caller there is space where the create can never go.
		pinned := hosts[:0:0]
		for _, h := range hosts {
			if hostMatches(h, p.placement.Host) {
				pinned = append(pinned, h)
			}
		}
		if len(pinned) == 0 {
			return cloud.PlatformCapacity{}, fmt.Errorf(
				"vsphere: spec.vsphere.host %q is not a host of cluster %q: "+
					"the host is a refinement of the cluster, so it cannot name a machine outside it",
				p.placement.Host, p.placement.Cluster)
		}
		hosts = pinned
		model = model + "/" + hostLeaf(p.placement.Host)
	}
	refs := make([]types.ManagedObjectReference, 0, len(hosts))
	for _, h := range hosts {
		refs = append(refs, h.Reference())
	}
	var mhs []mo.HostSystem
	if len(refs) > 0 {
		if err := cl.Retrieve(ctx, refs, []string{"name", "summary"}, &mhs); err != nil {
			return cloud.PlatformCapacity{}, fmt.Errorf("vsphere: read hosts of cluster %q: %w", p.placement.Cluster, err)
		}
	}
	// Sorted so CPUMHzPerCore is the same number on every pass when the hosts
	// of a cluster do not report the same speed.
	sort.Slice(mhs, func(i, j int) bool { return mhs[i].Name < mhs[j].Name })

	out := cloud.PlatformCapacity{Model: model}
	usable := 0
	for i := range mhs {
		m := &mhs[i]
		if !hostUsable(m) {
			continue
		}
		usable++
		if hw := m.Summary.Hardware; hw != nil {
			mhz := int64(hw.CpuMhz)
			out.CPU.Limit += int64(hw.NumCpuCores) * mhz
			out.MemoryMB.Limit += hw.MemorySize / (1024 * 1024)
			if out.CPUMHzPerCore == 0 {
				out.CPUMHzPerCore = mhz
			}
		}
		out.CPU.Used += int64(m.Summary.QuickStats.OverallCpuUsage)
		out.MemoryMB.Used += int64(m.Summary.QuickStats.OverallMemoryUsage)
	}
	if usable == 0 {
		return cloud.PlatformCapacity{}, fmt.Errorf("vsphere: cluster %q has no host that can take a worker", p.placement.Cluster)
	}

	var mds mo.Datastore
	if err := cl.RetrieveOne(ctx, ds.Reference(), []string{"name", "summary"}, &mds); err != nil {
		return cloud.PlatformCapacity{}, fmt.Errorf("vsphere: read datastore %q: %w", p.placement.Datastore, err)
	}
	name := mds.Name
	if name == "" {
		name = mds.Summary.Name
	}
	if mds.Summary.Capacity > 0 {
		const mb = int64(1024 * 1024)
		out.Storage = []cloud.StorageProfileCapacity{{
			Name:    name,
			Default: true,
			LimitMB: mds.Summary.Capacity / mb,
			UsedMB:  (mds.Summary.Capacity - mds.Summary.FreeSpace) / mb,
		}}
	}
	return out, nil
}

// hostUsable reports whether the host can take a clone right now.
func hostUsable(m *mo.HostSystem) bool {
	if rt := m.Summary.Runtime; rt != nil {
		if rt.InMaintenanceMode {
			return false
		}
		if rt.ConnectionState != "" && rt.ConnectionState != types.HostSystemConnectionStateConnected {
			return false
		}
	}
	return m.Summary.Hardware != nil
}
