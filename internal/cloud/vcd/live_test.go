package vcd

import (
	"context"
	"os"
	"testing"
	"time"
)

// TestLiveListWorkerVMs is a read-only smoke test against a real tenant. It is
// skipped unless VCD_URL is set.
//
//	VCD_URL=https://.../api VCD_USER=... VCD_PASSWORD=... VCD_ORG=... \
//	VCD_VDC=... VCD_VAPP=... [VCD_INSECURE=true] \
//	go test -run TestLiveListWorkerVMs -v ./internal/cloud/vcd
func TestLiveListWorkerVMs(t *testing.T) {
	if os.Getenv("VCD_URL") == "" {
		t.Skip("VCD_URL is not set")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()

	p, err := New(Config{
		URL:      os.Getenv("VCD_URL"),
		User:     os.Getenv("VCD_USER"),
		Password: os.Getenv("VCD_PASSWORD"),
		Org:      os.Getenv("VCD_ORG"),
		VDC:      os.Getenv("VCD_VDC"),
		Insecure: os.Getenv("VCD_INSECURE") == "true",
	})
	if err != nil {
		t.Fatalf("connect: %v", err)
	}

	vapp := os.Getenv("VCD_VAPP")
	if vapp == "" {
		t.Fatal("VCD_VAPP is not set")
	}

	// Empty selector lists every VM, exercising metadata, primary IP parsing
	// and resource extraction against real data.
	vms, err := p.ListWorkerVMs(ctx, vapp, nil)
	if err != nil {
		t.Fatalf("ListWorkerVMs(%q): %v", vapp, err)
	}
	t.Logf("vApp %q has %d VMs", vapp, len(vms))
	for _, vm := range vms {
		t.Logf("  %-32s ip=%-15s power=%-12s cpu=%d memMB=%d diskGB=%d",
			vm.Name, vm.IP, vm.PowerState, vm.CPU, vm.MemoryMB, vm.DiskGB)
	}
}
