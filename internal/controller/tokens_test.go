package controller

import (
	"testing"

	infrav1 "github.com/k8s-nodescale/vcd-k8s-node-controller/api/v1alpha1"
)

// The guest's bootstrap token is renewed while it installs and revoked the
// moment its Node is Ready: a joined node needs no credential, a slow one must
// not lose it.
func TestBootstrapTokenLifecycleFollowsTheWorker(t *testing.T) {
	pool := testPool(1)
	fc := &fakeCloud{}
	fk := &fakeKube{}
	r := newTestReconciler(t, fc, fk, pool)

	reconcileOnce(t, r, pool)
	got := getPool(t, r, pool)
	n := nodeByName(got, "pool-wrk-01")
	if n == nil || n.BootstrapTokenID != "abcdef" {
		t.Fatalf("worker = %#v, want the token id recorded", n)
	}
	if len(fk.renewedTokenIDs) == 0 || fk.renewedTokenIDs[0] != "abcdef" {
		t.Fatalf("renewed = %v, want the token kept alive while Provisioning", fk.renewedTokenIDs)
	}
	if len(fk.revokedTokenIDs) != 0 {
		t.Fatalf("revoked = %v before the node is Ready", fk.revokedTokenIDs)
	}

	fk.nodes = readyNodes("pool-wrk-01")
	reconcileOnce(t, r, pool)

	got = getPool(t, r, pool)
	n = nodeByName(got, "pool-wrk-01")
	if n == nil || n.Phase != infrav1.NodePhaseReady {
		t.Fatalf("worker = %#v, want Ready", n)
	}
	if n.BootstrapTokenID != "" {
		t.Fatalf("bootstrapTokenID = %q, want it cleared after the join", n.BootstrapTokenID)
	}
	if len(fk.revokedTokenIDs) != 1 || fk.revokedTokenIDs[0] != "abcdef" {
		t.Fatalf("revoked = %v, want the token revoked once", fk.revokedTokenIDs)
	}

	// A later pass does not try to revoke it again.
	reconcileOnce(t, r, pool)
	if len(fk.revokedTokenIDs) != 1 {
		t.Fatalf("revoked = %v, want no second revoke", fk.revokedTokenIDs)
	}
}
