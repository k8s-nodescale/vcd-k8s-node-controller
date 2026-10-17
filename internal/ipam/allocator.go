// Package ipam implements a persistent, ip_plan-aware IP allocator.
package ipam

import (
	"encoding/binary"
	"fmt"
	"net/netip"
	"sort"
)

// Plan mirrors Terraform's ip_plan variable.
type Plan struct {
	CIDR        string
	MNodeOffset int
	WNodeOffset int
	DVMOffset   int
	KubeAPILB   int
	IngressLB   int
	MasterCount int32
	Reserved    []string
}

// Ledger is the persisted allocation state.
type Ledger struct {
	NextIndex int
	Used      []string
	Free      []string
}

// Allocator hands out IPv4 addresses from a CIDR while never returning
// infrastructure addresses (gateway, VIPs, masters, DVM, explicit reservations).
type Allocator struct {
	plan     Plan
	prefix   netip.Prefix
	total    uint64
	reserved map[netip.Addr]struct{}
	used     map[netip.Addr]struct{}
	ledger   Ledger
}

// New builds an allocator from the plan and the last persisted ledger.
func New(plan Plan, ledger Ledger) (*Allocator, error) {
	p, err := netip.ParsePrefix(plan.CIDR)
	if err != nil {
		return nil, fmt.Errorf("parse cidr %q: %w", plan.CIDR, err)
	}
	if !p.Addr().Is4() {
		return nil, fmt.Errorf("only IPv4 CIDRs are supported, got %q", plan.CIDR)
	}
	p = p.Masked()

	a := &Allocator{
		plan:     plan,
		prefix:   p,
		total:    uint64(1) << uint(32-p.Bits()),
		reserved: map[netip.Addr]struct{}{},
		used:     map[netip.Addr]struct{}{},
		ledger: Ledger{
			NextIndex: ledger.NextIndex,
			Used:      append([]string(nil), ledger.Used...),
			Free:      append([]string(nil), ledger.Free...),
		},
	}
	a.buildReserved()
	a.buildUsed()
	return a, nil
}

func (a *Allocator) buildReserved() {
	a.reserveOffset(1) // default gateway
	if a.plan.KubeAPILB > 0 {
		a.reserveOffset(a.plan.KubeAPILB)
	}
	if a.plan.IngressLB > 0 {
		a.reserveOffset(a.plan.IngressLB)
	}
	if a.plan.DVMOffset != 0 {
		a.reserveOffset(a.plan.DVMOffset)
	}
	if a.plan.MasterCount > 0 && a.plan.MNodeOffset > 0 {
		for i := int32(0); i < a.plan.MasterCount; i++ {
			a.reserveOffset(a.plan.MNodeOffset + int(i))
		}
	}
	for _, s := range a.plan.Reserved {
		if addr, err := netip.ParseAddr(s); err == nil {
			a.reserved[addr] = struct{}{}
		}
	}
}

// reserveOffset reserves an address by offset. Negative offsets count from the
// end of the prefix (matching Terraform's cidrhost negative hostnum).
func (a *Allocator) reserveOffset(off int) {
	var u uint64
	if off < 0 {
		if uint64(-off) >= a.total {
			return
		}
		u = a.total - uint64(-off)
	} else {
		u = uint64(off)
	}
	if u >= a.total {
		return
	}
	a.reserved[a.addrAt(u)] = struct{}{}
}

func (a *Allocator) buildUsed() {
	for _, s := range a.ledger.Used {
		if addr, err := netip.ParseAddr(s); err == nil {
			a.used[addr] = struct{}{}
		}
	}
}

func (a *Allocator) addrAt(offset uint64) netip.Addr {
	b := a.prefix.Masked().Addr().As4()
	v := binary.BigEndian.Uint32(b[:]) + uint32(offset)
	var out [4]byte
	binary.BigEndian.PutUint32(out[:], v)
	return netip.AddrFrom4(out)
}

func (a *Allocator) available(addr netip.Addr) bool {
	if _, ok := a.reserved[addr]; ok {
		return false
	}
	if _, ok := a.used[addr]; ok {
		return false
	}
	if addr == a.prefix.Masked().Addr() {
		return false // network address
	}
	if a.total > 0 && addr == a.addrAt(a.total-1) {
		return false // broadcast address
	}
	return true
}

// Allocate returns the next free IP, reusing freed addresses first.
func (a *Allocator) Allocate() (string, error) {
	for len(a.ledger.Free) > 0 {
		s := a.ledger.Free[0]
		a.ledger.Free = a.ledger.Free[1:]
		addr, err := netip.ParseAddr(s)
		if err != nil {
			continue
		}
		if a.available(addr) {
			a.used[addr] = struct{}{}
			a.ledger.Used = append(a.ledger.Used, addr.String())
			return addr.String(), nil
		}
	}

	start := a.plan.WNodeOffset
	if start < 0 {
		start = 0
	}
	for off := uint64(start); off < a.total; off++ {
		addr := a.addrAt(off)
		if !a.available(addr) {
			continue
		}
		a.used[addr] = struct{}{}
		a.ledger.Used = append(a.ledger.Used, addr.String())
		if idx := int(off) - a.plan.WNodeOffset + 1; idx > a.ledger.NextIndex {
			a.ledger.NextIndex = idx
		}
		return addr.String(), nil
	}
	return "", fmt.Errorf("no free IPs in %s", a.plan.CIDR)
}

// Free releases an allocated IP and makes it eligible for reuse.
func (a *Allocator) Free(ip string) error {
	addr, err := netip.ParseAddr(ip)
	if err != nil {
		return fmt.Errorf("parse ip %q: %w", ip, err)
	}
	if _, ok := a.used[addr]; !ok {
		return fmt.Errorf("%s is not currently allocated", ip)
	}
	delete(a.used, addr)

	kept := a.ledger.Used[:0]
	for _, s := range a.ledger.Used {
		if s != addr.String() {
			kept = append(kept, s)
		}
	}
	a.ledger.Used = kept
	a.ledger.Free = append(a.ledger.Free, addr.String())
	sort.Strings(a.ledger.Free)
	return nil
}

// Ledger returns a copy of the current ledger for persistence.
func (a *Allocator) Ledger() Ledger {
	return Ledger{
		NextIndex: a.ledger.NextIndex,
		Used:      append([]string(nil), a.ledger.Used...),
		Free:      append([]string(nil), a.ledger.Free...),
	}
}
