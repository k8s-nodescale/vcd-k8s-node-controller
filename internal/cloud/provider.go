// Package cloud defines the platform contract the reconciler drives: the worker
// VMs of one pool, their compute shape, and the capacity they need.
//
// vCloud Director is one implementation (internal/cloud/vcd). A second platform
// only has to satisfy these interfaces, and it is chosen per pool through a
// Factory, so nothing in the contract carries a vApp, network or catalog name.
package cloud

import (
	"context"
	"errors"
	"fmt"
	"sort"
	"strings"
)

// ErrNotImplemented marks provider methods that still need a real implementation.
var ErrNotImplemented = errors.New("cloud provider method not implemented")

// ErrVMNotFound means the VM is gone from the vApp. A caller that races a
// deletion treats it as done, not as a failure.
var ErrVMNotFound = errors.New("VM not found")

// ErrPoweredOn means the call needs the VM powered off. It is a state to
// correct, not a failure: the caller stops the VM and retries.
var ErrPoweredOn = errors.New("VM must be powered off")

// ErrPoolNotConfigured means the worker pool has an ingress load balancer
// configured that this provider cannot address (missing edge gateway name, or
// NSX-T, which is not implemented yet). Callers treat it as a warning: the node
// is drained and deleted, but its pool member must be removed by hand.
var ErrPoolNotConfigured = errors.New("ingress pool not addressable by this provider")

// Provider names select the implementation behind a Factory.
const (
	// ProviderVCD is vCloud Director (internal/cloud/vcd).
	ProviderVCD = "vcd"
	// ProviderVSphere is the vSphere platform API. It is a stub for now
	// (internal/cloud/vsphere).
	ProviderVSphere = "vsphere"
)

// Placement says where a pool's workers live. The controller builds it from the
// pool's CR; each implementation reads the block it owns and ignores the rest.
type Placement struct {
	// Provider names the implementation, one of the Provider constants. An
	// empty value means ProviderVCD.
	Provider string
	// VCD is set when Provider is ProviderVCD.
	VCD *VCDPlacement
	// VSphere is set when Provider is ProviderVSphere.
	VSphere *VSpherePlacement
}

// VCDPlacement names the vCloud Director objects one pool lives in.
type VCDPlacement struct {
	Org        string
	VDC        string
	VApp       string
	OrgNetwork string
	Catalog    string
	CatalogOrg string
}

// VSpherePlacement names the vSphere objects one pool lives in. It is declared
// so the stub has a shape to fill in; the vSphere implementation is not
// written yet.
type VSpherePlacement struct {
	Datacenter   string
	Cluster      string
	ResourcePool string
	Datastore    string
	Network      string
	Folder       string
}

// DiskSpec describes an internal disk to attach to a worker VM.
type DiskSpec struct {
	SizeGB         int32
	Mount          string
	BusType        string
	BusNumber      int32
	UnitNumber     int32
	StorageProfile string
}

// ResourceCapacity is one capacity category of a vDC: what it allows and what
// it already counts as consumed, in the unit named by the field that carries it
// (MHz for CPU, MB for memory and storage).
type ResourceCapacity struct {
	// Limit is the ceiling the vDC enforces. Zero means the vDC does not cap
	// the category (pay-as-you-go and flex allocation models).
	Limit int64
	// Used is what vCD counts as consumed, in the same unit as Limit.
	Used int64
}

// StorageProfileCapacity is one storage profile of the vDC.
type StorageProfileCapacity struct {
	Name    string
	Default bool
	LimitMB int64
	UsedMB  int64
}

// PlatformCapacity is what the platform still has free for another worker.
//
// It is what the platform itself rates a new VM against, so a caller can refuse a create
// that cannot fit instead of letting it fail halfway through a clone.
type PlatformCapacity struct {
	// Model is the platform's own name for its allocation model, e.g. vCD's
	// AllocationPool or PayAsYouGo. It is informational: a zero Limit already
	// means "no cap".
	Model string
	// CPUMHzPerCore is the speed the vDC rates one vCPU at, as configured on
	// the vDC. Zero means it was not readable for this account.
	CPUMHzPerCore int64
	CPU           ResourceCapacity // MHz
	MemoryMB      ResourceCapacity // MB
	Storage       []StorageProfileCapacity
}

