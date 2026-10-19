// Package vcd implements cloud.Provider on top of the official go-vcloud-director SDK.
package vcd

import (
	"context"
	"errors"
	"fmt"
	"net/url"
	"strings"

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
	endpoint, err := normalizeURL(cfg.URL)
	if err != nil {
		return nil, err
	}
	u, err := url.Parse(endpoint)
	if err != nil {
		return nil, err
	}
	client := govcd.NewVCDClient(*u, cfg.Insecure)
	if err := client.Authenticate(cfg.User, cfg.Password, cfg.Org); err != nil {
		return nil, err
	}
	return &Provider{cfg: cfg, client: client}, nil
}

// normalizeURL returns the vCD API endpoint expected by govcd: a single
// http(s) scheme, a non-empty host, and a path ending in /api.
//
// It tolerates common misconfigurations such as "iaas.cloud.mts.ru",
// "https://iaas.cloud.mts.ru" and the doubly-prefixed
// "https://https://iaas.cloud.mts.ru/api/api".
func normalizeURL(raw string) (string, error) {
	s := strings.TrimSpace(raw)
	if s == "" {
		return "", errors.New("VCD_URL is empty")
	}
	if !strings.Contains(s, "://") {
		s = "https://" + s
	}

	u, err := url.Parse(s)
	if err != nil {
		return "", fmt.Errorf("invalid VCD_URL %q: %w", raw, err)
	}

	// url.Parse("https://https://host/api") yields Host "https:" and
	// Path "//host/api"; unwrap repeated schemes until the real host remains.
	for host := u.Hostname(); host == "http" || host == "https"; host = u.Hostname() {
		rest := strings.TrimPrefix(u.Path, "//")
		u, err = url.Parse(u.Scheme + "://" + rest)
		if err != nil {
			return "", fmt.Errorf("invalid VCD_URL %q: %w", raw, err)
		}
	}

	if u.Scheme != "http" && u.Scheme != "https" {
		return "", fmt.Errorf("invalid VCD_URL %q: unsupported scheme %q", raw, u.Scheme)
	}
	if u.Host == "" {
		return "", fmt.Errorf("invalid VCD_URL %q: missing host", raw)
	}

	u.Path = strings.TrimRight(u.Path, "/")
	for strings.HasSuffix(u.Path, "/api/api") {
		u.Path = strings.TrimSuffix(u.Path, "/api")
	}
	if !strings.HasSuffix(u.Path, "/api") {
		u.Path += "/api"
	}
	return u.String(), nil
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
