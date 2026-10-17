package ipam

import "testing"

func basePlan() Plan {
	return Plan{
		CIDR:        "192.168.100.0/24",
		MNodeOffset: 4,
		WNodeOffset: 7,
		DVMOffset:   -3,
		KubeAPILB:   2,
		IngressLB:   3,
		MasterCount: 3,
		Reserved:    []string{"192.168.100.100"},
	}
}

func TestAllocateSkipsReservedAndReusesFreed(t *testing.T) {
	a, err := New(basePlan(), Ledger{})
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{"192.168.100.7", "192.168.100.8", "192.168.100.9"} {
		got, err := a.Allocate()
		if err != nil {
			t.Fatalf("allocate: %v", err)
		}
		if got != want {
			t.Fatalf("got %s, want %s", got, want)
		}
	}
	if err := a.Free("192.168.100.8"); err != nil {
		t.Fatal(err)
	}
	got, err := a.Allocate()
	if err != nil {
		t.Fatal(err)
	}
	if got != "192.168.100.8" {
		t.Fatalf("expected reuse of 192.168.100.8, got %s", got)
	}
}

func TestAllocateNeverReturnsReserved(t *testing.T) {
	a, err := New(basePlan(), Ledger{})
	if err != nil {
		t.Fatal(err)
	}
	reserved := map[string]bool{
		"192.168.100.1":   true, // gateway
		"192.168.100.2":   true, // kubeapi LB
		"192.168.100.3":   true, // ingress LB
		"192.168.100.4":   true, // master 0
		"192.168.100.5":   true, // master 1
		"192.168.100.6":   true, // master 2
		"192.168.100.100": true, // explicit reservation
		"192.168.100.253": true, // dvm (-3)
		"192.168.100.255": true, // broadcast
		"192.168.100.0":   true, // network
	}
	for i := 0; i < 300; i++ {
		ip, err := a.Allocate()
		if err != nil {
			break // exhausted
		}
		if reserved[ip] {
			t.Fatalf("allocated reserved address %s", ip)
		}
	}
}

func TestLedgerPersistence(t *testing.T) {
	first, err := New(basePlan(), Ledger{})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := first.Allocate(); err != nil {
		t.Fatal(err)
	}
	if _, err := first.Allocate(); err != nil {
		t.Fatal(err)
	}
	ledger := first.Ledger()

	second, err := New(basePlan(), ledger)
	if err != nil {
		t.Fatal(err)
	}
	got, err := second.Allocate()
	if err != nil {
		t.Fatal(err)
	}
	if got != "192.168.100.9" {
		t.Fatalf("expected 192.168.100.9 after restoring ledger, got %s", got)
	}
}
