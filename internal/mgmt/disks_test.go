package mgmt

import (
	"context"
	"strings"
	"testing"

	"sigs.k8s.io/controller-runtime/pkg/client"

	infrav1 "github.com/k8s-nodescale/vcd-k8s-node-controller/api/v1alpha1"
)

// Disks only grow: the page and the controller both refuse a shrink, because
// vCD cannot do it and the only way to a smaller disk is a new VM, which an
// operator has to ask for deliberately.
func TestSetDisksGrowsAndRefusesShrink(t *testing.T) {
	pool := testPool(1)
	pool.Spec.Flavor.Disks = []infrav1.DiskSpec{
		{BusNumber: 1, UnitNumber: 0, Mount: "/var", SizeGB: 10},
		{BusNumber: 1, UnitNumber: 1, Mount: "/var/lib", SizeGB: 30},
	}
	fc := testClient(t, pool)
	s := &Server{Client: fc}
	ctx := context.Background()

	if err := s.SetDisks(ctx, "kion-web-dev", 0, []DiskTarget{{BusNumber: 1, UnitNumber: 0, SizeGB: 40}}); err != nil {
		t.Fatalf("grow: %v", err)
	}
	got := &infrav1.K8sWorkerPool{}
	if err := fc.Get(ctx, client.ObjectKey{Name: "kion-web-dev"}, got); err != nil {
		t.Fatal(err)
	}
	if got.Spec.Flavor.Disks[0].SizeGB != 40 {
		t.Fatalf("/var is %d GB, want 40", got.Spec.Flavor.Disks[0].SizeGB)
	}
	if got.Spec.Flavor.Disks[1].SizeGB != 30 {
		t.Fatalf("/var/lib changed to %d GB; only the named disk may move", got.Spec.Flavor.Disks[1].SizeGB)
	}

	err := s.SetDisks(ctx, "kion-web-dev", 0, []DiskTarget{{BusNumber: 1, UnitNumber: 0, SizeGB: 10}})
	if err == nil {
		t.Fatal("a shrink was accepted")
	}
	if !strings.Contains(err.Error(), "cannot shrink") {
		t.Fatalf("error = %v, want it to say a disk cannot shrink", err)
	}

	if err := s.SetDisks(ctx, "kion-web-dev", 0, []DiskTarget{{UnitNumber: 9, SizeGB: 10}}); err == nil {
		t.Fatal("a disk the pool does not have was accepted")
	}
	if err := s.SetDisks(ctx, "kion-web-dev", 0, nil); err == nil {
		t.Fatal("an empty request was accepted")
	}
}
