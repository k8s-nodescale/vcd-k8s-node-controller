// Package vcd implements cloud.Provider on top of the official go-vcloud-director SDK.
package vcd

import (
	"context"
	"net/url"

	"github.com/vmware/go-vcloud-director/v2/govcd"

	"github.com/werton13/vcd-k8s-node-controller/internal/cloud"
)

// Config holds vCD connection settings.
type Config struct {
	URL      string
	User     string
	Password string
	Org      string
	VDC      string
	Insecure bool
}

// Provider is a govcd-backed cloud.Provider.
type Provider struct {
	cfg    Config
	client *govcd.VCDClient
}

var _ cloud.Provider = (*Provider)(nil)

// New connects to vCD and authenticates.
func New(cfg Config) (*Provider, error) {
	u, err := url.Parse(cfg.URL)
	if err != nil {
		return nil, err
	}
	client := govcd.NewVCDClient(*u, cfg.Insecure)
	if err := client.Authenticate(cfg.User, cfg.Password, cfg.Org); err != nil {
		return nil, err
	}
	return &Provider{cfg: cfg, client: client}, nil
}

// ListWorkerVMs TODO: query the vApp for VMs matching selector metadata.
// Suggested govcd calls: VCDClient.GetVAppByName / vapp.GetAllVms, vm.GetMetadata.
func (p *Provider) ListWorkerVMs(ctx context.Context, vapp string, selector map[string]string) ([]cloud.VM, error) {
	return nil, cloud.ErrNotImplemented
}

// CreateVM TODO: vapp.AddNewVM -> attach Org network (MANUAL IP) -> set CPU/mem ->
// AddInternalDisk -> SetExtraConfig("disk.EnableUUID","TRUE") -> set guest
// customization (initscript) -> SetMetadata -> PowerOn.
func (p *Provider) CreateVM(ctx context.Context, spec cloud.CreateVMSpec) (cloud.VM, error) {
	return cloud.VM{}, cloud.ErrNotImplemented
}

// DeleteVM TODO: undeploy/power-off then vapp.RemoveVM(force=true, recursive=true).
func (p *Provider) DeleteVM(ctx context.Context, id string) error {
	return cloud.ErrNotImplemented
}

// AddPoolMember TODO: NSX-V edge.UpdateLBServerPool / NSX-T NsxtAlbPool.Update.
func (p *Provider) AddPoolMember(ctx context.Context, pool cloud.PoolRef, ip string) error {
	return cloud.ErrNotImplemented
}

// RemovePoolMember TODO: see AddPoolMember.
func (p *Provider) RemovePoolMember(ctx context.Context, pool cloud.PoolRef, ip string) error {
	return cloud.ErrNotImplemented
}
