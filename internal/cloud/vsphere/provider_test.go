package vsphere

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/vmware/govmomi/object"
	"github.com/vmware/govmomi/vim25/types"

	"github.com/k8s-nodescale/vcd-k8s-node-controller/internal/cloud"
)

func testPlacement() cloud.Placement {
	return cloud.Placement{
		Provider: cloud.ProviderVSphere,
		VSphere: &cloud.VSpherePlacement{
			Datacenter: "MSK_Dev_DC",
			Cluster:    "mskdev",
			Datastore:  "99DOR13_DEV_NFS_L11",
			Network:    "Avantage_VL_DEV_MGMT_CNO-330",
			Folder:     "/HUB Infrastructure/dev/k8s-test-lab",
			NICName:    "ens192",
			Gateway:    "10.99.96.65",
			PrefixLen:  26,
			DNSServers: []string{"10.74.64.91"},
		},
	}
}

func TestForBindsThePlacement(t *testing.T) {
	c := New(Config{})
	prov, err := c.For(testPlacement())
	if err != nil {
		t.Fatalf("For: %v", err)
	}
	p := prov.(*Provider)
	if p.placement.Cluster != "mskdev" || p.placement.Datastore != "99DOR13_DEV_NFS_L11" {
		t.Fatalf("placement = %#v, want the pool block", p.placement)
	}

	// The interface name is the one default a pool may leave out; the
	// datacenter is not, because it names an inventory object and there is no
	// process-wide copy to fall back to.
	pl := testPlacement()
	pl.VSphere.NICName = ""
	prov, err = c.For(pl)
	if err != nil {
		t.Fatalf("For: %v", err)
	}
	p = prov.(*Provider)
	if p.placement.NICName != "ens192" {
		t.Fatalf("nic = %q, want ens192", p.placement.NICName)
	}

	pl = testPlacement()
	pl.VSphere.Datacenter = ""
	if _, err := c.For(pl); err == nil || !strings.Contains(err.Error(), "datacenter") {
		t.Fatalf("For accepted a placement without a datacenter: %v", err)
	}
}

func TestForRejectsAnotherPlatform(t *testing.T) {
	c := New(Config{})
	if _, err := c.For(cloud.Placement{Provider: cloud.ProviderVCD}); err == nil {
		t.Fatal("For accepted a vcd placement")
	}
	if _, err := c.For(cloud.Placement{Provider: cloud.ProviderVSphere}); err == nil {
		t.Fatal("For accepted a vsphere placement without spec.vsphere")
	}
}

// A controller that serves only vCD must not fail at startup, so the factory
// is built with an empty URL and says what is missing on the first call.
func TestAnUnconfiguredFactoryFailsOnUse(t *testing.T) {
	prov, err := New(Config{}).For(testPlacement())
	if err != nil {
		t.Fatalf("For: %v", err)
	}
	_, err = prov.ListWorkerVMs(context.Background(), nil)
	if err == nil || !strings.Contains(err.Error(), "VSPHERE_URL") {
		t.Fatalf("err = %v, want a VSPHERE_URL message", err)
	}
}

// The folder root is the tenant boundary. The declared path is normalised, so
// the datacenter prefix and the vm/ prefix do not change the answer.
func TestFolderPathStaysInsideTheRoot(t *testing.T) {
	c := New(Config{FolderRoot: "/HUB Infrastructure/dev/k8s-test-lab"})
	prov, err := c.For(testPlacement())
	if err != nil {
		t.Fatalf("For: %v", err)
	}
	p := prov.(*Provider)

	got, err := p.folderPath()
	if err != nil {
		t.Fatalf("folderPath: %v", err)
	}
	if want := "/MSK_Dev_DC/vm/HUB Infrastructure/dev/k8s-test-lab"; got != want {
		t.Fatalf("folderPath = %q, want %q", got, want)
	}

	p.placement.Folder = "vm/HUB Infrastructure/dev/k8s-test-lab/pool-a"
	if _, err := p.folderPath(); err != nil {
		t.Fatalf("a subfolder of the root was rejected: %v", err)
	}
	p.placement.Folder = "/MSK_Dev_DC/vm/HUB Infrastructure/dev/k8s-test-lab/pool-b"
	if _, err := p.folderPath(); err != nil {
		t.Fatalf("a datacenter-prefixed subfolder was rejected: %v", err)
	}

	p.placement.Folder = "/HUB Infrastructure/dev/other-lab"
	if _, err := p.folderPath(); err == nil || !strings.Contains(err.Error(), "outside the allowed root") {
		t.Fatalf("err = %v, want the root check", err)
	}
}

