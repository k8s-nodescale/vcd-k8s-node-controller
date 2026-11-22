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

// The size of a worker's disk is the controller's own number - it declared it
// and the resize path updates it - so the column shows a size even when no
// node-exporter answers. Utilisation stays nil, which is what makes the page grey
// the bar instead of drawing a zero.
func TestDeclaredDisksWithoutAnExporter(t *testing.T) {
	pool := testPool(1)
	pool.Spec.Flavor.OSDiskGB = 20
	pool.Spec.Flavor.Disks = []infrav1.DiskSpec{
		{BusNumber: 1, UnitNumber: 0, Mount: "/var", SizeGB: 10},
		{BusNumber: 1, UnitNumber: 1, Mount: "/var/lib", SizeGB: 40},
	}
	pool.Status.Nodes = []infrav1.WorkerNodeStatus{{
		Name: "pool-wrk-01", NodeName: "pool-wrk-01", Phase: infrav1.NodePhaseReady,
	}}
	node := &pool.Status.Nodes[0]
	node.Flavor = &infrav1.FlavorStatus{
		OSDiskGB: 20,
		Disks: []infrav1.DiskStatus{
			{BusNumber: 1, UnitNumber: 0, SizeGB: 10},
			{BusNumber: 1, UnitNumber: 1, SizeGB: 40},
		},
	}
	// What the collector sends when no exporter answered: mounts and nothing else.
	empty := diskShares(exporterSample{})

	got := declaredDisks(pool, statusNode(pool, node.NodeName), empty)
	want := map[string]float64{"/": 20, "/var": 10, "/var/lib": 40}
	for _, d := range got {
		if d.Percent != nil {
			t.Fatalf("%s: percent = %v, want nil without an exporter", d.Mount, *d.Percent)
		}
		if d.SizeGB != want[d.Mount] {
			t.Fatalf("%s: size = %v, want %v", d.Mount, d.SizeGB, want[d.Mount])
		}
	}
	if len(got) != len(want) {
		t.Fatalf("got %d disks, want %d", len(got), len(want))
	}
}

// A scraped size wins over the declared one: the filesystem is what is really
// there, the spec is what was asked for.
func TestDeclaredDisksKeepsTheScrapedSize(t *testing.T) {
	pool := testPool(1)
	pool.Status.Nodes = []infrav1.WorkerNodeStatus{{Name: "pool-wrk-01", NodeName: "pool-wrk-01"}}
	node := &pool.Status.Nodes[0]
	node.Flavor = &infrav1.FlavorStatus{OSDiskGB: 20}
	pct := 41.0
	scraped := []Disk{{Mount: "/", SizeGB: 19.8, Percent: &pct}}

	got := declaredDisks(pool, node, scraped)
	if got[0].SizeGB != 19.8 {
		t.Fatalf("size = %v, want the scraped 19.8", got[0].SizeGB)
	}
}
