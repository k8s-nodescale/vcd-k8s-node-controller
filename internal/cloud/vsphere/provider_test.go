package vsphere

import (
	"context"
	"errors"
	"testing"

	"github.com/k8s-nodescale/vcd-k8s-node-controller/internal/cloud"
)

func testPlacement() cloud.Placement {
	return cloud.Placement{
		Provider: cloud.ProviderVSphere,
		VSphere:  &cloud.VSpherePlacement{Datacenter: "dc", Cluster: "cluster", Datastore: "ds", Network: "pg"},
	}
}

// The stub sits behind the same contract as vCD, and every call says so
// instead of pretending to work.
func TestTheStubSatisfiesTheContractAndRefuses(t *testing.T) {
	prov, err := New().For(testPlacement())
	if err != nil {
		t.Fatalf("For: %v", err)
	}
	ctx := context.Background()
	calls := map[string]func() error{
		"ListWorkerVMs": func() error { _, err := prov.ListWorkerVMs(ctx, nil); return err },
		"GetVM":         func() error { _, err := prov.GetVM(ctx, "vm-1"); return err },
		"CreateVM":      func() error { _, err := prov.CreateVM(ctx, cloud.CreateVMSpec{Name: "w"}); return err },
		"DeleteVM":      func() error { return prov.DeleteVM(ctx, "vm-1") },
		"PowerOffVM":    func() error { return prov.PowerOffVM(ctx, "vm-1") },
		"PowerOnVM":     func() error { return prov.PowerOnVM(ctx, "vm-1") },
		"CycleVM":       func() error { return prov.CycleVM(ctx, "vm-1") },
		"ResizeVM":      func() error { _, err := prov.ResizeVM(ctx, "vm-1", 4, 8192, 40); return err },
		"Capacity":      func() error { _, err := prov.Capacity(ctx); return err },
	}
	for name, call := range calls {
		if err := call(); !errors.Is(err, cloud.ErrNotImplemented) {
			t.Fatalf("%s: err = %v, want ErrNotImplemented", name, err)
		}
	}
}

// A platform with no load-balancer pool leaves membership to the operator, so
// the stub must not implement the optional interface.
func TestTheStubHasNoPoolMembership(t *testing.T) {
	prov, err := New().For(testPlacement())
	if err != nil {
		t.Fatalf("For: %v", err)
	}
	if _, ok := prov.(cloud.PoolMembership); ok {
		t.Fatal("the vSphere provider implements PoolMembership")
	}
}

func TestForRejectsAnotherPlatform(t *testing.T) {
	if _, err := New().For(cloud.Placement{Provider: cloud.ProviderVCD}); err == nil {
		t.Fatal("For accepted a vcd placement")
	}
	if _, err := New().For(cloud.Placement{Provider: cloud.ProviderVSphere}); err == nil {
		t.Fatal("For accepted a placement with no spec.vsphere")
	}
}