// CapacityRequest is the capacity one worker would add.
type CapacityRequest struct {
	CPUs     int32
	MemoryMB int32
	// StorageMB is the disk capacity per storage profile name. The empty name
	// means the vDC's default storage profile.
	StorageMB map[string]int64
}

// Fits reports whether req fits in the free capacity of c and, when it does
// not, one reason per category that is short. mhzPerCore converts the request's
// vCPUs into the MHz the vDC rates them at; zero skips the CPU check, because
// without the speed the conversion would be a guess.
//
// A category the vDC does not cap (Limit 0) never blocks, and a storage profile
// the request names but the vDC does not report is skipped: the create is the
// authority on whether that profile exists.
func (c PlatformCapacity) Fits(req CapacityRequest, mhzPerCore int64) (bool, []string) {
	var short []string
	if mhzPerCore > 0 && c.CPU.Limit > 0 {
		need := int64(req.CPUs) * mhzPerCore
		if free := c.CPU.Limit - c.CPU.Used; need > free {
			short = append(short, fmt.Sprintf("CPU: %d vCPU at %d MHz need %d MHz, %d MHz free",
				req.CPUs, mhzPerCore, need, max(free, 0)))
		}
	}
	if c.MemoryMB.Limit > 0 {
		need := int64(req.MemoryMB)
		if free := c.MemoryMB.Limit - c.MemoryMB.Used; need > free {
			short = append(short, fmt.Sprintf("memory: need %d MB, %d MB free", need, max(free, 0)))
		}
	}
	names := make([]string, 0, len(req.StorageMB))
	for name := range req.StorageMB {
		names = append(names, name)
	}
	sort.Strings(names)
	for _, name := range names {
		need := req.StorageMB[name]
		if need <= 0 {
			continue
		}
		sp, ok := c.StorageProfile(name)
		if !ok || sp.LimitMB <= 0 {
			continue
		}
		if free := sp.LimitMB - sp.UsedMB; need > free {
			short = append(short, fmt.Sprintf("storage profile %s: need %d MB, %d MB free", sp.Name, need, max(free, 0)))
		}
	}
	return len(short) == 0, short
}

// StorageProfile returns the profile named name, or the vDC's default profile
// when name is empty.
func (c PlatformCapacity) StorageProfile(name string) (StorageProfileCapacity, bool) {
	if name != "" {
		for _, sp := range c.Storage {
			if strings.EqualFold(sp.Name, name) {
				return sp, true
			}
		}
		return StorageProfileCapacity{}, false
	}
	for _, sp := range c.Storage {
		if sp.Default {
			return sp, true
		}
	}
	if len(c.Storage) == 1 {
		return c.Storage[0], true
	}
	return StorageProfileCapacity{}, false
}

// VM is the controller's view of a vCD VM.
type VM struct {
	ID         string
	Name       string
	IP         string
	PowerState string
	// PoweredOn is true while vCD reports the VM running.
	PoweredOn bool
	CPU       int32
	MemoryMB  int32
	// DiskGB is the total capacity of all disks on the VM.
	DiskGB int32
	// OSDiskGB is the system disk (bus 0/unit 0) alone, which is what
	// spec.flavor.osDiskGB describes.
	OSDiskGB int32
	// Template is the image the VM was cloned from, as the platform names it.
	// It is empty when the platform cannot tell - a VM the controller did not
	// create - and an empty template is "no opinion", never a difference.
	Template string
	Metadata map[string]string
}

// CreateVMSpec is the input for provisioning a single worker VM.
type CreateVMSpec struct {
	Name string
	IP   string
	// Template is the image to clone: a vCD vApp template name, or the
	// equivalent of the platform that implements this contract.
	Template string
	CPUs     int32
	MemoryMB int32

	// OSDiskGB grows the cloned template's system disk (bus 0/unit 0) before
	// the first power-on. Zero keeps the template size.
	OSDiskGB int32

	Disks []DiskSpec

	// Initscript seeds cloud-init (writes netplan + NoCloud seed).
	Initscript string
	// UserData is the cloud-init user-data (optional if Initscript embeds it).
	UserData string

	Metadata map[string]string

	// EnableUUID sets disk.EnableUUID=TRUE (required by the vCloud CSI driver).
	EnableUUID bool
}

