// Package vsphere implements cloud.Provider on top of the vSphere API.
//
// The unit of placement is one compute cluster: spec.vsphere.cluster names it,
// its hosts are the compute a pool may use, and spec.vsphere.datastore is the
// storage. The create and the capacity pre-flight read the same two objects,
// so "the platform has room" means "this cluster and this datastore have room",
// never "some host of the datacenter has room".
//
// A worker is cloned from spec.flavor.template into spec.vsphere.folder and
// customised the way the Terraform module does it: the cloud-init user-data and
// a network-config metadata document go into the guestinfo properties, which
// cloud-init's VMware datasource reads on the first boot. The controller
// account has no guest-customization privilege (VirtualMachine.Provisioning.
// Customize), so no agent runs inside the guest: the user-data itself writes
// netplan, grows the disks and joins the cluster.
//
// The VM annotation is the record the controller keeps about a worker (the vCD
// provider uses VM metadata for the same purpose): the identity it was created
// with, the image it was cloned from and the address it was given, because a
// guest that never boots never reports an address either. A VM without that
// annotation carries no opinion, which is how a worker Terraform created is
// adopted by name without being rewritten.
package vsphere

import (
	"context"
	"errors"
	"fmt"
	"net/url"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/vmware/govmomi"
	"github.com/vmware/govmomi/fault"
	"github.com/vmware/govmomi/find"
	"github.com/vmware/govmomi/object"
	"github.com/vmware/govmomi/vim25/mo"
	"github.com/vmware/govmomi/vim25/types"
	"sigs.k8s.io/controller-runtime/pkg/log"

	"github.com/k8s-nodescale/vcd-k8s-node-controller/internal/cloud"
)

// Config holds the vCenter connection settings. It is the process-wide half of
// a placement: everything that differs per pool comes from spec.vsphere.
//
// Placement is deliberately absent: the datacenter, cluster, resource pool,
// folder, datastore and network are properties of one pool and live in its CR,
// so there is exactly one place to read them and no silent fallback to a
// process-wide value that might name a different datacenter.
type Config struct {
	URL      string
	User     string
	Password string

	// FolderRoot is the boundary a pool folder must stay inside. Empty means
	// the account is trusted with the whole datacenter.
	FolderRoot string

	Insecure bool

	// Timeout bounds one platform call, including the wait for a clone task.
	// Zero selects defaultTimeout.
	Timeout time.Duration
}

// defaultTimeout is generous because a clone of a 40 GB template on NFS is a
// minutes-long task, not a request.
const defaultTimeout = 30 * time.Minute

// Client is the cloud.Factory for vSphere. It connects on first use, so a
// controller that serves only vCD pools does not need a vCenter at all.
type Client struct {
	cfg Config

	mu     sync.Mutex
	client *govmomi.Client
}

// Provider is one pool's vSphere client, bound to its placement.
type Provider struct {
	client    *Client
	placement cloud.VSpherePlacement
}

var (
	_ cloud.Factory  = (*Client)(nil)
	_ cloud.Provider = (*Provider)(nil)
)

// New returns the vSphere factory. An empty URL is not an error here: the
// first pool that asks for vsphere gets the error that says what is missing.
func New(cfg Config) *Client { return &Client{cfg: cfg} }

// For returns the provider for one pool.
func (c *Client) For(placement cloud.Placement) (cloud.Provider, error) {
	if placement.Provider != cloud.ProviderVSphere {
		return nil, fmt.Errorf("vsphere: placement names provider %q", placement.Provider)
	}
	if placement.VSphere == nil {
		return nil, errors.New("vsphere: the placement carries no spec.vsphere")
	}
	pl := *placement.VSphere
	if strings.TrimSpace(pl.Datacenter) == "" {
		return nil, errors.New("vsphere: spec.vsphere.datacenter is required: the datacenter is part of the pool's placement, not of the controller configuration")
	}
	if strings.TrimSpace(pl.NICName) == "" {
		pl.NICName = "ens192"
	}
	return &Provider{client: c, placement: pl}, nil
}

// session returns the vCenter client, logging in again when the platform
// closed the session (idle timeout, vCenter restart). One long reconcile must
// not fail every call until the pod is restarted.
func (c *Client) session(ctx context.Context) (*govmomi.Client, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.client != nil {
		if active, err := c.client.SessionManager.SessionIsActive(ctx); err == nil && active {
			return c.client, nil
		}
		c.client.Client.CloseIdleConnections()
		c.client = nil
	}

	u, err := normalizeURL(c.cfg.URL)
	if err != nil {
		return nil, err
	}
	u.User = url.UserPassword(c.cfg.User, c.cfg.Password)
	cl, err := govmomi.NewClient(ctx, u, c.cfg.Insecure)
	if err != nil {
		return nil, fmt.Errorf("vsphere: log in to %s as %s: %w", u.Host, c.cfg.User, err)
	}
	c.client = cl
	return cl, nil
}

// opCtx bounds one platform call.
func (c *Client) opCtx(ctx context.Context) (context.Context, context.CancelFunc) {
	t := c.cfg.Timeout
	if t <= 0 {
		t = defaultTimeout
	}
	return context.WithTimeout(ctx, t)
}

// normalizeURL accepts a vCenter address the way an operator writes it
// (host, https URL, or the full SDK endpoint) and returns the SDK URL govmomi
// expects.
func normalizeURL(raw string) (*url.URL, error) {
	s := strings.TrimSpace(raw)
	if s == "" {
		return nil, errors.New("vsphere: VSPHERE_URL is empty")
	}
	if !strings.Contains(s, "://") {
		s = "https://" + s
	}
	u, err := url.Parse(s)
	if err != nil {
		return nil, fmt.Errorf("vsphere: invalid VSPHERE_URL %q: %w", raw, err)
	}
	if u.Scheme != "https" && u.Scheme != "http" {
		return nil, fmt.Errorf("vsphere: invalid VSPHERE_URL %q: unsupported scheme %q", raw, u.Scheme)
	}
	if u.Host == "" {
		return nil, fmt.Errorf("vsphere: invalid VSPHERE_URL %q: missing host", raw)
	}
	path := strings.TrimRight(u.Path, "/")
	if !strings.HasSuffix(path, "/sdk") {
		path += "/sdk"
	}
	u.Path = path
	return u, nil
}

