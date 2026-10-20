package controller

import (
	"reflect"
	"testing"

	infrav1 "github.com/werton13/vcd-k8s-node-controller/api/v1alpha1"
)

func TestToCloudInitDisksOrdersByBusUnit(t *testing.T) {
	got, err := toCloudInitDisks([]infrav1.DiskSpec{
		{SizeGB: 30, Mount: "/var/lib", BusNumber: 1, UnitNumber: 1},
		{SizeGB: 10, Mount: "/var", BusNumber: 1, UnitNumber: 0},
	})
	if err != nil {
		t.Fatalf("toCloudInitDisks: %v", err)
	}
	if len(got) != 2 {
		t.Fatalf("got %d disks, want 2", len(got))
	}
	if got[0].Device != "/dev/sdb" || got[0].Mount != "/var" || got[0].VG != "data1_vg" {
		t.Fatalf("unexpected first disk: %#v", got[0])
	}
	if got[1].Device != "/dev/sdc" || got[1].Mount != "/var/lib" || got[1].VG != "data2_vg" {
		t.Fatalf("unexpected second disk: %#v", got[1])
	}
	if !reflect.DeepEqual(got[0].RsyncExcludes, []string{"/lib"}) {
		t.Fatalf("unexpected excludes for /var: %#v", got[0].RsyncExcludes)
	}
	if len(got[1].RsyncExcludes) != 0 {
		t.Fatalf("unexpected excludes for /var/lib: %#v", got[1].RsyncExcludes)
	}
}

func TestToCloudInitDisksDefaultsBusOne(t *testing.T) {
	got, err := toCloudInitDisks([]infrav1.DiskSpec{
		{SizeGB: 10, Mount: "/var"},
		{SizeGB: 30, Mount: "/var/lib"},
	})
	if err != nil {
		t.Fatalf("toCloudInitDisks: %v", err)
	}
	if got[0].Device != "/dev/sdb" || got[1].Device != "/dev/sdc" {
		t.Fatalf("unexpected devices: %#v", []string{got[0].Device, got[1].Device})
	}
}
