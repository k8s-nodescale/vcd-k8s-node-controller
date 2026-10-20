package cloud

import (
	"reflect"
	"testing"
)

func TestPlanDiskPlacementsDefaults(t *testing.T) {
	got, err := PlanDiskPlacements([]DiskSpec{
		{SizeGB: 10},
		{SizeGB: 30},
	}, []DiskPlacement{{Index: -1, BusNumber: 0, UnitNumber: 0}})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	want := []DiskPlacement{
		{Index: 0, Spec: DiskSpec{SizeGB: 10}, BusNumber: 1, UnitNumber: 0},
		{Index: 1, Spec: DiskSpec{SizeGB: 30}, BusNumber: 1, UnitNumber: 1},
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("got %#v, want %#v", got, want)
	}
}

func TestPlanDiskPlacementsExplicitIsSorted(t *testing.T) {
	got, err := PlanDiskPlacements([]DiskSpec{
		{SizeGB: 30, BusType: "paravirtual", BusNumber: 1, UnitNumber: 1},
		{SizeGB: 10, BusType: "paravirtual", BusNumber: 1, UnitNumber: 0},
	}, []DiskPlacement{{Index: -1, BusNumber: 0, UnitNumber: 0}})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	if len(got) != 2 || got[0].Index != 1 || got[1].Index != 0 {
		t.Fatalf("unexpected order: %#v", got)
	}
	if got[0].UnitNumber != 0 || got[1].UnitNumber != 1 {
		t.Fatalf("unexpected units: %#v", got)
	}
}

func TestPlanDiskPlacementsSkipsOccupiedUnits(t *testing.T) {
	got, err := PlanDiskPlacements([]DiskSpec{{SizeGB: 30}}, []DiskPlacement{
		{Index: -1, BusNumber: 0, UnitNumber: 0},
		{Index: 0, BusNumber: 1, UnitNumber: 0},
	})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if got[0].UnitNumber != 1 {
		t.Fatalf("got unit %d, want 1", got[0].UnitNumber)
	}
}

func TestPlanDiskPlacementsDefaultsAvoidOSDisk(t *testing.T) {
	got, err := PlanDiskPlacements([]DiskSpec{{SizeGB: 10, BusNumber: 0, UnitNumber: 0}}, []DiskPlacement{
		{Index: -1, BusNumber: 0, UnitNumber: 0},
	})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if got[0].BusNumber != 1 || got[0].UnitNumber != 0 {
		t.Fatalf("got bus %d/unit %d, want bus 1/unit 0", got[0].BusNumber, got[0].UnitNumber)
	}
}

func TestPlanDiskPlacementsRejectsDuplicate(t *testing.T) {
	_, err := PlanDiskPlacements([]DiskSpec{
		{SizeGB: 10, BusNumber: 1, UnitNumber: 0},
		{SizeGB: 30, BusNumber: 1, UnitNumber: 0},
	}, []DiskPlacement{{Index: -1, BusNumber: 0, UnitNumber: 0}})
	if err == nil {
		t.Fatal("expected duplicate placement error")
	}
}

func TestPlanDiskPlacementsRejectsZeroSize(t *testing.T) {
	_, err := PlanDiskPlacements([]DiskSpec{{SizeGB: 0}}, nil)
	if err == nil {
		t.Fatal("expected size validation error")
	}
}
