// Package vcd implements cloud.Provider on top of the official go-vcloud-director SDK.
package vcd

import (
	"context"
	"errors"
	"fmt"
	"net/url"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/vmware/go-vcloud-director/v2/govcd"
	"github.com/vmware/go-vcloud-director/v2/types/v56"

	"sigs.k8s.io/controller-runtime/pkg/log"

	"github.com/k8s-nodescale/vcd-k8s-node-controller/internal/cloud"
)

// Config holds vCD connection settings.
type Config struct {
	URL      string
	User     string
	Password string
	Org      string
	VDC      string
	Insecure bool

	// Timeout bounds every request the SDK makes to vCD. Zero selects
	// defaultHTTPTimeout.
	Timeout time.Duration
}

// defaultHTTPTimeout bounds every vCD API call.
//
// The SDK's http.Client has no timeout by default, so a connection that the
// platform silently drops leaves the call waiting forever. The controller runs
// a single reconcile worker: one such call stopped the pool from converging
// until the pod was restarted (observed on a dev tenant, stuck in GetVMByName
// for minutes while a scale-out was pending).
const defaultHTTPTimeout = 60 * time.Second

// httpTimeout returns the effective per-request timeout.
func httpTimeout(cfg Config) time.Duration {
	if cfg.Timeout > 0 {
		return cfg.Timeout
	}
	return defaultHTTPTimeout
}

// Client is an authenticated vCloud Director connection. It is the cloud
// Factory: it hands out one Pool per placement, so one process serves every
// pool regardless of where it lives.
type Client struct {
	cfg    Config
	client *govcd.VCDClient

	// cpuOnce guards the single admin read of the vDC's CPU speed. The speed is
	// a property of the vDC, not of the pass, and an account that cannot read
	// the admin view must not pay for a failing call on every reconcile.
	cpuOnce sync.Once
	cpuMHz  int64
}

// Pool is one pool's view of vCloud Director, bound to the vApp and org
// network its placement names. Every method works inside that vApp, so none of
// them carries its name.
type Pool struct {
	*Client
	placement cloud.VCDPlacement
}

var (
	_ cloud.Factory        = (*Client)(nil)
	_ cloud.Provider       = (*Pool)(nil)
	_ cloud.PoolMembership = (*Pool)(nil)
)

// New connects to vCD and authenticates.
func New(cfg Config) (*Client, error) {
	endpoint, err := normalizeURL(cfg.URL)
	if err != nil {
		return nil, err
	}
	u, err := url.Parse(endpoint)
	if err != nil {
		return nil, err
	}
	client := govcd.NewVCDClient(*u, cfg.Insecure)
	// Bound every call, including the ones the SDK issues on its own behalf.
	client.Client.Http.Timeout = httpTimeout(cfg)
	if err := client.Authenticate(cfg.User, cfg.Password, cfg.Org); err != nil {
		return nil, err
	}
	return &Client{cfg: cfg, client: client}, nil
}

// For returns the provider for one pool. The placement carries the vApp, org
// network and catalog every call works in.
func (c *Client) For(placement cloud.Placement) (cloud.Provider, error) {
	if placement.Provider != "" && placement.Provider != cloud.ProviderVCD {
		return nil, fmt.Errorf("vcd: placement names provider %q", placement.Provider)
	}
	if placement.VCD == nil {
		return nil, errors.New("vcd: the placement carries no spec.vcd")
	}
	if strings.TrimSpace(placement.VCD.VApp) == "" {
		return nil, errors.New("vcd: spec.vcd.vapp is empty")
	}
	return &Pool{Client: c, placement: *placement.VCD}, nil
}

// normalizeURL returns the vCD API endpoint expected by govcd: a single
// http(s) scheme, a non-empty host, and a path ending in /api.
//
// It tolerates common misconfigurations such as "vcd.example.com",
// "https://vcd.example.com" and the doubly-prefixed
// "https://https://vcd.example.com/api/api".
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

// Capacity reads what the configured vDC still has free: compute capacity and
// the storage profiles a worker can be placed on. The numbers are the ones vCD
// itself rates a new VM against.
//
// The compute capacity is reported by vCD in MHz (CPU) and MB (memory). A vDC
// whose allocation model does not cap a category reports Limit 0, which the
// caller reads as "unbounded".
func (p *Pool) Capacity(ctx context.Context) (cloud.PlatformCapacity, error) {
	if err := ctx.Err(); err != nil {
		return cloud.PlatformCapacity{}, err
	}
	vdc, err := p.vdc()
	if err != nil {
		return cloud.PlatformCapacity{}, err
	}

	out := cloud.PlatformCapacity{
		Model:         vdc.Vdc.AllocationModel,
		CPUMHzPerCore: p.cpuMHzPerCore(ctx),
	}
	if len(vdc.Vdc.ComputeCapacity) > 0 && vdc.Vdc.ComputeCapacity[0] != nil {
		cc := vdc.Vdc.ComputeCapacity[0]
		if cc.CPU != nil {
			out.CPU = cloud.ResourceCapacity{Limit: cc.CPU.Limit, Used: cc.CPU.Used}
		}
		if cc.Memory != nil {
			out.MemoryMB = cloud.ResourceCapacity{Limit: cc.Memory.Limit, Used: cc.Memory.Used}
		}
	}

	if profiles := vdc.Vdc.VdcStorageProfiles; profiles != nil {
		for _, ref := range profiles.VdcStorageProfile {
			if ref == nil || ref.HREF == "" {
				continue
			}
			sp, err := p.client.GetStorageProfileByHref(ref.HREF)
			if err != nil {
				// One unreadable profile must not cost the CPU and memory
				// check: the profile is left out and the caller skips it.
				log.FromContext(ctx).V(1).Info("storage profile capacity is not readable",
					"profile", ref.Name, "error", err)
				continue
			}
			out.Storage = append(out.Storage, cloud.StorageProfileCapacity{
				Name:    sp.Name,
				Default: sp.Default,
				LimitMB: storageLimitMB(sp),
				UsedMB:  sp.StorageUsedMB,
			})
		}
	}
	sort.Slice(out.Storage, func(i, j int) bool { return out.Storage[i].Name < out.Storage[j].Name })
	return out, nil
}

