package cloud

import (
	"fmt"
	"sort"
)

const (
	// DefaultDataBusNumber is the SCSI controller used for data disks. The
	// template OS disk lives on bus 0/unit 0 (usually /dev/sda), so data disks
	// start on bus 1/unit 0 (usually /dev/sdb, /dev/sdc, ...).
	DefaultDataBusNumber = 1

	// maxSCSIUnit is the highest SCSI unit number supported by the vCD API.
	maxSCSIUnit = 15
)

// DiskPlacement is a DiskSpec with an explicit controller/unit assignment.
// Index is the position in the original DiskSpec slice (-1 means "occupied by
// something else", e.g. the template OS disk).
type DiskPlacement struct {
	Index      int
	Spec       DiskSpec
	BusNumber  int
	UnitNumber int
}

// PlanDiskPlacements turns the user-provided disk list into a deterministic
// controller/unit layout.
//
// Rules:
//   - An explicit bus/unit (either value non-zero) is preserved.
//   - Otherwise the disk is placed on DefaultDataBusNumber, using the first
//     free unit in ascending order.
//   - (0,0) is reserved for the template OS disk.
//   - Duplicate (bus, unit) pairs are rejected.
//
// The result is sorted by (bus, unit). That order is also the order used for
// AddInternalDisk calls, so a single SCSI controller enumerates the data disks
// as /dev/sdb, /dev/sdc, ... in the same order.
func PlanDiskPlacements(specs []DiskSpec, occupied []DiskPlacement) ([]DiskPlacement, error) {
	used := make(map[[2]int]bool, len(occupied)+len(specs))
	owner := make(map[[2]int]int, len(occupied)+len(specs))
	for _, p := range occupied {
		key := [2]int{p.BusNumber, p.UnitNumber}
		used[key] = true
		owner[key] = p.Index
	}

	out := make([]DiskPlacement, 0, len(specs))
	nextUnit := 0
	for i, spec := range specs {
		if spec.SizeGB <= 0 {
			return nil, fmt.Errorf("disk %d: sizeGB must be greater than zero", i)
		}

		bus, unit := int(spec.BusNumber), int(spec.UnitNumber)
		explicit := spec.BusNumber != 0 || spec.UnitNumber != 0
		if explicit {
			if bus < 0 || unit < 0 {
				return nil, fmt.Errorf("disk %d: busNumber and unitNumber must be >= 0", i)
			}
			if bus == 0 && unit == 0 {
				return nil, fmt.Errorf("disk %d: bus 0/unit 0 is reserved for the OS disk", i)
			}
			if unit > maxSCSIUnit {
				return nil, fmt.Errorf("disk %d: unitNumber %d exceeds the SCSI maximum of %d", i, unit, maxSCSIUnit)
			}
		} else {
			bus = DefaultDataBusNumber
			for used[[2]int{bus, nextUnit}] {
				nextUnit++
			}
			if nextUnit > maxSCSIUnit {
				return nil, fmt.Errorf("disk %d: no free unit left on bus %d", i, bus)
			}
			unit = nextUnit
			nextUnit++
		}

		key := [2]int{bus, unit}
		if used[key] {
			who := owner[key]
			if who < 0 {
				return nil, fmt.Errorf("disk %d: bus %d/unit %d is already used by the OS disk", i, bus, unit)
			}
			return nil, fmt.Errorf("disk %d: bus %d/unit %d is already used by disk %d", i, bus, unit, who)
		}
		used[key] = true
		owner[key] = i

		out = append(out, DiskPlacement{
			Index:      i,
			Spec:       spec,
			BusNumber:  bus,
			UnitNumber: unit,
		})
	}

	sort.SliceStable(out, func(i, j int) bool {
		if out[i].BusNumber != out[j].BusNumber {
			return out[i].BusNumber < out[j].BusNumber
		}
		return out[i].UnitNumber < out[j].UnitNumber
	})

	return out, nil
}