func TestNormalizeURL(t *testing.T) {
	for raw, want := range map[string]string{
		"97dev-vcp01.mtscloud.local":             "https://97dev-vcp01.mtscloud.local/sdk",
		"https://97dev-vcp01.mtscloud.local":     "https://97dev-vcp01.mtscloud.local/sdk",
		"https://97dev-vcp01.mtscloud.local/":    "https://97dev-vcp01.mtscloud.local/sdk",
		"https://97dev-vcp01.mtscloud.local/sdk": "https://97dev-vcp01.mtscloud.local/sdk",
	} {
		u, err := normalizeURL(raw)
		if err != nil {
			t.Fatalf("normalizeURL(%q): %v", raw, err)
		}
		if u.String() != want {
			t.Fatalf("normalizeURL(%q) = %q, want %q", raw, u.String(), want)
		}
	}
	if _, err := normalizeURL("  "); err == nil {
		t.Fatal("normalizeURL accepted an empty URL")
	}
}

// The annotation is the ownership record. Anything else in that field belongs
// to a human or to Terraform and must not be decoded as ours.
func TestAnnotationRoundTrip(t *testing.T) {
	raw := encodeAnnotation(map[string]string{"managed-by": "vcd-nodescale", "token-id": "abc"}, "10.99.96.74", "noble-template")
	a := decodeAnnotation(raw)
	if a.Metadata["token-id"] != "abc" {
		t.Fatalf("metadata = %#v, want token-id", a.Metadata)
	}
	if a.IP != "10.99.96.74" || a.Template != "noble-template" {
		t.Fatalf("annotation = %#v, want the address and the template", a)
	}
	for _, foreign := range []string{"maintenance window", "{\"apiVersion\":\"other\"}", "{}"} {
		if got := decodeAnnotation(foreign); got.APIVersion != "" {
			t.Fatalf("decodeAnnotation(%q) claimed a foreign annotation: %#v", foreign, got)
		}
	}
}

func TestMetadataDocumentCarriesTheNetwork(t *testing.T) {
	p := cloud.VSpherePlacement{NICName: "ens192", Gateway: "10.99.96.65", PrefixLen: 26, DNSServers: []string{"10.74.64.91"}}
	doc := metadataDocument(p, "pool-1", "10.99.96.74")
	for _, want := range []string{
		"local-hostname: pool-1",
		"set-name: ens192",
		"addresses: [10.99.96.74/26]",
		"via: 10.99.96.65",
		"10.74.64.91",
	} {
		if !strings.Contains(doc, want) {
			t.Fatalf("document does not contain %q:\n%s", want, doc)
		}
	}
	// Without a gateway the document would bring the guest up with no
	// address, so the create refuses instead of writing half of one.
	if got := metadataDocument(cloud.VSpherePlacement{NICName: "ens192"}, "pool-1", "10.99.96.74"); got != "" {
		t.Fatalf("metadataDocument without a gateway = %q, want empty", got)
	}
}

func TestDiskStateMapsBusAndUnit(t *testing.T) {
	unit := func(u int32) *int32 { return &u }
	controllers := []types.BaseVirtualDevice{
		&types.ParaVirtualSCSIController{VirtualSCSIController: types.VirtualSCSIController{
			VirtualController: types.VirtualController{VirtualDevice: types.VirtualDevice{Key: 1000}, BusNumber: 0}}},
		&types.ParaVirtualSCSIController{VirtualSCSIController: types.VirtualSCSIController{
			VirtualController: types.VirtualController{VirtualDevice: types.VirtualDevice{Key: 2000}, BusNumber: 1}}},
	}
	devices := append(controllers,
		&types.VirtualDisk{VirtualDevice: types.VirtualDevice{ControllerKey: 1000, UnitNumber: unit(0)}, CapacityInKB: capacityKB(40)},
		&types.VirtualDisk{VirtualDevice: types.VirtualDevice{ControllerKey: 2000, UnitNumber: unit(0)}, CapacityInKB: capacityKB(10)},
	)
	total, system, data := diskState(devices)
	if total != 50 || system != 40 {
		t.Fatalf("total/system = %d/%d, want 50/40", total, system)
	}
	if len(data) != 1 || data[0].BusNumber != 1 || data[0].UnitNumber != 0 || data[0].SizeGB != 10 {
		t.Fatalf("data disks = %#v, want one 10 GB disk at bus 1/unit 0", data)
	}
}

func TestCreateRefusesAPlacementWithoutTheGuestNetwork(t *testing.T) {
	prov, err := New(Config{URL: "https://vcenter.example"}).For(testPlacement())
	if err != nil {
		t.Fatalf("For: %v", err)
	}
	p := prov.(*Provider)
	p.placement.Gateway = ""
	err = p.validatePlacement()
	if err == nil || !strings.Contains(err.Error(), "gateway") {
		t.Fatalf("err = %v, want a gateway message", err)
	}
}