// storageLimitMB converts a storage profile limit to MB. vCD reports the limit
// in the profile's units (MB in practice) and the usage always in MB, so a GB
// limit has to be converted before the two can be compared.
func storageLimitMB(sp *types.VdcStorageProfile) int64 {
	if strings.EqualFold(strings.TrimSpace(sp.Units), "GB") {
		return sp.Limit * 1024
	}
	return sp.Limit
}

// cpuMHzPerCore is the speed the vDC rates one vCPU at, in MHz. It is read once
// from the admin view of the VDC, because the tenant view does not carry it.
//
// A tenant account that is not an org administrator cannot read the admin view.
// That is not an error: zero tells the caller to fall back to the assumption in
// the pool spec, and the capacity check still guards memory and storage.
func (p *Pool) cpuMHzPerCore(ctx context.Context) int64 {
	p.cpuOnce.Do(func() {
		adminOrg, err := p.client.GetAdminOrgByName(p.cfg.Org)
		if err != nil {
			log.FromContext(ctx).V(1).Info("vDC CPU speed is not readable; the capacity pre-flight will use the pool assumption",
				"org", p.cfg.Org, "error", err)
			return
		}
		adminVDC, err := adminOrg.GetAdminVDCByName(p.cfg.VDC, true)
		if err != nil {
			log.FromContext(ctx).V(1).Info("vDC CPU speed is not readable; the capacity pre-flight will use the pool assumption",
				"vdc", p.cfg.VDC, "error", err)
			return
		}
		switch {
		case adminVDC.AdminVdc.VCpuInMhz2 != nil:
			p.cpuMHz = *adminVDC.AdminVdc.VCpuInMhz2
		case adminVDC.AdminVdc.VCpuInMhz != nil:
			p.cpuMHz = *adminVDC.AdminVdc.VCpuInMhz
		}
	})
	return p.cpuMHz
}

// ListWorkerVMs returns the VMs in vapp whose metadata contains every
// key/value pair in selector. A VM without metadata never matches a non-empty
// selector.
func (p *Pool) ListWorkerVMs(ctx context.Context, selector map[string]string) ([]cloud.VM, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}

	vdc, err := p.vdc()
	if err != nil {
		return nil, err
	}
	vapp, err := vdc.GetVAppByName(p.placement.VApp, true)
	if err != nil {
		return nil, fmt.Errorf("get vApp %q: %w", p.placement.VApp, err)
	}
	if vapp.VApp.Children == nil {
		return []cloud.VM{}, nil
	}

	result := make([]cloud.VM, 0, len(vapp.VApp.Children.VM))
	for _, child := range vapp.VApp.Children.VM {
		if child == nil || child.Name == "" {
			continue
		}
		vm, err := vapp.GetVMByName(child.Name, false)
		if err != nil {
			if govcd.ContainsNotFound(err) {
				continue
			}
			return nil, fmt.Errorf("get VM %q: %w", child.Name, err)
		}
		info, err := p.vmInfo(vm, selector)
		if err != nil {
			return nil, err
		}
		if info != nil {
			result = append(result, *info)
		}
	}
	return result, nil
}

// GetVM reads one worker VM by id.
func (p *Pool) GetVM(ctx context.Context, id string) (cloud.VM, error) {
	if err := ctx.Err(); err != nil {
		return cloud.VM{}, err
	}
	if strings.TrimSpace(id) == "" {
		return cloud.VM{}, fmt.Errorf("%w: empty id", cloud.ErrVMNotFound)
	}
	vdc, err := p.vdc()
	if err != nil {
		return cloud.VM{}, err
	}
	vapp, err := vdc.GetVAppByName(p.placement.VApp, true)
	if err != nil {
		return cloud.VM{}, fmt.Errorf("get vApp %q: %w", p.placement.VApp, err)
	}
	vm, err := vapp.GetVMById(id, true)
	if err != nil {
		if govcd.ContainsNotFound(err) {
			return cloud.VM{}, fmt.Errorf("%w: %q", cloud.ErrVMNotFound, id)
		}
		return cloud.VM{}, fmt.Errorf("get VM %q: %w", id, err)
	}
	info, err := p.vmInfo(vm, nil)
	if err != nil {
		return cloud.VM{}, err
	}
	if info == nil {
		return cloud.VM{}, fmt.Errorf("%w: %q", cloud.ErrVMNotFound, id)
	}
	return *info, nil
}

