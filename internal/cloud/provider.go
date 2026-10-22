// Package cloud defines the vCloud Director provider contract used by the reconciler.
package cloud

import (
	"context"
	"errors"
)

// ErrNotImplemented marks provider methods that still need a real implementation.
var ErrNotImplemented = errors.New("cloud provider method not implemented")

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

// VM is the controller's view of a vCD VM.
type VM struct {
	ID         string
	Name       string
	IP         string
	PowerState string
	CPU        int32
	MemoryMB   int32
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
	AddPoolMember(ctx context.Context, pool PoolRef, ip string) error
	RemovePoolMember(ctx context.Context, pool PoolRef, ip string) error
}
