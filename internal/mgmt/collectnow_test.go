package mgmt

import (
	"context"
	"testing"

	"sigs.k8s.io/controller-runtime/pkg/client"

	infrav1 "github.com/k8s-nodescale/vcd-k8s-node-controller/api/v1alpha1"
)

// A write has to be visible to the page on the refresh it makes straight
// afterwards. Without the invalidation the page gets the collector's previous
// pass back - up to a full collector period of "my click did nothing", which is
// how the CPU assumption switch looked broken on 2026-11-20.
func TestCollectNowMakesAWriteVisible(t *testing.T) {
	ctx := context.Background()
	pool := testPool(1)
	c := testClient(t, pool)

	col := &Collector{Client: c}
	if _, err := col.CollectOnce(ctx); err != nil {
		t.Fatalf("first collection: %v", err)
	}
	_, first, err := col.Snapshot()
	if err != nil {
		t.Fatal(err)
	}

	var stored infrav1.K8sWorkerPool
	if err := c.Get(ctx, client.ObjectKey{Name: pool.Name}, &stored); err != nil {
		t.Fatal(err)
	}
	stored.Spec.Replicas = 3
	if err := c.Update(ctx, &stored); err != nil {
		t.Fatalf("write replicas: %v", err)
	}

	// The cached pass still says 1: that is the staleness the page used to see.
	pools, _, _ := col.Snapshot()
	if pools[0].Replicas != 1 {
		t.Fatalf("the cached snapshot already reads %d, want the stale 1", pools[0].Replicas)
	}

	(&Server{Client: c, Collector: col}).collectNow(ctx)
	pools, after, err := col.Snapshot()
	if err != nil {
		t.Fatal(err)
	}
	if !after.After(first) {
		t.Fatalf("snapshot timestamp did not move: %v", after)
	}
	if pools[0].Replicas != 3 {
		t.Fatalf("after collectNow replicas = %d, want 3", pools[0].Replicas)
	}
}
