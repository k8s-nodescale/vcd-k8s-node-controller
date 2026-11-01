package mgmt

import (
	"strings"
	"testing"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	infrav1 "github.com/k8s-nodescale/vcd-k8s-node-controller/api/v1alpha1"
)

func poolWithWindow(name string, offset int, count int32) infrav1.K8sWorkerPool {
	return infrav1.K8sWorkerPool{
		ObjectMeta: metav1.ObjectMeta{Name: name},
		Spec: infrav1.K8sWorkerPoolSpec{
			IPPlan: infrav1.IPPlan{
				CIDR: "192.168.100.0/24", WNodeOffset: offset, NodeCount: count,
			},
		},
	}
}

// The page offers the next free window so an operator adding a pool does not do
// address arithmetic: the gap has to be what is left after every window, and the
// suggestion has to fit into it.
func TestAddressSpacesOfferTheNextFreeWindow(t *testing.T) {
	pools := []infrav1.K8sWorkerPool{
		poolWithWindow("large", 7, 48), // .7-.54
		poolWithWindow("small", 55, 8), // .55-.62
	}
	sp := buildAddressSpaces(pools)["192.168.100.0/24"]

	if len(sp.free) != 1 || sp.free[0] != "192.168.100.63 - 192.168.100.254" {
		t.Fatalf("free = %v, want one gap .63-.254", sp.free)
	}
	if sp.suggest != "wNodeOffset 63, nodeCount 10" {
		t.Fatalf("suggest = %q, want the start of the gap with a default-sized window", sp.suggest)
	}
}

// A network with no room left offers nothing rather than a window that cannot
// be used.
func TestAddressSpacesSayThereIsNoRoom(t *testing.T) {
	pools := []infrav1.K8sWorkerPool{
		poolWithWindow("all", 7, 248), // .7-.254, everything usable
	}
	sp := buildAddressSpaces(pools)["192.168.100.0/24"]
	if len(sp.free) != 0 {
		t.Fatalf("free = %v, want none", sp.free)
	}
	if sp.suggest != "" {
		t.Fatalf("suggest = %q, want none", sp.suggest)
	}
}

// A small gap still gets a suggestion, sized to the gap rather than to the
// default: a window that does not fit would be worse than none.
func TestAddressSpacesFitTheSuggestionToASmallGap(t *testing.T) {
	pools := []infrav1.K8sWorkerPool{
		poolWithWindow("a", 7, 240), // .7-.246, so the gap is .247-.254
	}
	sp := buildAddressSpaces(pools)["192.168.100.0/24"]
	if !strings.HasSuffix(sp.suggest, "nodeCount 8") {
		t.Fatalf("suggest = %q, want an eight-address window (the gap .247-.254)", sp.suggest)
	}
}

// Overlapping windows must not produce a negative gap: the union is what counts.
func TestAddressSpacesToleratOverlap(t *testing.T) {
	pools := []infrav1.K8sWorkerPool{
		poolWithWindow("a", 7, 10),  // .7-.16
		poolWithWindow("b", 10, 10), // .10-.19
	}
	sp := buildAddressSpaces(pools)["192.168.100.0/24"]
	if len(sp.free) != 1 || !strings.HasPrefix(sp.free[0], "192.168.100.20 ") {
		t.Fatalf("free = %v, want a single gap starting at .20", sp.free)
	}
}

// One spare address is reported as free but never suggested as a pool.
func TestAddressSpacesDoNotSuggestASingleAddress(t *testing.T) {
	pools := []infrav1.K8sWorkerPool{
		poolWithWindow("a", 7, 247), // .7-.253, leaving only .254
	}
	sp := buildAddressSpaces(pools)["192.168.100.0/24"]
	if len(sp.free) != 1 {
		t.Fatalf("free = %v, want the single address to be reported", sp.free)
	}
	if sp.suggest != "" {
		t.Fatalf("suggest = %q, want none for a one-address gap", sp.suggest)
	}
}