// CreateVM creates a worker VM from a vApp template and configures it before
// the first power-on:
//
//  1. metadata (so a partially created VM is discoverable by ListWorkerVMs),
//  2. CPU/memory,
//  3. the system disk,
//  4. disk.EnableUUID,
//  5. internal data disks in deterministic bus/unit order,
//  6. guest customization (the initscript),
//  7. power-on.
//
// If any step fails, the VM is removed again on a best-effort basis.
//
// It also finishes a create that an earlier pass did not complete. The second half matters: a controller restart or a timeout in
// the middle of a create leaves a half-configured VM behind (I-18), and the
// ledger has already given this slot its name, so converging on the existing
// VM is the only loss-free outcome. Every step below is idempotent, and a VM
// that is already powered on had reached the last step of its create.
func (p *Pool) CreateVM(ctx context.Context, spec cloud.CreateVMSpec) (cloud.VM, error) {
	if err := ctx.Err(); err != nil {
		return cloud.VM{}, err
	}
	if err := validateCreateVMSpec(spec); err != nil {
		return cloud.VM{}, err
	}
	if strings.TrimSpace(p.placement.OrgNetwork) == "" {
		return cloud.VM{}, errors.New("vcd: the placement carries no org network")
	}

	vdc, err := p.vdc()
	if err != nil {
		return cloud.VM{}, err
	}
	vapp, err := vdc.GetVAppByName(p.placement.VApp, true)
	if err != nil {
		return cloud.VM{}, fmt.Errorf("get vApp %q: %w", p.placement.VApp, err)
	}

	vm, resumed := (*govcd.VM)(nil), false
	if existing, gerr := vapp.GetVMByName(spec.Name, true); gerr == nil && existing != nil {
		vm, resumed = existing, true
	} else if gerr != nil && !govcd.ContainsNotFound(gerr) {
		return cloud.VM{}, fmt.Errorf("check existing VM %q: %w", spec.Name, gerr)
	}

	created := false
	if !resumed {
		// The template is only needed for a fresh VM: a resumed create must not fail
		// because the template moved.
		template, err := p.findTemplate(vdc, spec)
		if err != nil {
			return cloud.VM{}, err
		}
		network := &types.NetworkConnectionSection{
			Info:                          "NetworkConnectionSection",
			PrimaryNetworkConnectionIndex: 0,
			NetworkConnection: []*types.NetworkConnection{{
				Network:                 p.placement.OrgNetwork,
				NetworkConnectionIndex:  0,
				IPAddress:               spec.IP,
				IsConnected:             true,
				NeedsCustomization:      true,
				IPAddressAllocationMode: types.IPAllocationModeManual,
				IpType:                  "IPV4",
			}},
		}
		task, err := vapp.AddNewVM(spec.Name, *template, network, true)
		if err != nil {
			return cloud.VM{}, fmt.Errorf("create VM %q: %w", spec.Name, err)
		}
		if err := task.WaitTaskCompletion(); err != nil {
			return cloud.VM{}, fmt.Errorf("wait for VM %q creation: %w", spec.Name, err)
		}
		if vm, err = vapp.GetVMByName(spec.Name, true); err != nil {
			return cloud.VM{}, fmt.Errorf("get created VM %q: %w", spec.Name, err)
		}
	}

	defer func() {
		if !created {
			_ = removeVM(vapp, vm)
		}
	}()

	if resumed {
		if err := vm.Refresh(); err != nil {
			return cloud.VM{}, fmt.Errorf("refresh resumed VM %q: %w", spec.Name, err)
		}
		if vm.VM.Status == vcdPoweredOn {
			info, err := p.vmInfo(vm, nil)
			if err != nil {
				return cloud.VM{}, err
			}
			if info == nil {
				return cloud.VM{}, fmt.Errorf("VM %q disappeared", spec.Name)
			}
			created = true
			return *info, nil
		}
	}

	if len(spec.Metadata) > 0 {
		values := make(map[string]interface{}, len(spec.Metadata))
		for k, v := range spec.Metadata {
			values[k] = v
		}
		if err := vm.MergeMetadata(types.MetadataStringValue, values); err != nil {
			return cloud.VM{}, fmt.Errorf("set metadata on VM %q: %w", spec.Name, err)
		}
	}

	if err := configureCompute(vm, spec); err != nil {
		return cloud.VM{}, err
	}

	// Before the data disks: the system disk is the only disk in the spec at
	// this point, so the resize PUT carries no half-attached disks.
	if err := setSystemDiskSize(vm, spec.OSDiskGB); err != nil {
		return cloud.VM{}, err
	}

	if spec.EnableUUID {
		if _, err := vm.UpdateExtraConfig([]*types.ExtraConfigMarshal{
			{Key: "disk.EnableUUID", Value: "TRUE"},
		}); err != nil {
			return cloud.VM{}, fmt.Errorf("set disk.EnableUUID on VM %q: %w", spec.Name, err)
		}
	}

	if err := p.attachDisks(vdc, vm, spec.Disks); err != nil {
		return cloud.VM{}, fmt.Errorf("attach data disks to VM %q: %w", spec.Name, err)
	}

	if err := setGuestCustomization(vm, spec); err != nil {
		return cloud.VM{}, fmt.Errorf("set guest customization on VM %q: %w", spec.Name, err)
	}

	if err := ctx.Err(); err != nil {
		return cloud.VM{}, err
	}
	task, err := vm.PowerOn()
	if err != nil {
		return cloud.VM{}, fmt.Errorf("power on VM %q: %w", spec.Name, err)
	}
	if err := task.WaitTaskCompletion(); err != nil {
		return cloud.VM{}, fmt.Errorf("wait for VM %q power-on: %w", spec.Name, err)
	}
	if err := vm.Refresh(); err != nil {
		return cloud.VM{}, fmt.Errorf("refresh VM %q: %w", spec.Name, err)
	}

	info, err := p.vmInfo(vm, nil)
	if err != nil {
		return cloud.VM{}, err
	}
	if info == nil {
		return cloud.VM{}, fmt.Errorf("VM %q disappeared", spec.Name)
	}
	created = true
	return *info, nil
}

// DeleteVM removes a worker VM and the disks that belong to it. It is
// idempotent: a VM that is already absent returns nil, which lets a retried
// scale-in finish after a partial deletion.
func (p *Pool) DeleteVM(ctx context.Context, id string) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	vdc, err := p.vdc()
	if err != nil {
		return err
	}
	vapp, err := vdc.GetVAppByName(p.placement.VApp, true)
	if err != nil {
		if govcd.ContainsNotFound(err) {
			return nil
		}
		return fmt.Errorf("get vApp %q: %w", p.placement.VApp, err)
	}
	vm, err := vapp.GetVMById(id, true)
	if err != nil {
		if govcd.ContainsNotFound(err) {
			return nil
		}
		return fmt.Errorf("get VM %q: %w", id, err)
	}

	// RemoveVM only accepts a powered-off VM. A hard power-off is deliberate:
	// the guest has already been drained, and a wedged guest must not be able
	// to block its own removal.
	if err := powerOff(vm); err != nil {
		return err
	}

	if err := vapp.RemoveVM(*vm); err != nil {
		if govcd.ContainsNotFound(err) {
			return nil
		}
		return fmt.Errorf("remove VM %q from vApp %q: %w", vm.VM.Name, p.placement.VApp, err)
	}
	return nil
}

