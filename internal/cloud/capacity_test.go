package cloud

import (
	"strings"
	"testing"
)

func TestPlatformCapacityFits(t *testing.T) {
	base := PlatformCapacity{
		Model:         "AllocationPool",
		CPUMHzPerCore: 2300,
		CPU:           ResourceCapacity{Limit: 56000, Used: 46000},
		MemoryMB:      ResourceCapacity{Limit: 131072, Used: 122880},
		Storage: []StorageProfileCapacity{
			{Name: "SAS", Default: true, LimitMB: 1000 * 1024, UsedMB: 990 * 1024},
		},
	}
	fits := func(req CapacityRequest, mhz int64) (bool, string) {
		ok, short := base.Fits(req, mhz)
		return ok, strings.Join(short, "; ")
	}

	if ok, why := fits(CapacityRequest{CPUs: 2, MemoryMB: 4096, StorageMB: map[string]int64{"": 10 * 1024}}, 2300); !ok {
		t.Fatalf("expected the request to fit, got %q", why)
	}
	if ok, why := fits(CapacityRequest{CPUs: 5}, 2300); ok || !strings.Contains(why, "CPU") {
		t.Fatalf("expected a CPU shortfall, got ok=%v why=%q", ok, why)
	}
	if ok, why := fits(CapacityRequest{CPUs: 1, MemoryMB: 8193}, 2300); ok || !strings.Contains(why, "memory") {
		t.Fatalf("expected a memory shortfall, got ok=%v why=%q", ok, why)
	}
	if ok, why := fits(CapacityRequest{CPUs: 1, StorageMB: map[string]int64{"": 11 * 1024}}, 2300); ok || !strings.Contains(why, "storage profile SAS") {
		t.Fatalf("expected a storage shortfall, got ok=%v why=%q", ok, why)
	}
	// A profile the vDC does not report is the create's business, not the
	// pre-flight's.
	if ok, why := fits(CapacityRequest{CPUs: 1, StorageMB: map[string]int64{"unknown": 1 << 30}}, 2300); !ok {
		t.Fatalf("an unknown storage profile must not block, got %q", why)
	}
	// Without the vDC's CPU speed the conversion would be a guess.
	if ok, why := fits(CapacityRequest{CPUs: 1000}, 0); !ok {
		t.Fatalf("the CPU check must be skipped without a speed, got %q", why)
	}

	ok, short := base.Fits(CapacityRequest{CPUs: 100, MemoryMB: 1 << 20, StorageMB: map[string]int64{"": 1 << 30}}, 2300)
	if ok || len(short) != 3 {
		t.Fatalf("expected three shortfalls in one report, got ok=%v short=%v", ok, short)
	}
	if !strings.HasPrefix(short[0], "CPU") || !strings.HasPrefix(short[1], "memory") || !strings.HasPrefix(short[2], "storage profile") {
		t.Fatalf("shortfalls are not reported in a stable order: %v", short)
	}
}

func TestPlatformCapacityFitsUnbounded(t *testing.T) {
	// Pay-as-you-go and flex report Limit 0: nothing to refuse.
	var c PlatformCapacity
	if ok, why := c.Fits(CapacityRequest{CPUs: 64, MemoryMB: 1 << 20, StorageMB: map[string]int64{"": 1 << 30}}, 2300); !ok {
		t.Fatalf("an uncapped vDC must accept anything, got %q", why)
	}
}

func TestPlatformCapacityStorageProfile(t *testing.T) {
	c := PlatformCapacity{Storage: []StorageProfileCapacity{
		{Name: "SAS", Default: true, LimitMB: 100},
		{Name: "SSD", LimitMB: 200},
	}}
	if sp, ok := c.StorageProfile(""); !ok || sp.Name != "SAS" {
		t.Fatalf("empty name must resolve to the default profile: %#v %v", sp, ok)
	}
	if sp, ok := c.StorageProfile("ssd"); !ok || sp.Name != "SSD" {
		t.Fatalf("profile names are case-insensitive: %#v %v", sp, ok)
	}
	if _, ok := c.StorageProfile("nope"); ok {
		t.Fatal("an unknown profile must not resolve")
	}
	single := PlatformCapacity{Storage: []StorageProfileCapacity{{Name: "ONLY", LimitMB: 1}}}
	if sp, ok := single.StorageProfile(""); !ok || sp.Name != "ONLY" {
		t.Fatalf("a single profile is the default even without the flag: %#v %v", sp, ok)
	}
}
