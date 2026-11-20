package vsphere

import (
	"testing"

	"github.com/vmware/govmomi/vim25/mo"
	"github.com/vmware/govmomi/vim25/types"
)

// dsref is a datastore reference that is comparable and readable in a failure.
func dsref(name string) types.ManagedObjectReference {
	return types.ManagedObjectReference{Type: "Datastore", Value: name}
}

// host builds a usable host that mounts the named datastores.
func host(name string, mounts ...string) mo.HostSystem {
	h := mo.HostSystem{}
	h.Name = name
	h.Summary.Hardware = &types.HostHardwareSummary{}
	for _, m := range mounts {
		h.Datastore = append(h.Datastore, dsref(m))
	}
	return h
}

func names(refs []types.ManagedObjectReference) []string {
	out := make([]string, 0, len(refs))
	for _, r := range refs {
		out = append(out, r.Value)
	}
	return out
}

func TestCommonDatastoresIntersectsUsableHosts(t *testing.T) {
	hosts := []mo.HostSystem{
		host("h1", "shared", "only-h1"),
		host("h2", "shared", "only-h2"),
	}
	got := names(commonDatastores(hosts))
	if len(got) != 1 || got[0] != "shared" {
		t.Fatalf("every host must mount a datastore for it to be available, got %v", got)
	}
}

func TestCommonDatastoresSkipsHostsThatCannotTakeAClone(t *testing.T) {
	// A host in maintenance mounts a datastore the others do not. It takes no
	// clone, so it must not narrow the list to nothing.
	maint := host("maint", "only-maint")
	maint.Summary.Runtime = &types.HostRuntimeInfo{InMaintenanceMode: true}
	down := host("down", "only-down")
	down.Summary.Runtime = &types.HostRuntimeInfo{ConnectionState: types.HostSystemConnectionStateDisconnected}

	hosts := []mo.HostSystem{host("h1", "a", "b"), maint, host("h2", "a", "b"), down}
	got := names(commonDatastores(hosts))
	if len(got) != 2 || got[0] != "a" || got[1] != "b" {
		t.Fatalf("maintenance and disconnected hosts must be ignored, got %v", got)
	}
}

func TestCommonDatastoresFallsBackToTheUnion(t *testing.T) {
	// No datastore is common. The list still answers "what does this cluster
	// offer", and the clone remains the authority on reachability.
	hosts := []mo.HostSystem{
		host("h1", "a", "shared-on-h1"),
		host("h2", "b", "shared-on-h2"),
	}
	got := names(commonDatastores(hosts))
	want := []string{"a", "shared-on-h1", "b", "shared-on-h2"}
	if len(got) != len(want) {
		t.Fatalf("expected the union when nothing is common, got %v", got)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("expected the union when nothing is common, got %v want %v", got, want)
		}
	}
}

func TestUnionRefsDeduplicates(t *testing.T) {
	got := names(unionRefs([]types.ManagedObjectReference{dsref("a"), dsref("b")}, []types.ManagedObjectReference{dsref("b"), dsref("c")}))
	if len(got) != 3 || got[0] != "a" || got[1] != "b" || got[2] != "c" {
		t.Fatalf("union must keep order and drop duplicates, got %v", got)
	}
}

func TestCommonDatastoresWithoutUsableHosts(t *testing.T) {
	maint := host("maint", "a")
	maint.Summary.Runtime = &types.HostRuntimeInfo{InMaintenanceMode: true}
	if got := commonDatastores([]mo.HostSystem{maint}); len(got) != 0 {
		t.Fatalf("a host that cannot take a clone offers nothing, got %v", names(got))
	}
}