// PowerOffVM hard-powers-off a VM without removing it. The removal path uses
// it to stop kubelet before the Node object is deleted: a guest that is still
// running re-registers its Node within seconds and leaves a NotReady corpse.
// It is idempotent, and a VM that is already gone counts as done.
func (p *Pool) PowerOffVM(ctx context.Context, id string) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	vdc, err := p.vdc()
	if err != nil {
		return err
	}
	vapp, err := vdc.GetVAppByName(p.placement.VApp, true)
	if err != nil {
		if govcd.ContainsNotFound(err) {
			return nil
		}
		return fmt.Errorf("get vApp %q: %w", p.placement.VApp, err)
	}
	vm, err := vapp.GetVMById(id, true)
	if err != nil {
		if govcd.ContainsNotFound(err) {
			return nil
		}
		return fmt.Errorf("get VM %q: %w", id, err)
	}
	return powerOff(vm)
}

// powerOff powers the VM off when it is on and refreshes it afterwards, so the
// caller can act on the new status.
func powerOff(vm *govcd.VM) error {
	if vm.VM.Status != 4 { // POWERED_ON
		return nil
	}
	task, err := vm.PowerOff()
	if err != nil {
		return fmt.Errorf("power off VM %q: %w", vm.VM.Name, err)
	}
	if err := task.WaitTaskCompletion(); err != nil {
		return fmt.Errorf("wait for VM %q power-off: %w", vm.VM.Name, err)
	}
	if err := vm.Refresh(); err != nil {
		return fmt.Errorf("refresh VM %q after power-off: %w", vm.VM.Name, err)
	}
	return nil
}

// PowerOnVM powers a stopped VM on. It is idempotent, and a VM that is
// already gone counts as done.
func (p *Pool) PowerOnVM(ctx context.Context, id string) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	vdc, err := p.vdc()
	if err != nil {
		return err
	}
	vapp, err := vdc.GetVAppByName(p.placement.VApp, true)
	if err != nil {
		if govcd.ContainsNotFound(err) {
			return nil
		}
		return fmt.Errorf("get vApp %q: %w", p.placement.VApp, err)
	}
	vm, err := vapp.GetVMById(id, true)
	if err != nil {
		if govcd.ContainsNotFound(err) {
			return nil
		}
		return fmt.Errorf("get VM %q: %w", id, err)
	}
	return powerOn(vm)
}

// CycleVM power-cycles a VM. AutoHeal uses it when the VM is alive but its
// Node object disappeared: a guest reboot re-registers kubelet with the
// cluster, which a metadata-only repair cannot do.
func (p *Pool) CycleVM(ctx context.Context, id string) error {
	if err := p.PowerOffVM(ctx, id); err != nil {
		return err
	}
	return p.PowerOnVM(ctx, id)
}

func powerOn(vm *govcd.VM) error {
	if vm.VM.Status == 4 { // POWERED_ON
		return nil
	}
	task, err := vm.PowerOn()
	if err != nil {
		return fmt.Errorf("power on VM %q: %w", vm.VM.Name, err)
	}
	if err := task.WaitTaskCompletion(); err != nil {
		return fmt.Errorf("wait for VM %q power-on: %w", vm.VM.Name, err)
	}
	if err := vm.Refresh(); err != nil {
		return fmt.Errorf("refresh VM %q after power-on: %w", vm.VM.Name, err)
	}
	return nil
}

