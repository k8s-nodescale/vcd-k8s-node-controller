// Package cloud defines the vCloud Director provider contract used by the reconciler.
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

// VDCCapacity is what a vDC still has free for another worker.
//
// It is what vCD itself rates a new VM against, so a caller can refuse a create
// that cannot fit instead of letting it fail halfway through a clone.
type VDCCapacity struct {
	// AllocationModel is vCD's name for the model, e.g. AllocationPool or
	// PayAsYouGo. It is informational: a zero Limit already means "no cap".
	AllocationModel string
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
func (c VDCCapacity) Fits(req CapacityRequest, mhzPerCore int64) (bool, []string) {
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
func (c VDCCapacity) StorageProfile(name string) (StorageProfileCapacity, bool) {
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
	Metadata map[string]string
}

// CreateVMSpec is the input for provisioning a single worker VM.
type CreateVMSpec struct {
	Name       string
	IP         string
	VApp       string
	OrgNetwork string
	Catalog    string
	CatalogOrg string
	Template   string
	CPUs       int32
	MemoryMB   int32

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

// Provider abstracts all vCD operations required for worker scaling.
type Provider interface {
	ListWorkerVMs(ctx context.Context, vapp string, selector map[string]string) ([]VM, error)
	// GetVM reads one VM by id. It is the cheap read the rollout does before
	// it changes anything: a step retried after a status conflict must not
	// power-cycle a VM that is already in the wanted state.
	GetVM(ctx context.Context, vappName, id string) (VM, error)
	CreateVM(ctx context.Context, spec CreateVMSpec) (VM, error)
	DeleteVM(ctx context.Context, vappName, id string) error
	// PowerOffVM hard-powers-off a VM, keeping it in the vApp. It is
	// idempotent and treats a missing VM as done.
	PowerOffVM(ctx context.Context, vappName, id string) error
	// PowerOnVM powers a stopped VM on and is idempotent.
	PowerOnVM(ctx context.Context, vappName, id string) error
	// CycleVM power-cycles a VM (off then on) and is idempotent. AutoHeal uses
	// it when the VM is alive but its Node object disappeared.
	CycleVM(ctx context.Context, vappName, id string) error
	// ResizeVM grows a powered-off VM to the requested shape and returns the
	// refreshed VM. Shrinking is refused: vCD cannot shrink CPU/memory and a
	// smaller disk would destroy data, so a shrink is a recreate operation.
	ResizeVM(ctx context.Context, vappName, id string, cpus, memoryMB, osDiskGB int32) (VM, error)

	// Capacity reads what the vDC still has free: compute capacity and the
	// storage profiles a worker could be placed on. It is the pre-flight the
	// reconciler runs before a create or a resize, so a request that cannot fit
	// is parked instead of failing inside vCD.
	Capacity(ctx context.Context) (VDCCapacity, error)
	AddPoolMember(ctx context.Context, pool PoolRef, ip string) error
	RemovePoolMember(ctx context.Context, pool PoolRef, ip string) error
}
