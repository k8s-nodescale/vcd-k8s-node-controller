// Package vsphere is the stub of the vSphere platform provider.
//
// It exists so the second implementation is a matter of filling in these
// methods rather than reshaping the controller: it satisfies the cloud
// contract, so spec.provider: vsphere is wired end to end, and every call
// refuses with cloud.ErrNotImplemented until the platform API calls are
// written. The controller's validation says so before a pool is reconciled.
//
// The shape of the implementation: a worker is cloned from the placement's
// template into its resource pool, customised through the guestinfo properties
// instead of a NoCloud seed, and resized by reconfiguring the VM while it is
// powered off. It implements cloud.Provider only: vSphere has no
// load-balancer pool, so cloud.PoolMembership stays with the operator.
package vsphere

import (
	"context"
	"fmt"

	"github.com/k8s-nodescale/vcd-k8s-node-controller/internal/cloud"
)

// Client is the cloud.Factory for vSphere.
type Client struct{}

// Provider is one pool's vSphere client, bound to its placement.
type Provider struct {
	placement cloud.VSpherePlacement
}

var (
	_ cloud.Factory  = (*Client)(nil)
	_ cloud.Provider = (*Provider)(nil)
)

// New returns the vSphere factory.
func New() *Client { return &Client{} }

// For returns the provider for one pool.
func (c *Client) For(placement cloud.Placement) (cloud.Provider, error) {
	if placement.Provider != cloud.ProviderVSphere {
		return nil, fmt.Errorf("vsphere: placement names provider %q", placement.Provider)
	}
	if placement.VSphere == nil {
		return nil, fmt.Errorf("vsphere: the placement carries no spec.vsphere")
	}
	return &Provider{placement: *placement.VSphere}, nil
}

// ListWorkerVMs returns the pool's VMs.
func (p *Provider) ListWorkerVMs(ctx context.Context, selector map[string]string) ([]cloud.VM, error) {
	return nil, cloud.ErrNotImplemented
}

// GetVM reads one VM by id.
func (p *Provider) GetVM(ctx context.Context, id string) (cloud.VM, error) {
	return cloud.VM{}, cloud.ErrNotImplemented
}

// CreateVM clones a worker VM.
func (p *Provider) CreateVM(ctx context.Context, spec cloud.CreateVMSpec) (cloud.VM, error) {
	return cloud.VM{}, cloud.ErrNotImplemented
}

// DeleteVM destroys a worker VM.
func (p *Provider) DeleteVM(ctx context.Context, id string) error { return cloud.ErrNotImplemented }

// PowerOffVM stops a worker VM.
func (p *Provider) PowerOffVM(ctx context.Context, id string) error { return cloud.ErrNotImplemented }

// PowerOnVM starts a worker VM.
func (p *Provider) PowerOnVM(ctx context.Context, id string) error { return cloud.ErrNotImplemented }

// CycleVM power-cycles a worker VM.
func (p *Provider) CycleVM(ctx context.Context, id string) error { return cloud.ErrNotImplemented }

// ResizeVM reconfigures a powered-off worker VM.
func (p *Provider) ResizeVM(ctx context.Context, id string, cpus, memoryMB, osDiskGB int32) (cloud.VM, error) {
	return cloud.VM{}, cloud.ErrNotImplemented
}

// Capacity reads the cluster's free capacity.
func (p *Provider) Capacity(ctx context.Context) (cloud.PlatformCapacity, error) {
	return cloud.PlatformCapacity{}, cloud.ErrNotImplemented
}