// ResizeVM grows a powered-off worker to the requested shape. A shrink is
// refused: vCD cannot reduce CPU or memory and a smaller disk would destroy
// data, so shrinking is a recreate operation (drain, delete the VM, create a
// new one in the same ledger slot), not a resize.
// ResizeVM grows a powered-off VM. It reads the VM first and returns without
// touching it when the shape already matches, so a step that a status conflict
// re-drove cannot fail on a VM that a previous pass already resized (I-10).
// CPU, memory and the system disk are changed in the one power-off window the
// caller opened, and the result is verified before the caller powers it on: a
// resize that only half applied must not boot a worker with the wrong shape
// (I-17).
func (p *Pool) ResizeVM(ctx context.Context, id string, cpus, memoryMB, osDiskGB int32) (cloud.VM, error) {
	if err := ctx.Err(); err != nil {
		return cloud.VM{}, err
	}
	vdc, err := p.vdc()
	if err != nil {
		return cloud.VM{}, err
	}
	vapp, err := vdc.GetVAppByName(p.placement.VApp, true)
	if err != nil {
		return cloud.VM{}, fmt.Errorf("get vApp %q: %w", p.placement.VApp, err)
	}
	vm, err := vapp.GetVMById(id, true)
	if err != nil {
		return cloud.VM{}, fmt.Errorf("get VM %q: %w", id, err)
	}

	curCPU, curMem, _ := vmResources(vm)
	curDisk := systemDiskGB(vm)
	switch {
	case cpus > 0 && cpus < curCPU:
		return cloud.VM{}, fmt.Errorf("VM %q has %d vCPU, cannot shrink to %d", vm.VM.Name, curCPU, cpus)
	case memoryMB > 0 && memoryMB < curMem:
		return cloud.VM{}, fmt.Errorf("VM %q has %d MB memory, cannot shrink to %d MB", vm.VM.Name, curMem, memoryMB)
	case osDiskGB > 0 && curDisk > 0 && osDiskGB < curDisk:
		return cloud.VM{}, fmt.Errorf("VM %q has a %d GB system disk, cannot shrink to %d GB", vm.VM.Name, curDisk, osDiskGB)
	}

	want := func(have, want int32) bool { return want <= 0 || have == want }
	if want(curCPU, cpus) && want(curMem, memoryMB) && want(curDisk, osDiskGB) {
		info, err := p.vmInfo(vm, nil)
		if err != nil {
			return cloud.VM{}, err
		}
		if info == nil {
			return cloud.VM{}, fmt.Errorf("VM %q disappeared", id)
		}
		return *info, nil
	}

	if vm.VM.Status != vcdPoweredOff {
		return cloud.VM{}, fmt.Errorf("%w: VM %q is %s", cloud.ErrPoweredOn, vm.VM.Name, vappStatus(vm.VM.Status))
	}

	// The system disk is changed through the VM spec section, so it goes
	// first: that call carries the current CPU and memory with it, and doing
	// it after ChangeCPUCount would write the pre-resize values back.
	if osDiskGB > 0 && curDisk != osDiskGB {
		if err := setSystemDiskSize(vm, osDiskGB); err != nil {
			return cloud.VM{}, err
		}
	}
	if cpus > 0 && cpus != curCPU {
		task, err := vm.ChangeCPUCount(int(cpus))
		if err != nil {
			return cloud.VM{}, fmt.Errorf("set vCPU of VM %q to %d: %w", vm.VM.Name, cpus, err)
		}
		if err := task.WaitTaskCompletion(); err != nil {
			return cloud.VM{}, fmt.Errorf("wait for vCPU change of VM %q: %w", vm.VM.Name, err)
		}
	}
	if memoryMB > 0 && memoryMB != curMem {
		task, err := vm.ChangeMemorySize(int(memoryMB))
		if err != nil {
			return cloud.VM{}, fmt.Errorf("set memory of VM %q to %d MB: %w", vm.VM.Name, memoryMB, err)
		}
		if err := task.WaitTaskCompletion(); err != nil {
			return cloud.VM{}, fmt.Errorf("wait for memory change of VM %q: %w", vm.VM.Name, err)
		}
	}
	if err := vm.Refresh(); err != nil {
		return cloud.VM{}, fmt.Errorf("refresh VM %q after resize: %w", vm.VM.Name, err)
	}
	info, err := p.vmInfo(vm, nil)
	if err != nil {
		return cloud.VM{}, err
	}
	if info == nil {
		return cloud.VM{}, fmt.Errorf("VM %q disappeared during the resize", id)
	}
	if info.OSDiskGB == 0 {
		info.OSDiskGB = systemDiskGB(vm)
	}
	if !want(info.CPU, cpus) || !want(info.MemoryMB, memoryMB) || !want(info.OSDiskGB, osDiskGB) {
		return cloud.VM{}, fmt.Errorf("VM %q reports %d vCPU / %d MB / %d GB after the resize, want %d / %d / %d",
			vm.VM.Name, info.CPU, info.MemoryMB, info.OSDiskGB, cpus, memoryMB, osDiskGB)
	}
	return *info, nil
}

// systemDiskGB is the size of the system disk (bus 0/unit 0), which is what
// spec.flavor.osDiskGB describes. vmResources sums all disks instead, so it
// cannot be used for a flavor comparison.
func systemDiskGB(vm *govcd.VM) int32 {
	if vm == nil || vm.VM == nil || vm.VM.VmSpecSection == nil || vm.VM.VmSpecSection.DiskSection == nil {
		return 0
	}
	for _, disk := range vm.VM.VmSpecSection.DiskSection.DiskSettings {
		if disk != nil && disk.BusNumber == 0 && disk.UnitNumber == 0 {
			return int32((disk.SizeMb + 1023) / 1024)
		}
	}
	return 0
}

// AddPoolMember adds ip to an NSX-V edge load-balancer pool. It is
// idempotent: an address that is already a member is left untouched.
func (p *Pool) AddPoolMember(ctx context.Context, pool cloud.PoolRef, ip string) error {
	return p.updatePoolMember(ctx, pool, ip, true)
}

// RemovePoolMember removes ip from an NSX-V edge load-balancer pool.
func (p *Pool) RemovePoolMember(ctx context.Context, pool cloud.PoolRef, ip string) error {
	return p.updatePoolMember(ctx, pool, ip, false)
}

// updatePoolMember edits one member of an NSX-V server pool. The whole pool is
// PUT back (that is the NSX-V API shape), so the update starts from the pool as
// the edge currently reports it: members added by Terraform or by hand survive.
func (p *Pool) updatePoolMember(ctx context.Context, pool cloud.PoolRef, ip string, add bool) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	if strings.TrimSpace(pool.Name) == "" {
		return fmt.Errorf("%w: pool name is empty", cloud.ErrPoolNotConfigured)
	}
	switch pool.Type {
	case "", "nsxv":
	case "nsxt":
		// NSX-T ALB pools are a different API surface (NsxtAlbPool), tracked
		// separately; failing here keeps the caller honest instead of silently
		// leaving a drained node in a live pool.
		return fmt.Errorf("%w: NSX-T ingress pools are not implemented", cloud.ErrPoolNotConfigured)
	default:
		return fmt.Errorf("%w: unknown pool type %q", cloud.ErrPoolNotConfigured, pool.Type)
	}
	if strings.TrimSpace(pool.Edge) == "" {
		return fmt.Errorf("%w: ingressPool.edge is empty", cloud.ErrPoolNotConfigured)
	}

	edge, err := p.edgeGateway(pool.Edge)
	if err != nil {
		return err
	}
	lbPool, err := edge.GetLbServerPoolByName(pool.Name)
	if err != nil {
		if govcd.ContainsNotFound(err) {
			return fmt.Errorf("%w: pool %q not found on edge %q", cloud.ErrPoolNotConfigured, pool.Name, pool.Edge)
		}
		return fmt.Errorf("get LB pool %q: %w", pool.Name, err)
	}

	port := pool.Port
	members := make([]types.LbPoolMember, 0, len(lbPool.Members)+1)
	found := false
	for _, m := range lbPool.Members {
		if m.IpAddress == ip {
			found = true
			continue
		}
		members = append(members, m)
	}
	if add {
		if found {
			return nil
		}
		if port <= 0 {
			return fmt.Errorf("%w: pool %q has port %d; set ingressPool.port to the node port", cloud.ErrPoolNotConfigured, pool.Name, port)
		}
		members = append(members, types.LbPoolMember{
			Name:        poolMemberName(ip),
			IpAddress:   ip,
			Port:        int(port),
			MonitorPort: int(port),
			Condition:   "enabled",
			Weight:      1,
			MaxConn:     100,
		})
	} else if !found {
		return nil
	}

	lbPool.Members = members
	if _, err := edge.UpdateLbServerPool(lbPool); err != nil {
		return fmt.Errorf("update LB pool %q: %w", pool.Name, err)
	}
	return nil
}

