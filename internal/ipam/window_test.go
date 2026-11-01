package ipam

import "testing"

// Two pools share one CIDR by stating the window each owns: allocation stops at
// the window's end instead of borrowing the neighbour's addresses.
func TestWindowBoundsAllocation(t *testing.T) {
	plan := basePlan()
	plan.WNodeOffset = 40
	plan.NodeCount = 2
	a, err := New(plan, Ledger{})
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{"192.168.100.40", "192.168.100.41"} {
		got, err := a.Allocate()
		if err != nil {
			t.Fatalf("allocate: %v", err)
		}
		if got != want {
			t.Fatalf("got %s, want %s", got, want)
		}
	}
	if _, err := a.Allocate(); err == nil {
		t.Fatal("a third allocation succeeded: the window is not a boundary")
	}
}

// A window that starts inside the CIDR must not reach back before it, even when
// the previous ledger still holds a freed address there.
func TestWindowIgnoresFreeListOutsideIt(t *testing.T) {
	plan := basePlan()
	plan.WNodeOffset = 40
	plan.NodeCount = 2
	a, err := New(plan, Ledger{Free: []string{"192.168.100.7"}})
	if err != nil {
		t.Fatal(err)
	}
	got, err := a.Allocate()
	if err != nil {
		t.Fatal(err)
	}
	if got != "192.168.100.40" {
		t.Fatalf("got %s, want 192.168.100.40: a freed address outside the window was handed out", got)
	}
}

// There is no unbounded mode: a zero (or negative) count is refused rather than
// read as "the whole network".
func TestZeroAndNegativeWindowsAreRejected(t *testing.T) {
	for _, n := range []int32{0, -1} {
		plan := basePlan()
		plan.WNodeOffset = 40
		plan.NodeCount = n
		if _, err := New(plan, Ledger{}); err == nil {
			t.Errorf("nodeCount %d was accepted", n)
		}
	}
}

func TestWindowMustFitTheCIDR(t *testing.T) {
	plan := basePlan()
	plan.WNodeOffset = 250
	plan.NodeCount = 10
	if _, err := New(plan, Ledger{}); err == nil {
		t.Fatal("a window that runs past the last usable address was accepted")
	}
}

// The last usable offset is total-2: the broadcast address is never allocatable,
// so a window may not claim it.
func TestWindowMayEndAtTheLastUsableAddress(t *testing.T) {
	plan := basePlan()
	plan.WNodeOffset = 250
	plan.NodeCount = 4 // .250 .. .253, broadcast is .255
	if _, err := New(plan, Ledger{}); err != nil {
		t.Fatalf("a window ending at the last usable address was refused: %v", err)
	}
}