// base returns the client and a finder scoped to the placement datacenter.
func (p *Provider) base(ctx context.Context) (*govmomi.Client, *find.Finder, error) {
	cl, err := p.client.session(ctx)
	if err != nil {
		return nil, nil, err
	}
	dc := strings.TrimSpace(p.placement.Datacenter)
	if dc == "" {
		return nil, nil, errors.New("vsphere: no datacenter: spec.vsphere.datacenter is required")
	}
	f := find.NewFinder(cl.Client, true)
	dco, err := f.Datacenter(ctx, dc)
	if err != nil {
		return nil, nil, fmt.Errorf("vsphere: datacenter %q: %w", dc, err)
	}
	f.SetDatacenter(dco)
	return cl, f, nil
}

// folderPath resolves spec.vsphere.folder to an inventory path below the
// datacenter VM folder, and enforces the configured root.
func (p *Provider) folderPath() (string, error) {
	raw := strings.TrimSpace(p.placement.Folder)
	if raw == "" {
		return "", errors.New("vsphere: spec.vsphere.folder is empty")
	}
	dc := strings.TrimSpace(p.placement.Datacenter)
	path := normalizeInventoryPath(dc, raw)
	if root := strings.TrimSpace(p.client.cfg.FolderRoot); root != "" {
		rootPath := normalizeInventoryPath(dc, root)
		if path != rootPath && !strings.HasPrefix(path, rootPath+"/") {
			return "", fmt.Errorf("vsphere: folder %q is outside the allowed root %q (VSPHERE_FOLDER_ROOT)", raw, root)
		}
	}
	return path, nil
}

// normalizeInventoryPath turns a folder written any of the accepted ways
// (a path below the datacenter, one that starts with vm/, or a full
// /<datacenter>/vm/... path) into /<datacenter>/vm/<path>.
func normalizeInventoryPath(dc, folder string) string {
	rel := strings.TrimSpace(folder)
	rel = strings.TrimPrefix(rel, "/")
	dcPrefix := dc + "/vm/"
	switch {
	case strings.HasPrefix(rel, dcPrefix):
		rel = strings.TrimPrefix(rel, dcPrefix)
	case strings.HasPrefix(rel, "vm/"):
		rel = strings.TrimPrefix(rel, "vm/")
	}
	path := "/" + dc + "/vm/" + strings.Trim(rel, "/")
	return strings.TrimRight(path, "/")
}

// folder returns the pool folder and its inventory path.
func (p *Provider) folder(ctx context.Context, f *find.Finder) (*object.Folder, string, error) {
	path, err := p.folderPath()
	if err != nil {
		return nil, "", err
	}
	obj, err := f.Folder(ctx, path)
	if err != nil {
		return nil, "", fmt.Errorf("vsphere: folder %q: %w", path, err)
	}
	return obj, path, nil
}

func (p *Provider) cluster(ctx context.Context, f *find.Finder) (*object.ClusterComputeResource, error) {
	name := strings.TrimSpace(p.placement.Cluster)
	if name == "" {
		return nil, errors.New("vsphere: spec.vsphere.cluster is empty")
	}
	c, err := f.ClusterComputeResource(ctx, name)
	if err != nil {
		return nil, fmt.Errorf("vsphere: compute cluster %q: %w", name, err)
	}
	return c, nil
}

// host returns the host a pinned pool was given, after checking that it is one
// of the cluster's hosts. A host of another cluster is refused instead of
// cloned onto: the cluster is the pool's capacity and the host is a
// refinement of it, not a way out of it.
//
// A name matches the host's inventory path or its own name, case-insensitively,
// so both the FQDN a tfvars carries and a full path work.
func (p *Provider) host(ctx context.Context, f *find.Finder, cluster *object.ClusterComputeResource) (*object.HostSystem, error) {
	want := strings.TrimSpace(p.placement.Host)
	if want == "" {
		return nil, nil
	}
	hosts, err := cluster.Hosts(ctx)
	if err != nil {
		return nil, fmt.Errorf("vsphere: hosts of cluster %q: %w", p.placement.Cluster, err)
	}
	for _, h := range hosts {
		if hostMatches(h, want) {
			return h, nil
		}
	}
	return nil, fmt.Errorf("vsphere: spec.vsphere.host %q is not a host of cluster %q: "+
		"the host is a refinement of the cluster, so it cannot name a machine outside it",
		want, p.placement.Cluster)
}

// hostMatches compares a configured host against one inventory entry: the full
// path, or the leaf name (the FQDN in the terraform tfvars).
func hostMatches(h *object.HostSystem, want string) bool {
	if h == nil {
		return false
	}
	want = strings.TrimSpace(want)
	return strings.EqualFold(strings.TrimSpace(h.InventoryPath), want) ||
		strings.EqualFold(hostLeaf(h.InventoryPath), want)
}

// hostLeaf drops the inventory path, leaving the host name a tfvars carries.
func hostLeaf(path string) string {
	if i := strings.LastIndex(path, "/"); i >= 0 {
		return path[i+1:]
	}
	return path
}

func (p *Provider) resourcePool(ctx context.Context, f *find.Finder, cluster *object.ClusterComputeResource) (*object.ResourcePool, error) {
	name := strings.TrimSpace(p.placement.ResourcePool)
	if name == "" {
		return cluster.ResourcePool(ctx)
	}
	rp, err := f.ResourcePool(ctx, cluster.InventoryPath+"/Resources/"+name)
	if err != nil {
		return nil, fmt.Errorf("vsphere: resource pool %q in cluster %q: %w", name, p.placement.Cluster, err)
	}
	return rp, nil
}

