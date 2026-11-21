package controller

import (
	"strings"
	"testing"

	infrav1 "github.com/k8s-nodescale/vcd-k8s-node-controller/api/v1alpha1"
)

// The window is a per-object rule the controller can check on its own: it is
// usable and fits, or it is not. Overlap with another pool needs the other
// objects and is the admission webhook's job (roadmap 5).
func TestValidateIPWindow(t *testing.T) {
	cases := []struct {
		name    string
		plan    infrav1.IPPlan
		wantErr string
	}{
		{
			name: "a window that fits",
			plan: infrav1.IPPlan{CIDR: "192.168.100.0/24", WNodeOffset: 40, NodeCount: 24},
		},
		{
			name: "the default of ten",
			plan: infrav1.IPPlan{CIDR: "192.168.100.0/24", WNodeOffset: 7, NodeCount: 10},
		},
		{
			name: "a window ending at the last usable address",
			plan: infrav1.IPPlan{CIDR: "192.168.100.0/24", WNodeOffset: 250, NodeCount: 4},
		},
		{
			// The lab tfvars' shape: vm_ip_offset 35 on 10.99.96.64/26.
			name: "the lab plan with vm_ip_offset",
			plan: infrav1.IPPlan{
				CIDR: "10.99.96.64/26", VMIPOffset: 35,
				DVMOffset: 2, KubeAPILB: 3, LBNodeOffset: 5, LBNodeCount: 2,
				MNodeOffset: 7, MasterCount: 3, WNodeOffset: 10, NodeCount: 17,
			},
		},
		{
			name:    "a window pushed past the end by vm_ip_offset",
			plan:    infrav1.IPPlan{CIDR: "192.168.100.0/24", VMIPOffset: 240, WNodeOffset: 7, NodeCount: 10},
			wantErr: "does not fit",
		},
		{
			name:    "a window that starts at the gateway",
			plan:    infrav1.IPPlan{CIDR: "192.168.100.0/24", WNodeOffset: 1, NodeCount: 10},
			wantErr: "gateway",
		},
		{
			name:    "a named offset outside the prefix",
			plan:    infrav1.IPPlan{CIDR: "192.168.100.0/24", WNodeOffset: 7, NodeCount: 10, KubeAPILB: 300},
			wantErr: "outside",
		},
		{
			name:    "a negative vm_ip_offset",
			plan:    infrav1.IPPlan{CIDR: "192.168.100.0/24", VMIPOffset: -1, WNodeOffset: 7, NodeCount: 10},
			wantErr: "must not be negative",
		},
		{
			name:    "a window that runs past the last usable address",
			plan:    infrav1.IPPlan{CIDR: "192.168.100.0/24", WNodeOffset: 250, NodeCount: 6},
			wantErr: "does not fit",
		},
		{
			name:    "a zero count",
			plan:    infrav1.IPPlan{CIDR: "192.168.100.0/24", NodeCount: 0},
			wantErr: "at least 1",
		},
		{
			name:    "a negative count",
			plan:    infrav1.IPPlan{CIDR: "192.168.100.0/24", NodeCount: -1},
			wantErr: "at least 1",
		},
		{
			name:    "an offset outside the CIDR",
			plan:    infrav1.IPPlan{CIDR: "192.168.100.0/24", WNodeOffset: 300, NodeCount: 10},
			wantErr: "outside",
		},
		{
			name:    "not IPv4",
			plan:    infrav1.IPPlan{CIDR: "2001:db8::/64", NodeCount: 10},
			wantErr: "IPv4",
		},
		{
			name:    "not a CIDR",
			plan:    infrav1.IPPlan{CIDR: "192.168.100.7", NodeCount: 10},
			wantErr: "CIDR",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := validateIPWindow(tc.plan)
			if tc.wantErr == "" {
				if got != "" {
					t.Fatalf("rejected a valid plan: %s", got)
				}
				return
			}
			if !strings.Contains(got, tc.wantErr) {
				t.Fatalf("got %q, want it to mention %q", got, tc.wantErr)
			}
		})
	}
}