// poolMemberName builds the NSX-V member name for an address. NSX-V accepts
// letters, digits, dash and underscore and the name has to start with a letter,
// so a bare address is rejected (API error 14571, "invalid member name").
func poolMemberName(ip string) string {
	return "node-" + strings.NewReplacer(".", "-", ":", "-").Replace(ip)
}

// edgeGateway resolves the NSX-V edge that owns a load-balancer pool.
func (p *Pool) edgeGateway(name string) (*govcd.EdgeGateway, error) {
	vdc, err := p.vdc()
	if err != nil {
		return nil, err
	}
	edge, err := vdc.GetEdgeGatewayByName(name, true)
	if err != nil {
		if govcd.ContainsNotFound(err) {
			return nil, fmt.Errorf("%w: edge gateway %q not found", cloud.ErrPoolNotConfigured, name)
		}
		return nil, fmt.Errorf("get edge gateway %q: %w", name, err)
	}
	return edge, nil
}

// setSystemDiskSize grows the system disk (bus 0/unit 0) of a freshly cloned
// VM to sizeGB before the first power-on. Terraform's override_template_disk
// does the same thing for the static nodes; a clone without it keeps the
// catalog template size.
func setSystemDiskSize(vm *govcd.VM, sizeGB int32) error {
	if sizeGB <= 0 {
		return nil
	}
	if vm.VM.VmSpecSection == nil {
		if err := vm.Refresh(); err != nil {
			return fmt.Errorf("refresh VM %q: %w", vm.VM.Name, err)
		}
	}
	spec := vm.VM.VmSpecSection
	if spec == nil || spec.DiskSection == nil {
		return fmt.Errorf("VM %q has no disk section", vm.VM.Name)
	}

	wantMB := int64(sizeGB) * 1024
	var system *types.DiskSettings
	for _, disk := range spec.DiskSection.DiskSettings {
		if disk != nil && disk.BusNumber == 0 && disk.UnitNumber == 0 {
			system = disk
			break
		}
	}
	if system == nil {
		return fmt.Errorf("VM %q has no system disk at bus 0/unit 0", vm.VM.Name)
	}
	switch {
	case system.SizeMb == wantMB:
		return nil
	case system.SizeMb > wantMB:
		// vCD cannot shrink a disk, and a template larger than the requested
		// size is a configuration mismatch, not something to paper over.
		return fmt.Errorf("VM %q system disk is %d MB, cannot shrink to %d MB", vm.VM.Name, system.SizeMb, wantMB)
	}

	system.SizeMb = wantMB
	if _, err := vm.UpdateVmSpecSection(spec, "resize worker system disk"); err != nil {
		return fmt.Errorf("resize system disk of VM %q to %d GB: %w", vm.VM.Name, sizeGB, err)
	}
	if err := vm.Refresh(); err != nil {
		return fmt.Errorf("refresh VM %q after disk resize: %w", vm.VM.Name, err)
	}
	return nil
}

// vdc resolves the configured org and VDC. The controller is scoped to one
// org/VDC (the same one Terraform used for the initial cluster).
func (p *Pool) vdc() (*govcd.Vdc, error) {
	org, err := p.client.GetOrgByName(p.cfg.Org)
	if err != nil {
		return nil, fmt.Errorf("get org %q: %w", p.cfg.Org, err)
	}
	vdc, err := org.GetVDCByName(p.cfg.VDC, true)
	if err != nil {
		return nil, fmt.Errorf("get VDC %q: %w", p.cfg.VDC, err)
	}
	return vdc, nil
}

func (p *Pool) findTemplate(vdc *govcd.Vdc, spec cloud.CreateVMSpec) (*govcd.VAppTemplate, error) {
	if strings.TrimSpace(p.placement.Catalog) == "" {
		tpl, err := vdc.GetVAppTemplateByName(spec.Template)
		if err != nil {
			return nil, fmt.Errorf("find template %q in VDC %q: %w", spec.Template, p.cfg.VDC, err)
		}
		return tpl, nil
	}

	// A catalog shared into this org is visible here, and an org admin often
	// cannot read the publishing org at all, so try the controller org first and
	// only then the org named in spec.vcd.catalogOrg.
	org, err := p.client.GetOrgByName(p.cfg.Org)
	if err != nil {
		return nil, fmt.Errorf("get org %q: %w", p.cfg.Org, err)
	}
	catalog, err := org.GetCatalogByName(p.placement.Catalog, true)
	if err != nil {
		orgName := strings.TrimSpace(p.placement.CatalogOrg)
		if orgName == "" || strings.EqualFold(orgName, p.cfg.Org) {
			return nil, fmt.Errorf("get catalog %q in org %q: %w", p.placement.Catalog, p.cfg.Org, err)
		}
		other, oerr := p.client.GetOrgByName(orgName)
		if oerr != nil {
			return nil, fmt.Errorf("get catalog org %q: %w", orgName, oerr)
		}
		catalog, err = other.GetCatalogByName(p.placement.Catalog, true)
		if err != nil {
			return nil, fmt.Errorf("get catalog %q in org %q: %w", p.placement.Catalog, orgName, err)
		}
	}
	tpl, err := catalog.GetVAppTemplateByName(spec.Template)
	if err != nil {
		return nil, fmt.Errorf("find template %q in catalog %q: %w", spec.Template, p.placement.Catalog, err)
	}
	return tpl, nil
}