func (p *Provider) datastore(ctx context.Context, f *find.Finder) (*object.Datastore, error) {
	name := strings.TrimSpace(p.placement.Datastore)
	if name == "" {
		return nil, errors.New("vsphere: spec.vsphere.datastore is empty")
	}
	ds, err := f.Datastore(ctx, name)
	if err != nil {
		return nil, fmt.Errorf("vsphere: datastore %q: %w", name, err)
	}
	return ds, nil
}

func (p *Provider) network(ctx context.Context, f *find.Finder) (object.NetworkReference, error) {
	name := strings.TrimSpace(p.placement.Network)
	if name == "" {
		return nil, errors.New("vsphere: spec.vsphere.network is empty")
	}
	n, err := f.Network(ctx, name)
	if err != nil {
		return nil, fmt.Errorf("vsphere: port group %q: %w", name, err)
	}
	return n, nil
}

// vmProperties is what the controller reads about a VM: identity, shape, the
// annotation it wrote, the power state and the address the guest reports.
var vmProperties = []string{"name", "config", "runtime", "guest"}

// ListWorkerVMs returns the VMs below the pool folder. The folder is the scope
// of a pool, the way a vApp is for vCD: the controller filters by name prefix
// afterwards, so adopted Terraform workers are included and VMs of a sibling
// pool in another folder are not.
func (p *Provider) ListWorkerVMs(ctx context.Context, selector map[string]string) ([]cloud.VM, error) {
	ctx, cancel := p.client.opCtx(ctx)
	defer cancel()
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	cl, f, err := p.base(ctx)
	if err != nil {
		return nil, err
	}
	_, folderPath, err := p.folder(ctx, f)
	if err != nil {
		return nil, err
	}
	vms, err := f.VirtualMachineList(ctx, folderPath+"/...")
	if err != nil {
		if isNotFound(err) {
			return []cloud.VM{}, nil
		}
		return nil, fmt.Errorf("vsphere: list VMs under %q: %w", folderPath, err)
	}
	refs := make([]types.ManagedObjectReference, 0, len(vms))
	for _, vm := range vms {
		refs = append(refs, vm.Reference())
	}
	var mvs []mo.VirtualMachine
	if len(refs) > 0 {
		if err := cl.Retrieve(ctx, refs, vmProperties, &mvs); err != nil {
			return nil, fmt.Errorf("vsphere: read VMs under %q: %w", folderPath, err)
		}
	}
	out := make([]cloud.VM, 0, len(mvs))
	for i := range mvs {
		info := vmInfoFromMO(&mvs[i])
		if !matchesSelector(info.Metadata, selector) {
			continue
		}
		out = append(out, info)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	return out, nil
}

// GetVM reads one VM by its managed object id.
func (p *Provider) GetVM(ctx context.Context, id string) (cloud.VM, error) {
	ctx, cancel := p.client.opCtx(ctx)
	defer cancel()
	if err := ctx.Err(); err != nil {
		return cloud.VM{}, err
	}
	vm, name, err := p.vmByID(ctx, id)
	if err != nil {
		return cloud.VM{}, err
	}
	info, err := p.vmInfo(ctx, vm)
	if err != nil {
		return cloud.VM{}, err
	}
	if info.Name == "" {
		info.Name = name
	}
	return info, nil
}

func (p *Provider) vmByID(ctx context.Context, id string) (*object.VirtualMachine, string, error) {
	id = strings.TrimSpace(id)
	if id == "" {
		return nil, "", fmt.Errorf("%w: empty id", cloud.ErrVMNotFound)
	}
	cl, err := p.client.session(ctx)
	if err != nil {
		return nil, "", err
	}
	ref := types.ManagedObjectReference{Type: "VirtualMachine", Value: id}
	vm := object.NewVirtualMachine(cl.Client, ref)
	var m mo.VirtualMachine
	if err := vm.Properties(ctx, ref, []string{"name"}, &m); err != nil {
		if isManagedObjectNotFound(err) {
			return nil, "", fmt.Errorf("%w: %q", cloud.ErrVMNotFound, id)
		}
		return nil, "", fmt.Errorf("vsphere: read VM %q: %w", id, err)
	}
	return vm, m.Name, nil
}

func (p *Provider) vmInfo(ctx context.Context, vm *object.VirtualMachine) (cloud.VM, error) {
	ref := vm.Reference()
	var m mo.VirtualMachine
	if err := vm.Properties(ctx, ref, vmProperties, &m); err != nil {
		if isManagedObjectNotFound(err) {
			return cloud.VM{}, fmt.Errorf("%w: %q", cloud.ErrVMNotFound, ref.Value)
		}
		return cloud.VM{}, fmt.Errorf("vsphere: read VM %q: %w", ref.Value, err)
	}
	return vmInfoFromMO(&m), nil
}

// vmInfoFromMO maps the API object onto the controller view.
func vmInfoFromMO(m *mo.VirtualMachine) cloud.VM {
	out := cloud.VM{ID: m.Self.Value, Name: m.Name}
	if m.Config != nil {
		out.CPU = m.Config.Hardware.NumCPU
		out.MemoryMB = m.Config.Hardware.MemoryMB
		out.DiskGB, out.OSDiskGB, out.DataDisks = diskState(m.Config.Hardware.Device)
		ann := decodeAnnotation(m.Config.Annotation)
		metadata := make(map[string]string, len(ann.Metadata)+2)
		for k, v := range ann.Metadata {
			metadata[k] = v
		}
		if ann.Template != "" {
			out.Template = ann.Template
			metadata[metadataKeyTemplate] = ann.Template
		}
		if ann.IP != "" {
			out.IP = ann.IP
			metadata[metadataKeyIP] = ann.IP
		}
		if len(metadata) > 0 {
			out.Metadata = metadata
		}
	}
	out.PowerState = string(m.Runtime.PowerState)
	out.PoweredOn = m.Runtime.PowerState == types.VirtualMachinePowerStatePoweredOn
	if out.IP == "" && m.Guest != nil {
		out.IP = m.Guest.IpAddress
	}
	return out
}

// CreateVM clones a worker and configures it before the first power-on.
//
// The order matters: the clone carries the annotation (so the VM is
// discoverable if the pass dies), the guestinfo documents (they are only read
// on the first boot) and the NIC backing; then the system disk and the data
// disks are grown and attached, and only then is the VM powered on.
//
// A VM with the slot name that already exists is finished, not cloned again:
// the ledger has already given this slot its name, so a restart in the middle
// of a create must converge on it.
func (p *Provider) CreateVM(ctx context.Context, spec cloud.CreateVMSpec) (cloud.VM, error) {
	ctx, cancel := p.client.opCtx(ctx)
	defer cancel()
	if err := ctx.Err(); err != nil {
		return cloud.VM{}, err
	}
	if err := validateCreateVMSpec(spec); err != nil {
		return cloud.VM{}, err
	}
	if err := p.validatePlacement(); err != nil {
		return cloud.VM{}, err
	}

	cl, f, err := p.base(ctx)
	if err != nil {
		return cloud.VM{}, err
	}
	folder, folderPath, err := p.folder(ctx, f)
	if err != nil {
		return cloud.VM{}, err
	}
	if existing, gerr := f.VirtualMachine(ctx, folderPath+"/"+spec.Name); gerr == nil {
		return p.finishCreate(ctx, existing, spec)
	} else if !isNotFound(gerr) {
		return cloud.VM{}, fmt.Errorf("vsphere: look up VM %q: %w", spec.Name, gerr)
	}

	cluster, err := p.cluster(ctx, f)
	if err != nil {
		return cloud.VM{}, err
	}
	pool, err := p.resourcePool(ctx, f, cluster)
	if err != nil {
		return cloud.VM{}, fmt.Errorf("vsphere: resource pool of cluster %q: %w", p.placement.Cluster, err)
	}
	ds, err := p.datastore(ctx, f)
	if err != nil {
		return cloud.VM{}, err
	}
	network, err := p.network(ctx, f)
	if err != nil {
		return cloud.VM{}, err
	}
	tpl, err := f.VirtualMachine(ctx, spec.Template)
	if err != nil {
		return cloud.VM{}, fmt.Errorf("vsphere: template %q: %w", spec.Template, err)
	}
	devices, err := tpl.Device(ctx)
	if err != nil {
		return cloud.VM{}, fmt.Errorf("vsphere: read devices of template %q: %w", spec.Template, err)
	}
	nics := devices.SelectByType((*types.VirtualEthernetCard)(nil))
	if len(nics) == 0 {
		return cloud.VM{}, fmt.Errorf("vsphere: template %q has no network adapter", spec.Template)
	}
	backing, err := network.EthernetCardBackingInfo(ctx)
	if err != nil {
		return cloud.VM{}, fmt.Errorf("vsphere: backing for port group %q: %w", p.placement.Network, err)
	}
	// A clone keeps the template MAC unless it is cleared, and two VMs with
	// the same MAC on one port group is a network outage.
	card := nics[0].(types.BaseVirtualEthernetCard).GetVirtualEthernetCard()
	card.Backing = backing
	card.MacAddress = ""
	card.AddressType = string(types.VirtualEthernetCardMacTypeGenerated)
	card.Connectable = &types.VirtualDeviceConnectInfo{StartConnected: true, Connected: true, AllowGuestControl: true}

	metadata := metadataDocument(p.placement, spec.Name, spec.IP)
	if metadata == "" {
		return cloud.VM{}, errors.New("vsphere: cannot render the guest network document: gateway, prefix or nic name is missing")
	}
	extraConfig, err := guestExtraConfig(spec.UserData, metadata, spec.EnableUUID)
	if err != nil {
		return cloud.VM{}, fmt.Errorf("vsphere: encode the guest bootstrap: %w", err)
	}
	cfg := &types.VirtualMachineConfigSpec{
		NumCPUs:     spec.CPUs,
		MemoryMB:    int64(spec.MemoryMB),
		Annotation:  encodeAnnotation(spec.Metadata, spec.IP, spec.Template),
		ExtraConfig: extraConfig,
		DeviceChange: []types.BaseVirtualDeviceConfigSpec{
			&types.VirtualDeviceConfigSpec{Operation: types.VirtualDeviceConfigSpecOperationEdit, Device: nics[0]},
		},
	}

	dsRef := ds.Reference()
	poolRef := pool.Reference()
	loc := types.VirtualMachineRelocateSpec{Datastore: &dsRef, Pool: &poolRef}
	// Placement is the pool's: when it names a host, the clone is created
	// there. vCenter's own placement cannot know that the port group exists
	// on only one host (each host may have its own switch), and its mistake
	// surfaces at power-on, after the clone.
	if p.placement.Host != "" {
		host, herr := p.host(ctx, f, cluster)
		if herr != nil {
			return cloud.VM{}, herr
		}
		href := host.Reference()
		loc.Host = &href
	}
	task, err := tpl.Clone(ctx, folder, spec.Name, types.VirtualMachineCloneSpec{
		Location: loc,
		Config:   cfg,
	})
	if err != nil {
		return cloud.VM{}, fmt.Errorf("vsphere: clone %q from %q: %w", spec.Name, spec.Template, err)
	}
	info, err := task.WaitForResult(ctx, nil)
	if err != nil {
		return cloud.VM{}, fmt.Errorf("vsphere: clone %q from %q: %w", spec.Name, spec.Template, err)
	}
	ref, ok := info.Result.(types.ManagedObjectReference)
	if !ok {
		return cloud.VM{}, fmt.Errorf("vsphere: clone %q returned %T, not a VM reference", spec.Name, info.Result)
	}
	vm := object.NewVirtualMachine(cl.Client, ref)

	if err := p.applyDisks(ctx, vm, ds, spec.Name, spec.OSDiskGB, spec.Disks); err != nil {
		// A clone that never got its disks is not a worker: remove it so the
		// next pass starts from a clean slot instead of resuming a broken VM.
		if derr := destroyVM(ctx, vm); derr != nil {
			log.FromContext(ctx).Error(derr, "remove the half-built VM", "vm", spec.Name)
		}
		return cloud.VM{}, err
	}
	if err := startOrRemove(spec.Name,
		func() error { return powerOn(ctx, vm) },
		func() error { return destroyVM(ctx, vm) }); err != nil {
		return cloud.VM{}, err
	}
	return p.vmInfo(ctx, vm)
}

// startOrRemove powers a fresh clone on and removes it when that fails: a VM
// that never started is not a worker. Leaving it behind gives the folder a
// powered-off VM that no status row owns and nothing ever revisits (I-29) -
// the controller drops the slot after a failed create, so the orphan would
// outlive every retry.
func startOrRemove(name string, start, remove func() error) error {
	if err := start(); err != nil {
		if derr := remove(); derr != nil {
			// The name is the only pointer left to the VM an operator has to
			// delete by hand, because the slot is gone by the time this shows.
			return fmt.Errorf("%w (the VM %q could not be removed either: %v)", err, name, derr)
		}
		return err
	}
	return nil
}

// finishCreate completes a create that an earlier pass did not: the VM exists,
// so it is annotated (when the pass died before that) and powered on.
func (p *Provider) finishCreate(ctx context.Context, vm *object.VirtualMachine, spec cloud.CreateVMSpec) (cloud.VM, error) {
	cur, err := p.vmInfo(ctx, vm)
	if err != nil {
		return cloud.VM{}, err
	}
	if cur.Template == "" {
		annotation := encodeAnnotation(spec.Metadata, spec.IP, spec.Template)
		if annotation != "" {
			task, err := vm.Reconfigure(ctx, types.VirtualMachineConfigSpec{Annotation: annotation})
			if err != nil {
				return cloud.VM{}, fmt.Errorf("vsphere: annotate VM %q: %w", cur.Name, err)
			}
			if err := waitTask(ctx, task, "annotate VM "+cur.Name); err != nil {
				return cloud.VM{}, err
			}
		}
	}
	if !cur.PoweredOn {
		if err := powerOn(ctx, vm); err != nil {
			return cloud.VM{}, err
		}
	}
	return p.vmInfo(ctx, vm)
}

// applyDisks gives a fresh clone its shape: the system disk is grown, the
// controllers the data disks need are added, and each data disk is grown or
// attached at its bus/unit. Every step is a separate reconfigure so the keys
// the API assigns are read back before they are referenced.
func (p *Provider) applyDisks(ctx context.Context, vm *object.VirtualMachine, ds *object.Datastore, vmName string, osDiskGB int32, disks []cloud.DiskSpec) error {
	devices, err := vm.Device(ctx)
	if err != nil {
		return fmt.Errorf("vsphere: read devices of VM %q: %w", vm.Reference().Value, err)
	}

	var changes []types.BaseVirtualDeviceConfigSpec
	if osDiskGB > 0 {
		disk := systemDisk(devices)
		if disk == nil {
			return errors.New("vsphere: the template has no system disk at bus 0/unit 0")
		}
		if want := capacityKB(osDiskGB); disk.CapacityInKB < want {
			setDiskCapacity(disk, want)
			changes = append(changes, editDevice(disk))
		}
	}
	if len(changes) > 0 {
		if err := reconfigure(ctx, vm, types.VirtualMachineConfigSpec{DeviceChange: changes}); err != nil {
			return err
		}
	}

	// Controllers first: a disk can only be added to a bus that exists.
	wanted := map[int32]string{}
	for _, d := range disks {
		if d.SizeGB <= 0 {
			continue
		}
		if controllerAt(devices, d.BusNumber) == nil {
			wanted[d.BusNumber] = d.BusType
		}
	}
	if len(wanted) > 0 {
		buses := make([]int32, 0, len(wanted))
		for bus := range wanted {
			buses = append(buses, bus)
		}
		sort.Slice(buses, func(i, j int) bool { return buses[i] < buses[j] })
		var add []types.BaseVirtualDeviceConfigSpec
		for _, bus := range buses {
			ctrl, err := newController(devices, wanted[bus], bus)
			if err != nil {
				return err
			}
			add = append(add, addDevice(ctrl))
		}
		if err := reconfigure(ctx, vm, types.VirtualMachineConfigSpec{DeviceChange: add}); err != nil {
			return err
		}
		if devices, err = vm.Device(ctx); err != nil {
			return fmt.Errorf("vsphere: re-read devices of VM %q: %w", vm.Reference().Value, err)
		}
	}

	changes = nil
	for i, d := range disks {
		if d.SizeGB <= 0 {
			continue
		}
		want := capacityKB(d.SizeGB)
		if disk := diskAt(devices, d.BusNumber, d.UnitNumber); disk != nil {
			if disk.CapacityInKB < want {
				setDiskCapacity(disk, want)
				changes = append(changes, editDevice(disk))
			}
			continue
		}
		ctrl := controllerAt(devices, d.BusNumber)
		if ctrl == nil {
			return fmt.Errorf("vsphere: disk %d: no SCSI controller at bus %d", i, d.BusNumber)
		}
		// The backing name is the full "[datastore] vm/disk.vmdk" form the API
		// expects; a bare relative path is rejected as invalid. The datastore
		// name comes from the placement rather than from Datastore.Path: the
		// object handed to the create has no inventory path loaded, and Path
		// would render an empty bracket ("[]/vmfs/volumes/...").
		name := fmt.Sprintf("[%s] %s/%s-data-%d-%d.vmdk",
			p.placement.Datastore, vmName, vmName, d.BusNumber, d.UnitNumber)
		changes = append(changes, addDiskDevice(newDisk(devices, ctrl, ds.Reference(), name, d.UnitNumber, want)))
	}
	if len(changes) > 0 {
		if err := reconfigure(ctx, vm, types.VirtualMachineConfigSpec{DeviceChange: changes}); err != nil {
			return err
		}
	}

	// A device the API accepted but stored without capacity would only surface
	// at power-on, as "Unable to enumerate all disks", and would leave a
	// half-built VM behind. Check the stored shape here, while the create can
	// still fail with a message that names the disk.
	if len(disks) == 0 {
		return nil
	}
	devices, err = vm.Device(ctx)
	if err != nil {
		return fmt.Errorf("vsphere: re-read devices of VM %q: %w", vm.Reference().Value, err)
	}
	for i, d := range disks {
		if d.SizeGB <= 0 {
			continue
		}
		want := capacityKB(d.SizeGB)
		disk := diskAt(devices, d.BusNumber, d.UnitNumber)
		if disk == nil {
			return fmt.Errorf("vsphere: disk %d (bus %d unit %d) is not on the VM after the reconfigure", i, d.BusNumber, d.UnitNumber)
		}
		if disk.CapacityInKB < want {
			return fmt.Errorf("vsphere: disk %d (bus %d unit %d) is %d KB after the reconfigure, want %d KB",
				i, d.BusNumber, d.UnitNumber, disk.CapacityInKB, want)
		}
	}
	return nil
}

// DeleteVM removes a worker and its disks. A VM that is already gone counts as
// done, so a retried scale-in finishes instead of failing.
func (p *Provider) DeleteVM(ctx context.Context, id string) error {
	ctx, cancel := p.client.opCtx(ctx)
	defer cancel()
	if err := ctx.Err(); err != nil {
		return err
	}
	vm, name, err := p.vmByID(ctx, id)
	if err != nil {
		if errors.Is(err, cloud.ErrVMNotFound) {
			return nil
		}
		return err
	}
	// A hard power-off is deliberate: the guest has been drained, and a
	// wedged guest must not be able to block its own removal.
	if err := powerOff(ctx, vm, name); err != nil {
		return err
	}
	task, err := vm.Destroy(ctx)
	if err != nil {
		if isManagedObjectNotFound(err) {
			return nil
		}
		return fmt.Errorf("vsphere: delete VM %q: %w", name, err)
	}
	if err := waitTask(ctx, task, "delete VM "+name); err != nil {
		if isManagedObjectNotFound(err) {
			return nil
		}
		return err
	}
	return nil
}

func (p *Provider) PowerOffVM(ctx context.Context, id string) error {
	ctx, cancel := p.client.opCtx(ctx)
	defer cancel()
	if err := ctx.Err(); err != nil {
		return err
	}
	vm, name, err := p.vmByID(ctx, id)
	if err != nil {
		if errors.Is(err, cloud.ErrVMNotFound) {
			return nil
		}
		return err
	}
	return powerOff(ctx, vm, name)
}

func (p *Provider) PowerOnVM(ctx context.Context, id string) error {
	ctx, cancel := p.client.opCtx(ctx)
	defer cancel()
	if err := ctx.Err(); err != nil {
		return err
	}
	vm, name, err := p.vmByID(ctx, id)
	if err != nil {
		if errors.Is(err, cloud.ErrVMNotFound) {
			return nil
		}
		return err
	}
	return powerOnNamed(ctx, vm, name)
}

// CycleVM power-cycles a VM. AutoHeal uses it when the VM is alive but its Node
// object disappeared: a guest reboot re-registers kubelet.
func (p *Provider) CycleVM(ctx context.Context, id string) error {
	if err := p.PowerOffVM(ctx, id); err != nil {
		return err
	}
	return p.PowerOnVM(ctx, id)
}

// ResizeVM grows a powered-off worker. A shrink is refused: vSphere cannot
// reduce CPU or memory and a smaller disk would destroy data, so shrinking is
// a recreate operation, not a resize.
//
// The VM is read first and left untouched when the shape already matches, so a
// step re-driven by a status conflict cannot fail on an already resized VM.
func (p *Provider) ResizeVM(ctx context.Context, id string, cpus, memoryMB, osDiskGB int32, disks []cloud.DiskSpec) (cloud.VM, error) {
	ctx, cancel := p.client.opCtx(ctx)
	defer cancel()
	if err := ctx.Err(); err != nil {
		return cloud.VM{}, err
	}
	vm, name, err := p.vmByID(ctx, id)
	if err != nil {
		return cloud.VM{}, err
	}
	cur, err := p.vmInfo(ctx, vm)
	if err != nil {
		return cloud.VM{}, err
	}
	devices, err := vm.Device(ctx)
	if err != nil {
		return cloud.VM{}, fmt.Errorf("vsphere: read devices of VM %q: %w", name, err)
	}

	switch {
	case cpus > 0 && cur.CPU > 0 && cpus < cur.CPU:
		return cloud.VM{}, fmt.Errorf("VM %q has %d vCPU, cannot shrink to %d", name, cur.CPU, cpus)
	case memoryMB > 0 && cur.MemoryMB > 0 && memoryMB < cur.MemoryMB:
		return cloud.VM{}, fmt.Errorf("VM %q has %d MB memory, cannot shrink to %d MB", name, cur.MemoryMB, memoryMB)
	case osDiskGB > 0 && cur.OSDiskGB > 0 && osDiskGB < cur.OSDiskGB:
		return cloud.VM{}, fmt.Errorf("VM %q has a %d GB system disk, cannot shrink to %d GB", name, cur.OSDiskGB, osDiskGB)
	}
	for _, d := range disks {
		if d.SizeGB <= 0 {
			continue
		}
		have := diskAt(devices, d.BusNumber, d.UnitNumber)
		if have == nil {
			continue
		}
		if size := kbToGB(have.CapacityInKB); size > 0 && d.SizeGB < size {
			return cloud.VM{}, fmt.Errorf("VM %q has a %d GB data disk at bus %d/unit %d, cannot shrink to %d GB",
				name, size, d.BusNumber, d.UnitNumber, d.SizeGB)
		}
	}

	want := func(have, want int32) bool { return want <= 0 || have == want }
	dataMatches := true
	for _, d := range disks {
		if d.SizeGB <= 0 {
			continue
		}
		if have := diskAt(devices, d.BusNumber, d.UnitNumber); have != nil && kbToGB(have.CapacityInKB) != d.SizeGB {
			dataMatches = false
		}
	}
	if want(cur.CPU, cpus) && want(cur.MemoryMB, memoryMB) && want(cur.OSDiskGB, osDiskGB) && dataMatches {
		return cur, nil
	}
	if cur.PowerState != string(types.VirtualMachinePowerStatePoweredOff) {
		return cloud.VM{}, fmt.Errorf("%w: VM %q is %s", cloud.ErrPoweredOn, name, cur.PowerState)
	}

	cfg := types.VirtualMachineConfigSpec{}
	changed := false
	if cpus > 0 && cpus != cur.CPU {
		cfg.NumCPUs = cpus
		changed = true
	}
	if memoryMB > 0 && memoryMB != cur.MemoryMB {
		cfg.MemoryMB = int64(memoryMB)
		changed = true
	}
	var changes []types.BaseVirtualDeviceConfigSpec
	if osDiskGB > 0 && cur.OSDiskGB != osDiskGB {
		disk := systemDisk(devices)
		if disk == nil {
			return cloud.VM{}, fmt.Errorf("vsphere: VM %q has no system disk at bus 0/unit 0", name)
		}
		setDiskCapacity(disk, capacityKB(osDiskGB))
		changes = append(changes, editDevice(disk))
	}
	for _, d := range disks {
		if d.SizeGB <= 0 {
			continue
		}
		disk := diskAt(devices, d.BusNumber, d.UnitNumber)
		if disk == nil {
			continue // a disk the template does not have is not ours to add here
		}
		if wantKB := capacityKB(d.SizeGB); disk.CapacityInKB < wantKB {
			setDiskCapacity(disk, wantKB)
			changes = append(changes, editDevice(disk))
		}
	}
	if len(changes) > 0 {
		cfg.DeviceChange = changes
		changed = true
	}
	if changed {
		if err := reconfigure(ctx, vm, cfg); err != nil {
			return cloud.VM{}, err
		}
	}

	info, err := p.vmInfo(ctx, vm)
	if err != nil {
		return cloud.VM{}, err
	}
	if !want(info.CPU, cpus) || !want(info.MemoryMB, memoryMB) || !want(info.OSDiskGB, osDiskGB) {
		return cloud.VM{}, fmt.Errorf("VM %q reports %d vCPU / %d MB / %d GB after the resize, want %d / %d / %d",
			name, info.CPU, info.MemoryMB, info.OSDiskGB, cpus, memoryMB, osDiskGB)
	}
	return info, nil
}

// ---- helpers -------------------------------------------------------------

func reconfigure(ctx context.Context, vm *object.VirtualMachine, cfg types.VirtualMachineConfigSpec) error {
	task, err := vm.Reconfigure(ctx, cfg)
	if err != nil {
		return fmt.Errorf("vsphere: reconfigure VM %q: %w", vm.Reference().Value, err)
	}
	return waitTask(ctx, task, "reconfigure VM "+vm.Reference().Value)
}

func waitTask(ctx context.Context, task *object.Task, what string) error {
	if err := task.Wait(ctx); err != nil {
		return fmt.Errorf("vsphere: %s: %w", what, err)
	}
	return nil
}

// destroyVM deletes a VM and waits for the task.
func destroyVM(ctx context.Context, vm *object.VirtualMachine) error {
	task, err := vm.Destroy(ctx)
	if err != nil {
		return err
	}
	return task.Wait(ctx)
}

func powerOn(ctx context.Context, vm *object.VirtualMachine) error {
	return powerOnNamed(ctx, vm, vm.Reference().Value)
}

func powerOnNamed(ctx context.Context, vm *object.VirtualMachine, name string) error {
	state, err := vm.PowerState(ctx)
	if err != nil {
		return fmt.Errorf("vsphere: state of VM %q: %w", name, err)
	}
	if state == types.VirtualMachinePowerStatePoweredOn {
		return nil
	}
	task, err := vm.PowerOn(ctx)
	if err != nil {
		return fmt.Errorf("vsphere: power on VM %q: %w", name, err)
	}
	return waitTask(ctx, task, "power on VM "+name)
}

func powerOff(ctx context.Context, vm *object.VirtualMachine, name string) error {
	state, err := vm.PowerState(ctx)
	if err != nil {
		return fmt.Errorf("vsphere: state of VM %q: %w", name, err)
	}
	if state == types.VirtualMachinePowerStatePoweredOff {
		return nil
	}
	task, err := vm.PowerOff(ctx)
	if err != nil {
		return fmt.Errorf("vsphere: power off VM %q: %w", name, err)
	}
	return waitTask(ctx, task, "power off VM "+name)
}

// ---- disk helpers --------------------------------------------------------

// capacityKB converts a gigabyte size to the kilobytes the API uses.
func capacityKB(gb int32) int64 {
	return int64(gb) * 1024 * 1024
}

// setDiskCapacity writes both capacity fields. capacityInBytes is the one
// vSphere 8 reads when a disk is created or grown - "clients must initialize it
// when creating a new non-RDM disk" - while capacityInKB is deprecated. With
// only the deprecated field set the API accepts the device and creates it with
// 0 bytes, and the VM then fails to power on with "Unable to enumerate all
// disks".
func setDiskCapacity(disk *types.VirtualDisk, kb int64) {
	disk.CapacityInKB = kb
	disk.CapacityInBytes = kb * 1024
}

func kbToGB(kb int64) int32 {
	if kb <= 0 {
		return 0
	}
	return int32((kb + 1024*1024 - 1) / (1024 * 1024))
}

// scsiBusType maps spec.flavor.disks[].busType onto a controller device.
func scsiBusType(busType string) (types.BaseVirtualDevice, error) {
	switch strings.ToLower(strings.TrimSpace(busType)) {
	case "", "paravirtual", "pvscsi":
		return &types.ParaVirtualSCSIController{HotAddRemove: types.NewBool(true)}, nil
	case "lsilogic":
		return &types.VirtualLsiLogicController{}, nil
	case "lsilogicsas":
		return &types.VirtualLsiLogicSASController{}, nil
	case "buslogic":
		return &types.VirtualBusLogicController{}, nil
	default:
		return nil, fmt.Errorf("unsupported disk bus type %q (paravirtual, lsilogic, lsilogicsas, buslogic)", busType)
	}
}

func newController(devices object.VirtualDeviceList, busType string, bus int32) (types.BaseVirtualDevice, error) {
	dev, err := scsiBusType(busType)
	if err != nil {
		return nil, err
	}
	ctrl := dev.(types.BaseVirtualSCSIController).GetVirtualSCSIController()
	ctrl.BusNumber = bus
	ctrl.Key = devices.NewKey()
	ctrl.ScsiCtlrUnitNumber = 7
	return dev, nil
}

func controllerAt(devices object.VirtualDeviceList, bus int32) types.BaseVirtualDevice {
	for _, d := range devices.SelectByType((*types.VirtualSCSIController)(nil)) {
		if c, ok := d.(types.BaseVirtualSCSIController); ok && c.GetVirtualSCSIController().BusNumber == bus {
			return d
		}
	}
	return nil
}

func busOf(devices object.VirtualDeviceList, disk *types.VirtualDisk) int32 {
	for _, d := range devices.SelectByType((*types.VirtualSCSIController)(nil)) {
		c, ok := d.(types.BaseVirtualSCSIController)
		if !ok {
			continue
		}
		if ctl := c.GetVirtualSCSIController(); ctl.Key == disk.ControllerKey {
			return ctl.BusNumber
		}
	}
	return -1
}

func unitOf(disk *types.VirtualDisk) int32 {
	if disk.UnitNumber == nil {
		return 0
	}
	return *disk.UnitNumber
}

func diskAt(devices object.VirtualDeviceList, bus, unit int32) *types.VirtualDisk {
	for _, d := range devices.SelectByType((*types.VirtualDisk)(nil)) {
		disk := d.(*types.VirtualDisk)
		if busOf(devices, disk) == bus && unitOf(disk) == unit {
			return disk
		}
	}
	return nil
}

func systemDisk(devices object.VirtualDeviceList) *types.VirtualDisk {
	return diskAt(devices, 0, 0)
}

func newDisk(devices object.VirtualDeviceList, ctrl types.BaseVirtualDevice, ds types.ManagedObjectReference, fileName string, unit int32, kb int64) *types.VirtualDisk {
	// The file name is explicit on purpose: an empty backing name is documented
	// as "one will be chosen for you", but on this vCenter it produced a
	// zero-byte device that could not be powered on ("Unable to enumerate all
	// disks"). A path inside the VM's own directory is also what Terraform and
	// the vSphere UI create.
	disk := devices.CreateDisk(ctrl.(types.BaseVirtualController), ds, fileName)
	disk.ControllerKey = ctrl.GetVirtualDevice().Key
	u := unit
	disk.UnitNumber = &u
	disk.CapacityInKB = kb
	return disk
}

func addDevice(device types.BaseVirtualDevice) types.BaseVirtualDeviceConfigSpec {
	return &types.VirtualDeviceConfigSpec{Operation: types.VirtualDeviceConfigSpecOperationAdd, Device: device}
}

// addDiskDevice is addDevice for a disk whose backing file does not exist yet.
// Without FileOperation=create vCenter treats the file name as an existing disk
// and fails with "File [...] ... was not found"; govc's vm.disk.create sets the
// same flag.
func addDiskDevice(device types.BaseVirtualDevice) types.BaseVirtualDeviceConfigSpec {
	return &types.VirtualDeviceConfigSpec{
		Operation:     types.VirtualDeviceConfigSpecOperationAdd,
		FileOperation: types.VirtualDeviceConfigSpecFileOperationCreate,
		Device:        device,
	}
}

func editDevice(device types.BaseVirtualDevice) types.BaseVirtualDeviceConfigSpec {
	return &types.VirtualDeviceConfigSpec{Operation: types.VirtualDeviceConfigSpecOperationEdit, Device: device}
}

// diskState reports the total, the system disk and the data disks, by bus/unit
// so a size a human raised in vCenter is noticed the way a raised CPU count is.
func diskState(devices []types.BaseVirtualDevice) (totalGB, osGB int32, data []cloud.DiskState) {
	buses := map[int32]int32{}
	for _, d := range devices {
		if c, ok := d.(types.BaseVirtualSCSIController); ok {
			ctl := c.GetVirtualSCSIController()
			buses[ctl.Key] = ctl.BusNumber
		}
	}
	var totalKB, osKB int64
	for _, d := range devices {
		disk, ok := d.(*types.VirtualDisk)
		if !ok {
			continue
		}
		totalKB += disk.CapacityInKB
		bus, known := buses[disk.ControllerKey]
		unit := unitOf(disk)
		if known && bus == 0 && unit == 0 {
			osKB = disk.CapacityInKB
			continue
		}
		if !known {
			bus = -1
		}
		data = append(data, cloud.DiskState{BusNumber: bus, UnitNumber: unit, SizeGB: kbToGB(disk.CapacityInKB)})
	}
	return kbToGB(totalKB), kbToGB(osKB), data
}

func matchesSelector(metadata, selector map[string]string) bool {
	for k, v := range selector {
		if metadata[k] != v {
			return false
		}
	}
	return true
}

func isNotFound(err error) bool {
	var nf *find.NotFoundError
	return errors.As(err, &nf)
}

func isManagedObjectNotFound(err error) bool {
	return fault.Is(err, &types.ManagedObjectNotFound{})
}
