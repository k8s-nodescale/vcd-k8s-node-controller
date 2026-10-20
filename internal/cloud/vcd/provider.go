// Package vcd implements cloud.Provider on top of the official go-vcloud-director SDK.
package vcd

import (
	"context"
	"errors"
	"fmt"
	"net/url"
	"strings"

	"github.com/vmware/go-vcloud-director/v2/govcd"
	"github.com/vmware/go-vcloud-director/v2/types/v56"

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

// ListWorkerVMs returns the VMs in vapp whose metadata contains every
// key/value pair in selector. A VM without metadata never matches a non-empty
// selector.
func (p *Provider) ListWorkerVMs(ctx context.Context, vappName string, selector map[string]string) ([]cloud.VM, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}

	vdc, err := p.vdc()
	if err != nil {
		return nil, err
	}
	vapp, err := vdc.GetVAppByName(vappName, true)
	if err != nil {
		return nil, fmt.Errorf("get vApp %q: %w", vappName, err)
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

// CreateVM creates a worker VM from a vApp template and configures it before
// the first power-on:
//
//  1. metadata (so a partially created VM is discoverable by ListWorkerVMs),
//  2. CPU/memory,
//  3. disk.EnableUUID,
//  4. internal data disks in deterministic bus/unit order,
//  5. guest customization (the initscript),
//  6. power-on.
//
// If any step fails, the VM is removed again on a best-effort basis.
func (p *Provider) CreateVM(ctx context.Context, spec cloud.CreateVMSpec) (cloud.VM, error) {
	if err := ctx.Err(); err != nil {
		return cloud.VM{}, err
	}
	if err := validateCreateVMSpec(spec); err != nil {
		return cloud.VM{}, err
	}

	vdc, err := p.vdc()
	if err != nil {
		return cloud.VM{}, err
	}
	vapp, err := vdc.GetVAppByName(spec.VApp, true)
	if err != nil {
		return cloud.VM{}, fmt.Errorf("get vApp %q: %w", spec.VApp, err)
	}
	template, err := p.findTemplate(vdc, spec)
	if err != nil {
		return cloud.VM{}, err
	}

	if existing, err := vapp.GetVMByName(spec.Name, true); err == nil && existing != nil {
		return cloud.VM{}, fmt.Errorf("VM %q already exists in vApp %q", spec.Name, spec.VApp)
	} else if err != nil && !govcd.ContainsNotFound(err) {
		return cloud.VM{}, fmt.Errorf("check existing VM %q: %w", spec.Name, err)
	}

	network := &types.NetworkConnectionSection{
		Info:                          "NetworkConnectionSection",
		PrimaryNetworkConnectionIndex: 0,
		NetworkConnection: []*types.NetworkConnection{{
			Network:                 spec.OrgNetwork,
			NetworkConnectionIndex:  0,
			IPAddress:               spec.IP,
			IsConnected:             true,
			NeedsCustomization:      true,
			IPAddressAllocationMode: types.IPAllocationModeManual,
			IpType:                  "IPv4",
		}},
	}

	task, err := vapp.AddNewVM(spec.Name, *template, network, true)
	if err != nil {
		return cloud.VM{}, fmt.Errorf("create VM %q: %w", spec.Name, err)
	}
	if err := task.WaitTaskCompletion(); err != nil {
		return cloud.VM{}, fmt.Errorf("wait for VM %q creation: %w", spec.Name, err)
	}

	vm, err := vapp.GetVMByName(spec.Name, true)
	if err != nil {
		return cloud.VM{}, fmt.Errorf("get created VM %q: %w", spec.Name, err)
	}

	created := false
	defer func() {
		if !created {
			_ = removeVM(vapp, vm)
		}
	}()

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
	task, err = vm.PowerOn()
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
	created = true
	return *info, nil
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

// vdc resolves the configured org and VDC. The controller is scoped to one
// org/VDC (the same one Terraform used for the initial cluster).
func (p *Provider) vdc() (*govcd.Vdc, error) {
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

func (p *Provider) findTemplate(vdc *govcd.Vdc, spec cloud.CreateVMSpec) (*govcd.VAppTemplate, error) {
	if strings.TrimSpace(spec.Catalog) == "" {
		tpl, err := vdc.GetVAppTemplateByName(spec.Template)
		if err != nil {
			return nil, fmt.Errorf("find template %q in VDC %q: %w", spec.Template, p.cfg.VDC, err)
		}
		return tpl, nil
	}

	orgName := strings.TrimSpace(spec.CatalogOrg)
	if orgName == "" {
		orgName = p.cfg.Org
	}
	org, err := p.client.GetOrgByName(orgName)
	if err != nil {
		return nil, fmt.Errorf("get catalog org %q: %w", orgName, err)
	}
	catalog, err := org.GetCatalogByName(spec.Catalog, true)
	if err != nil {
		return nil, fmt.Errorf("get catalog %q in org %q: %w", spec.Catalog, orgName, err)
	}
	tpl, err := catalog.GetVAppTemplateByName(spec.Template)
	if err != nil {
		return nil, fmt.Errorf("find template %q in catalog %q: %w", spec.Template, spec.Catalog, err)
	}
	return tpl, nil
}

func (p *Provider) vmInfo(vm *govcd.VM, selector map[string]string) (*cloud.VM, error) {
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
		CPU:        cpu,
		MemoryMB:   memMB,
		DiskGB:     diskGB,
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

func (p *Provider) attachDisks(vdc *govcd.Vdc, vm *govcd.VM, specs []cloud.DiskSpec) error {
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
	if vm.VM.VmSpecSection != nil && vm.VM.VmSpecSection.DiskSection != nil {
		for i, disk := range vm.VM.VmSpecSection.DiskSection.DiskSettings {
			if disk == nil {
				continue
			}
			occupied = append(occupied, cloud.DiskPlacement{
				Index:      i,
				BusNumber:  disk.BusNumber,
				UnitNumber: disk.UnitNumber,
			})
		}
	}

	placements, err := cloud.PlanDiskPlacements(specs, occupied)
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
	case strings.TrimSpace(spec.VApp) == "":
		return errors.New("vApp name is empty")
	case strings.TrimSpace(spec.OrgNetwork) == "":
		return errors.New("org network is empty")
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