func (p *Pool) vmInfo(vm *govcd.VM, selector map[string]string) (*cloud.VM, error) {
	if vm == nil || vm.VM == nil {
		return nil, nil
	}
	metadata, err := getMetadataMap(vm)
	if err != nil {
		return nil, err
	}
	for k, v := range selector {
		if metadata[k] != v {
			return nil, nil
		}
	}

	cpu, memMB, diskGB := vmResources(vm)
	return &cloud.VM{
		ID:         vm.VM.ID,
		Name:       vm.VM.Name,
		IP:         primaryIP(vm),
		PowerState: vappStatus(vm.VM.Status),
		PoweredOn:  vm.VM.Status == vcdPoweredOn,
		CPU:        cpu,
		MemoryMB:   memMB,
		DiskGB:     diskGB,
		OSDiskGB:   systemDiskGB(vm),
		Metadata:   metadata,
	}, nil
}

func getMetadataMap(vm *govcd.VM) (map[string]string, error) {
	out := make(map[string]string)
	metadata, err := vm.GetMetadata()
	if err != nil {
		if govcd.ContainsNotFound(err) {
			return out, nil
		}
		return nil, fmt.Errorf("get metadata for VM %q: %w", vm.VM.Name, err)
	}
	if metadata == nil {
		return out, nil
	}
	for _, entry := range metadata.MetadataEntry {
		if entry == nil || entry.TypedValue == nil {
			continue
		}
		out[entry.Key] = entry.TypedValue.Value
	}
	return out, nil
}

func primaryIP(vm *govcd.VM) string {
	section, err := vm.GetNetworkConnectionSection()
	if err != nil || section == nil {
		return ""
	}
	for _, nic := range section.NetworkConnection {
		if nic != nil && nic.NetworkConnectionIndex == section.PrimaryNetworkConnectionIndex && nic.IPAddress != "" {
			return nic.IPAddress
		}
	}
	for _, nic := range section.NetworkConnection {
		if nic != nil && nic.IPAddress != "" {
			return nic.IPAddress
		}
	}
	return ""
}

func vmResources(vm *govcd.VM) (cpu, memMB, diskGB int32) {
	if vm.VM.VmSpecSection == nil {
		return 0, 0, 0
	}
	spec := vm.VM.VmSpecSection
	if spec.NumCpus != nil {
		cpu = int32(*spec.NumCpus)
	}
	if spec.MemoryResourceMb != nil {
		memMB = int32(spec.MemoryResourceMb.Configured)
	}
	if spec.DiskSection != nil {
		var totalMB int64
		for _, disk := range spec.DiskSection.DiskSettings {
			if disk != nil {
				totalMB += disk.SizeMb
			}
		}
		diskGB = int32((totalMB + 1023) / 1024)
	}
	return cpu, memMB, diskGB
}

// vCD VM power states. types.VAppStatuses maps them for display; the two used
// here are the ones a resize has to tell apart.
const (
	vcdPoweredOn  = 4
	vcdPoweredOff = 8
)

func vappStatus(status int) string {
	if s, ok := types.VAppStatuses[status]; ok {
		return s
	}
	return fmt.Sprintf("STATUS_%d", status)
}

func configureCompute(vm *govcd.VM, spec cloud.CreateVMSpec) error {
	if vm.VM.VmSpecSection == nil {
		if err := vm.Refresh(); err != nil {
			return fmt.Errorf("refresh VM %q: %w", spec.Name, err)
		}
	}
	if vm.VM.VmSpecSection == nil {
		return fmt.Errorf("VM %q has no VmSpecSection", spec.Name)
	}
	// ChangeCPU/ChangeMemory both use VmSpecSection. Make sure the required
	// MemoryResourceMb element exists even if the template returned no value.
	if vm.VM.VmSpecSection.MemoryResourceMb == nil {
		vm.VM.VmSpecSection.MemoryResourceMb = &types.MemoryResourceMb{}
	}
	if err := vm.ChangeCPU(int(spec.CPUs), 1); err != nil {
		return fmt.Errorf("set CPU on VM %q: %w", spec.Name, err)
	}
	if err := vm.ChangeMemory(int64(spec.MemoryMB)); err != nil {
		return fmt.Errorf("set memory on VM %q: %w", spec.Name, err)
	}
	return nil
}

