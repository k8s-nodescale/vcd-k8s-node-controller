package ipam

import (
	"strings"
	"testing"
)

// The lab acceptance case: Terraform's vSphere tfvars write
//
//	ip_plan = {
//	  dvm = 2, kubeapi_lb = 3, lb_node = 5, m_node = 7, w_node = 10,
//	  vm_ip_offset = 35
//	}
//
// on 10.99.96.64/26, and the cluster already uses .101 (dvm), .102 (API VIP),
// .104/.105 (LB pair), .106-.108 (masters), .109-.111 (workers). The controller
// has to hand out exactly those numbers, and keep the gateway .65 - which is not
// shifted - out of the pool.
func TestVMIPOffsetMovesTheWholePlan(t *testing.T) {
	plan := Plan{
		CIDR:         "10.99.96.64/26",
		VMIPOffset:   35,
		DVMOffset:    2,
		KubeAPILB:    3,
		LBNodeOffset: 5,
		LBNodeCount:  2,
		MNodeOffset:  7,
		MasterCount:  3,
		WNodeOffset:  10,
		NodeCount:    17,
	}
	a, err := New(plan, Ledger{})
	if err != nil {
		t.Fatalf("the lab plan was refused: %v", err)
	}
	for _, want := range []string{"10.99.96.109", "10.99.96.110", "10.99.96.111"} {
		got, err := a.Allocate()
		if err != nil {
			t.Fatalf("allocate: %v", err)
		}
		if got != want {
			t.Fatalf("got %s, want %s", got, want)
		}
	}
	if ord, err := a.Ordinal("10.99.96.109"); err != nil || ord != 1 {
		t.Fatalf("ordinal of .109 = %d, %v; want 1", ord, err)
	}
	if ord, err := a.Ordinal("10.99.96.111"); err != nil || ord != 3 {
		t.Fatalf("ordinal of .111 = %d, %v; want 3", ord, err)
	}

	for _, ip := range []string{
		"10.99.96.65",  // gateway, not shifted by vm_ip_offset
		"10.99.96.101", // dvm (35+2)
		"10.99.96.102", // kubeapi LB (35+3)
		"10.99.96.104", // first lb node (35+5)
		"10.99.96.105", // second lb node
		"10.99.96.106", // master 0 (35+7)
		"10.99.96.107", // master 1
		"10.99.96.108", // master 2
	} {
		if err := a.Reserve(ip); err == nil || !strings.Contains(err.Error(), "reserved") {
			t.Errorf("%s is not reserved: Reserve returned %v", ip, err)
		}
	}
}

// The shift is applied once: a ledger address is an address, so a plan that
// changes only vm_ip_offset does not move a worker that is already allocated.
func TestVMIPOffsetDoesNotDoubleApply(t *testing.T) {
	plan := basePlan()
	plan.VMIPOffset = 10
	a, err := New(plan, Ledger{Used: []string{"192.168.100.17"}})
	if err != nil {
		t.Fatal(err)
	}
	if got, err := a.Ordinal("192.168.100.17"); err != nil || got != 1 {
		t.Fatalf("ordinal of the first worker = %d, %v; want 1", got, err)
	}
	if err := a.Adopt("192.168.100.17"); err != nil {
		t.Fatalf("a worker at the shifted window start was not adopted: %v", err)
	}
	got, err := a.Allocate()
	if err != nil {
		t.Fatal(err)
	}
	if got != "192.168.100.18" {
		t.Fatalf("got %s, want 192.168.100.18: the shift was applied twice or not at all", got)
	}
}

// A negative offset counts from the end of the subnet, so vm_ip_offset must not
// move it: "-3" stays three addresses below the broadcast whatever the range
// start is.
func TestNegativeOffsetsAreNotShifted(t *testing.T) {
	plan := basePlan()
	plan.VMIPOffset = 100
	plan.NodeCount = 10
	plan.DVMOffset = -3
	a, err := New(plan, Ledger{})
	if err != nil {
		t.Fatal(err)
	}
	if err := a.Reserve("192.168.100.253"); err == nil {
		t.Fatal("a negative dvmOffset was shifted instead of counting from the end")
	}
}

func TestVMIPOffsetIsValidated(t *testing.T) {
	cases := []struct {
		name    string
		mutate  func(*Plan)
		wantErr string
	}{
		{
			name:    "a negative vm_ip_offset",
			mutate:  func(p *Plan) { p.VMIPOffset = -1 },
			wantErr: "must not be negative",
		},
		{
			name:    "a window pushed past the last usable address",
			mutate:  func(p *Plan) { p.VMIPOffset = 240 },
			wantErr: "does not fit",
		},
		{
			name: "a window that starts at the gateway",
			mutate: func(p *Plan) {
				p.VMIPOffset = 0
				p.WNodeOffset = 0
			},
			wantErr: "gateway",
		},
		{
			name: "a named offset outside the prefix",
			mutate: func(p *Plan) {
				p.VMIPOffset = 100
				p.NodeCount = 10
				p.KubeAPILB = 200
			},
			wantErr: "kubeapiLB",
		},
		{
			name: "a master range outside the prefix",
			mutate: func(p *Plan) {
				p.VMIPOffset = 10
				p.MNodeOffset = 245
			},
			wantErr: "mNodeOffset",
		},
		{
			name: "an lb range outside the prefix",
			mutate: func(p *Plan) {
				p.VMIPOffset = 10
				p.LBNodeOffset = 240
				p.LBNodeCount = 6
			},
			wantErr: "lbNodeOffset",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			plan := basePlan()
			tc.mutate(&plan)
			_, err := New(plan, Ledger{})
			if err == nil {
				t.Fatalf("the plan was accepted")
			}
			if !strings.Contains(err.Error(), tc.wantErr) {
				t.Fatalf("got %q, want it to mention %q", err, tc.wantErr)
			}
		})
	}
}

// An offset that lands exactly on the last usable address still fits; one
// address further is the broadcast and is refused.
func TestShiftedWindowMayEndAtTheLastUsableAddress(t *testing.T) {
	plan := basePlan()
	plan.VMIPOffset = 240
	plan.WNodeOffset = 10 // .250
	plan.NodeCount = 4    // .250-.253
	if _, err := New(plan, Ledger{}); err != nil {
		t.Fatalf("a shifted window ending at the last usable address was refused: %v", err)
	}
	plan.NodeCount = 6 // .250-.255, the broadcast is .255
	if _, err := New(plan, Ledger{}); err == nil {
		t.Fatal("a shifted window that claims the broadcast address was accepted")
	}
}

// Validate is what the controller's InvalidSpec condition and the page use; it
// must agree with New.
func TestValidateMatchesNew(t *testing.T) {
	good := Plan{
		CIDR: "10.99.96.64/26", VMIPOffset: 35, DVMOffset: 2, KubeAPILB: 3,
		LBNodeOffset: 5, LBNodeCount: 2, MNodeOffset: 7, MasterCount: 3,
		WNodeOffset: 10, NodeCount: 17,
	}
	if err := Validate(good); err != nil {
		t.Fatalf("Validate rejected the lab plan: %v", err)
	}
	bad := good
	bad.NodeCount = 19 // .109-.127, and .127 is the broadcast address
	if err := Validate(bad); err == nil {
		t.Fatal("Validate accepted a window past the last usable address")
	}
}
