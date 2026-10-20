// Package cloud defines the vCloud Director provider contract used by the reconciler.
package cloud

import (
	"context"
	"errors"
)

// ErrNotImplemented marks provider methods that still need a real implementation.
var ErrNotImplemented = errors.New("cloud provider method not implemented")

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
	DiskGB     int32
	Metadata   map[string]string
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
	Disks      []DiskSpec

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
}

// Provider abstracts all vCD operations required for worker scaling.
type Provider interface {
	ListWorkerVMs(ctx context.Context, vapp string, selector map[string]string) ([]VM, error)
	CreateVM(ctx context.Context, spec CreateVMSpec) (VM, error)
	DeleteVM(ctx context.Context, id string) error
	AddPoolMember(ctx context.Context, pool PoolRef, ip string) error
	RemovePoolMember(ctx context.Context, pool PoolRef, ip string) error
}