// PoolRef identifies a load-balancer pool.
type PoolRef struct {
	Type string // nsxv | nsxt
	Name string
	Port int32
	// Edge is the NSX-V edge gateway that owns the pool. Required for nsxv;
	// NSX-V load balancer pools are scoped to an edge, not to the VDC.
	Edge string
}

// WorkerCompute is the compute contract: the VMs that back a pool's nodes.
//
// A Provider is bound to one pool's placement, so no method carries the vApp,
// network or catalog it works in: the binding is the placement.
type WorkerCompute interface {
	// ListWorkerVMs returns the pool's VMs. selector narrows the list to the
	// VMs that carry every matching metadata entry; nil means all of them.
	ListWorkerVMs(ctx context.Context, selector map[string]string) ([]VM, error)
	// GetVM reads one VM by id. It is the cheap read the rollout does before
	// it changes anything: a step retried after a status conflict must not
	// power-cycle a VM that is already in the wanted state.
	GetVM(ctx context.Context, id string) (VM, error)
	CreateVM(ctx context.Context, spec CreateVMSpec) (VM, error)
	DeleteVM(ctx context.Context, id string) error
	// PowerOffVM hard-powers-off a VM, keeping it in the pool. It is
	// idempotent and treats a missing VM as done.
	PowerOffVM(ctx context.Context, id string) error
	// PowerOnVM powers a stopped VM on and is idempotent.
	PowerOnVM(ctx context.Context, id string) error
	// CycleVM power-cycles a VM (off then on) and is idempotent. AutoHeal uses
	// it when the VM is alive but its Node object disappeared.
	CycleVM(ctx context.Context, id string) error
	// ResizeVM grows a powered-off VM to the requested shape and returns the
	// refreshed VM. Shrinking is refused: the platform cannot shrink CPU or
	// memory and a smaller disk would destroy data, so a shrink is a recreate.
	ResizeVM(ctx context.Context, id string, cpus, memoryMB, osDiskGB int32) (VM, error)
}

// CapacityReader reads what the platform still has free: compute capacity and
// the storage profiles a worker could be placed on. It is the pre-flight the
// reconciler runs before a create or a resize, so a request that cannot fit is
// parked instead of failing inside the platform. An implementation that cannot
// read capacity returns the zero PlatformCapacity, which caps nothing.
type CapacityReader interface {
	Capacity(ctx context.Context) (PlatformCapacity, error)
}

// Provider is one pool's platform client, bound to the placement the Factory
// resolved.
type Provider interface {
	WorkerCompute
	CapacityReader
}

// PoolMembership is the optional half: a load-balancer pool the controller can
// edit. A platform without one (or with one it cannot address) does not
// implement it, and the reconciler then leaves membership to the operator. vCD
// implements it for NSX-V and reports ErrPoolNotConfigured for a pool it cannot
// address.
type PoolMembership interface {
	AddPoolMember(ctx context.Context, pool PoolRef, ip string) error
	RemovePoolMember(ctx context.Context, pool PoolRef, ip string) error
}

// Factory builds the Provider for one pool from its placement. It is what the
// controller is wired with, so the platform is a property of the pool, not of
// the process.
type Factory interface {
	For(placement Placement) (Provider, error)
}

// Registry is a Factory that dispatches on Placement.Provider. A pool that
// names no provider gets ProviderVCD.
type Registry map[string]Factory

// For implements Factory.
func (r Registry) For(placement Placement) (Provider, error) {
	name := placement.Provider
	if name == "" {
		name = ProviderVCD
	}
	f, ok := r[name]
	if !ok {
		names := make([]string, 0, len(r))
		for n := range r {
			names = append(names, n)
		}
		sort.Strings(names)
		return nil, fmt.Errorf("no cloud provider named %q (registered: %s)", name, strings.Join(names, ", "))
	}
	return f.For(placement)
}