func (p *Pool) attachDisks(vdc *govcd.Vdc, vm *govcd.VM, specs []cloud.DiskSpec) error {
	if len(specs) == 0 {
		return nil
	}
	if vm.VM.VmSpecSection == nil {
		if err := vm.Refresh(); err != nil {
			return fmt.Errorf("refresh VM before attaching disks: %w", err)
		}
	}
	if vm.VM.VmSpecSection == nil {
		return fmt.Errorf("VM %q has no VmSpecSection", vm.VM.Name)
	}
	if vm.VM.VmSpecSection.DiskSection == nil {
		return fmt.Errorf("VM %q has no DiskSection; cannot attach data disks", vm.VM.Name)
	}

	occupied := []cloud.DiskPlacement{{Index: -1, BusNumber: 0, UnitNumber: 0}}
	// Data disks that are already there are matched by size, so a resumed
	// create does not attach a second copy of a disk it attached before the
	// interruption. Disks of the same size are interchangeable for a worker.
	present := make(map[int32]int)
	for i, disk := range vm.VM.VmSpecSection.DiskSection.DiskSettings {
		if disk == nil {
			continue
		}
		if disk.BusNumber == 0 && disk.UnitNumber == 0 {
			continue
		}
		present[int32((disk.SizeMb+1023)/1024)]++
		occupied = append(occupied, cloud.DiskPlacement{
			Index:      i,
			BusNumber:  disk.BusNumber,
			UnitNumber: disk.UnitNumber,
		})
	}
	var missing []cloud.DiskSpec
	for _, spec := range specs {
		if present[spec.SizeGB] > 0 {
			present[spec.SizeGB]--
			continue
		}
		missing = append(missing, spec)
	}
	if len(missing) == 0 {
		return nil
	}

	placements, err := cloud.PlanDiskPlacements(missing, occupied)
	if err != nil {
		return err
	}

	for _, placement := range placements {
		storageProfile, override, err := diskStorageProfile(vdc, vm, placement.Spec.StorageProfile)
		if err != nil {
			return fmt.Errorf("disk %d storage profile: %w", placement.Index, err)
		}
		adapter, err := adapterType(placement.Spec.BusType)
		if err != nil {
			return fmt.Errorf("disk %d: %w", placement.Index, err)
		}
		thin := true
		settings := &types.DiskSettings{
			SizeMb:              int64(placement.Spec.SizeGB) * 1024,
			BusNumber:           placement.BusNumber,
			UnitNumber:          placement.UnitNumber,
			AdapterType:         adapter,
			ThinProvisioned:     &thin,
			StorageProfile:      storageProfile,
			OverrideVmDefault:   override,
			VirtualQuantityUnit: "byte",
		}
		if _, err := vm.AddInternalDisk(settings); err != nil {
			return fmt.Errorf("add disk %d at bus %d/unit %d: %w", placement.Index, placement.BusNumber, placement.UnitNumber, err)
		}
	}
	return nil
}

func diskStorageProfile(vdc *govcd.Vdc, vm *govcd.VM, name string) (*types.Reference, bool, error) {
	if strings.TrimSpace(name) == "" {
		if vm.VM.StorageProfile == nil {
			return nil, false, errors.New("VM has no default storage profile")
		}
		ref := *vm.VM.StorageProfile
		return &ref, false, nil
	}
	ref, err := vdc.FindStorageProfileReference(name)
	if err != nil {
		return nil, false, err
	}
	return &ref, true, nil
}

// adapterType maps the CRD-friendly bus type to the numeric AdapterType used
// by the vCD API. The values match terraform-provider-vcd's
// vcd_vm_internal_disk resource.
func adapterType(busType string) (string, error) {
	switch strings.ToLower(strings.TrimSpace(busType)) {
	case "", "paravirtual":
		return "5", nil
	case "ide":
		return "1", nil
	case "parallel", "lsilogic":
		return "3", nil
	case "sas", "lsilogicsas":
		return "4", nil
	case "sata":
		return "6", nil
	case "nvme":
		return "7", nil
	default:
		return "", fmt.Errorf("unsupported disk bus type %q (supported: ide, parallel, sas, paravirtual, sata, nvme)", busType)
	}
}

func setGuestCustomization(vm *govcd.VM, spec cloud.CreateVMSpec) error {
	gcs, err := vm.GetGuestCustomizationSection()
	if err != nil {
		if !govcd.ContainsNotFound(err) {
			return err
		}
		gcs = &types.GuestCustomizationSection{Info: "Guest customization"}
	}
	if gcs == nil {
		gcs = &types.GuestCustomizationSection{Info: "Guest customization"}
	}

	enabled := true
	changeSid := false
	joinDomain := false
	useOrgSettings := false
	adminPasswordEnabled := false
	resetPasswordRequired := false

	gcs.Enabled = &enabled
	gcs.ChangeSid = &changeSid
	gcs.JoinDomainEnabled = &joinDomain
	gcs.UseOrgSettings = &useOrgSettings
	gcs.AdminPasswordEnabled = &adminPasswordEnabled
	gcs.ResetPasswordRequired = &resetPasswordRequired
	gcs.ComputerName = spec.Name
	if spec.Initscript != "" {
		gcs.CustomizationScript = spec.Initscript
	}

	_, err = vm.SetGuestCustomizationSection(gcs)
	return err
}

func removeVM(vapp *govcd.VApp, vm *govcd.VM) error {
	if vm == nil || vm.VM == nil || vm.VM.HREF == "" {
		return nil
	}
	if vm.VM.Status == 4 { // POWERED_ON
		if task, err := vm.PowerOff(); err == nil {
			_ = task.WaitTaskCompletion()
		}
	}
	return vapp.RemoveVM(*vm)
}

func validateCreateVMSpec(spec cloud.CreateVMSpec) error {
	switch {
	case strings.TrimSpace(spec.Name) == "":
		return errors.New("VM name is empty")
	case strings.TrimSpace(spec.IP) == "":
		return errors.New("VM IP is empty")
	case strings.TrimSpace(spec.Template) == "":
		return errors.New("template is empty")
	case spec.CPUs <= 0:
		return errors.New("CPUs must be greater than zero")
	case spec.MemoryMB <= 0:
		return errors.New("memoryMB must be greater than zero")
	}
	for i, disk := range spec.Disks {
		if disk.SizeGB <= 0 {
			return fmt.Errorf("disk %d: sizeGB must be greater than zero", i)
		}
		if _, err := adapterType(disk.BusType); err != nil {
			return fmt.Errorf("disk %d: %w", i, err)
		}
	}
	return nil
}