// capacityInBytes is the field vSphere 8 reads for a new disk; with only the
// deprecated capacityInKB the device is created with 0 bytes.
func TestSetDiskCapacityWritesBothFields(t *testing.T) {
	d := &types.VirtualDisk{}
	setDiskCapacity(d, capacityKB(10))
	if d.CapacityInKB != 10*1024*1024 {
		t.Fatalf("capacityInKB = %d, want %d", d.CapacityInKB, 10*1024*1024)
	}
	if d.CapacityInBytes != 10*1024*1024*1024 {
		t.Fatalf("capacityInBytes = %d, want %d", d.CapacityInBytes, int64(10)*1024*1024*1024)
	}
}

// TestHostMatchesTheTfvarsName: the pool names the host the way the Terraform
// tfvars does (vsphere.esxi_host_name, an FQDN), while vSphere reports an
// inventory path. Both must resolve to the same host, and a host outside the
// cluster must not.
func TestHostMatchesTheTfvarsName(t *testing.T) {
	fqdn := "99r2488-01-dev.mtscloud.local"
	h := &object.HostSystem{Common: object.Common{
		InventoryPath: "/MSK_Dev_DC/host/mskdev/" + fqdn,
	}}
	for _, want := range []string{
		fqdn,
		"99R2488-01-DEV.MTSCLOUD.LOCAL",
		" /MSK_Dev_DC/host/mskdev/" + fqdn + " ",
	} {
		if !hostMatches(h, want) {
			t.Errorf("hostMatches(%q) = false, want true", want)
		}
	}
	for _, want := range []string{
		"",
		"77r2488-01-dev.mtscloud.local",
		"99r2488-01",
		"/MSK_Dev_DC/host/other/99r2488-01-dev.mtscloud.local",
	} {
		if hostMatches(h, want) {
			t.Errorf("hostMatches(%q) = true, want false", want)
		}
	}
	if got := hostLeaf("/MSK_Dev_DC/host/mskdev/" + fqdn); got != fqdn {
		t.Errorf("hostLeaf = %q, want %q", got, fqdn)
	}
	if got := hostLeaf(fqdn); got != fqdn {
		t.Errorf("hostLeaf on a bare name = %q, want %q", got, fqdn)
	}
}

// TestPlacementCarriesTheHost: the host is part of placement, so it reaches the
// clone the same way the cluster and the datastore do.
func TestPlacementCarriesTheHost(t *testing.T) {
	pl := testPlacement()
	pl.VSphere.Host = "99r2488-01-dev.mtscloud.local"
	prov, err := New(Config{URL: "https://vcenter.example"}).For(pl)
	if err != nil {
		t.Fatalf("For: %v", err)
	}
	if got := prov.(*Provider).placement.Host; got != pl.VSphere.Host {
		t.Fatalf("placement host = %q, want %q", got, pl.VSphere.Host)
	}
}

// I-29: a clone that cannot be powered on must not survive the create. The
// controller drops the slot when the create fails, so a VM left behind is
// invisible to status and to the page, and no later pass will ever clean it.
func TestAFailedPowerOnRemovesTheVM(t *testing.T) {
	removed := 0
	err := startOrRemove("nscl-test-wrk-09",
		func() error { return errors.New("power on: no host is compatible") },
		func() error { removed++; return nil })
	if err == nil || !strings.Contains(err.Error(), "no host is compatible") {
		t.Fatalf("err = %v, want the power-on failure", err)
	}
	if removed != 1 {
		t.Fatalf("removed %d times, want the clone removed once", removed)
	}
}

func TestASuccessfulPowerOnKeepsTheVM(t *testing.T) {
	removed := 0
	if err := startOrRemove("nscl-test-wrk-09",
		func() error { return nil },
		func() error { removed++; return nil }); err != nil {
		t.Fatalf("err = %v, want nil", err)
	}
	if removed != 0 {
		t.Fatalf("removed %d times, want the VM kept", removed)
	}
}

// When the cleanup fails too, the error has to name the VM: the slot is already
// gone, so this message is the only thing pointing at the leftover object.
func TestAFailedPowerOnAndCleanupNamesTheVM(t *testing.T) {
	err := startOrRemove("nscl-test-wrk-09",
		func() error { return errors.New("power on: no host is compatible") },
		func() error { return errors.New("destroy: permission denied") })
	if err == nil {
		t.Fatal("err = nil, want both failures")
	}
	for _, want := range []string{"no host is compatible", "permission denied", "nscl-test-wrk-09"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("err = %q, want it to mention %q", err, want)
		}
	}
}
