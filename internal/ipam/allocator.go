// Package ipam implements a persistent, ip_plan-aware IP allocator.
package ipam

import (
	"encoding/binary"
	"fmt"
	"net/netip"
	"sort"
)

// Plan mirrors Terraform's ip_plan variable. Offsets are written relative to
// the start of the VM range, and VMIPOffset says where that range begins inside
// the prefix: address = prefix + VMIPOffset + offset. New applies the shift
// once, so every offset the allocator stores and reports afterwards is absolute
// inside the prefix.
type Plan struct {
	CIDR         string
	VMIPOffset   int
	MNodeOffset  int
	WNodeOffset  int
	DVMOffset    int
	KubeAPILB    int
	IngressLB    int
	LBNodeOffset int
	LBNodeCount  int32
	MasterCount  int32
	// NodeCount is the size of the worker window: WNodeOffset .. WNodeOffset+
	// NodeCount-1, both already shifted. It is never zero - the CRD defaults it
	// to 10 and validates a minimum of 1, so "unset" cannot mean "the whole
	// network".
	NodeCount int32
	Reserved  []string
}

// Shift applies vm_ip_offset to a named offset. Negative offsets count from the
// end of the prefix (Terraform's cidrhost) and are never shifted: "-3" is the
// third address from the end of the subnet, whatever vm_ip_offset says.
//
// It is exported because the controller's pre-flight and the management page
// render the window from spec.ipPlan without building an allocator.
func Shift(off, vmIPOffset int) int {
	if off < 0 {
		return off
	}
	return off + vmIPOffset
}

// shiftPlan returns the plan with every named offset made absolute. VMIPOffset
// is zeroed afterwards: the shift is applied exactly once, at construction.
func shiftPlan(plan Plan) Plan {
	plan.MNodeOffset = Shift(plan.MNodeOffset, plan.VMIPOffset)
	plan.WNodeOffset = Shift(plan.WNodeOffset, plan.VMIPOffset)
	plan.DVMOffset = Shift(plan.DVMOffset, plan.VMIPOffset)
	plan.KubeAPILB = Shift(plan.KubeAPILB, plan.VMIPOffset)
	plan.IngressLB = Shift(plan.IngressLB, plan.VMIPOffset)
	plan.LBNodeOffset = Shift(plan.LBNodeOffset, plan.VMIPOffset)
	plan.VMIPOffset = 0
	return plan
}

// validatePlan refuses a plan whose window or named offsets cannot be honoured.
// A wrong offset is expensive - it either silently protects the wrong address
// or hands out an address the cluster already uses - so nothing here is
// clamped: the plan is refused and the operator sees which number is wrong.
func validatePlan(plan Plan, total uint64) error {
	if plan.NodeCount < 1 {
		return fmt.Errorf("nodeCount %d must be at least 1: a pool without a window would take the whole network", plan.NodeCount)
	}
	if plan.VMIPOffset < 0 {
		return fmt.Errorf("vmIPOffset %d must not be negative: it moves the VM range into the prefix", plan.VMIPOffset)
	}
	// The last usable offset is the one before the broadcast address.
	last := int64(total) - 2
	if last < 1 {
		return fmt.Errorf("cidr %s is too small for a pool", plan.CIDR)
	}
	start := int64(Shift(plan.WNodeOffset, plan.VMIPOffset))
	switch {
	case start <= 1:
		return fmt.Errorf("wNodeOffset %d with vmIPOffset %d starts at %d, at or below the gateway of %s",
			plan.WNodeOffset, plan.VMIPOffset, start, plan.CIDR)
	case start > last:
		return fmt.Errorf("wNodeOffset %d with vmIPOffset %d is outside %s",
			plan.WNodeOffset, plan.VMIPOffset, plan.CIDR)
	case start+int64(plan.NodeCount)-1 > last:
		return fmt.Errorf("nodeCount %d from offset %d (wNodeOffset %d, vmIPOffset %d) does not fit %s: the window would end past the last usable address (%d available)",
			plan.NodeCount, start, plan.WNodeOffset, plan.VMIPOffset, plan.CIDR, last-start+1)
	}
	// Infrastructure offsets are reserved, so one that falls outside the prefix
	// would be a silent no-op and the address would be handed to a worker.
	named := []struct {
		field string
		off   int
	}{
		{"dvmOffset", plan.DVMOffset},
		{"kubeapiLB", plan.KubeAPILB},
		{"ingressLB", plan.IngressLB},
		{"lbNodeOffset", plan.LBNodeOffset},
		{"mNodeOffset", plan.MNodeOffset},
	}
	for _, n := range named {
		if n.off <= 0 {
			continue
		}
		off := int64(Shift(n.off, plan.VMIPOffset))
		if off > last {
			return fmt.Errorf("%s %d with vmIPOffset %d is outside %s", n.field, n.off, plan.VMIPOffset, plan.CIDR)
		}
	}
	masters := int64(0)
	if plan.MasterCount > 0 && plan.MNodeOffset > 0 {
		masters = int64(Shift(plan.MNodeOffset, plan.VMIPOffset)) + int64(plan.MasterCount) - 1
	}
	if masters > last {
		return fmt.Errorf("mNodeOffset %d for masterCount %d with vmIPOffset %d is outside %s",
			plan.MNodeOffset, plan.MasterCount, plan.VMIPOffset, plan.CIDR)
	}
	lbCount := int64(plan.LBNodeCount)
	if lbCount < 1 {
		lbCount = 1
	}
	if plan.LBNodeOffset > 0 {
		if int64(Shift(plan.LBNodeOffset, plan.VMIPOffset))+lbCount-1 > last {
			return fmt.Errorf("lbNodeOffset %d for lbNodeCount %d with vmIPOffset %d is outside %s",
				plan.LBNodeOffset, plan.LBNodeCount, plan.VMIPOffset, plan.CIDR)
		}
	}
	return nil
}

// Validate reports whether a plan can allocate at all, without building an
// allocator. The controller uses it for the InvalidSpec condition and the
// management page for its window warning, so both agree with what New enforces.
func Validate(plan Plan) error {
	p, err := netip.ParsePrefix(plan.CIDR)
	if err != nil {
		return fmt.Errorf("parse cidr %q: %w", plan.CIDR, err)
	}
	if !p.Addr().Is4() {
		return fmt.Errorf("only IPv4 CIDRs are supported, got %q", plan.CIDR)
	}
	total := uint64(1) << uint(32-p.Masked().Bits())
	return validatePlan(plan, total)
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
	plan   Plan
	prefix netip.Prefix
	total  uint64
	// windowStart and windowEnd are the offsets this pool may allocate from.
	// Everything outside is another pool's business: the addresses are real,
	// but they are not ours to hand out.
	windowStart uint64
	windowEnd   uint64
	reserved    map[netip.Addr]struct{}
	used        map[netip.Addr]struct{}
	ledger      Ledger
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
	total := uint64(1) << uint(32-p.Bits())
	if err := validatePlan(plan, total); err != nil {
		return nil, err
	}

	a := &Allocator{
		plan:     shiftPlan(plan),
		prefix:   p,
		total:    total,
		reserved: map[netip.Addr]struct{}{},
		used:     map[netip.Addr]struct{}{},
		ledger: Ledger{
			NextIndex: ledger.NextIndex,
			Used:      append([]string(nil), ledger.Used...),
			Free:      append([]string(nil), ledger.Free...),
		},
	}
	a.windowStart, a.windowEnd = a.window()
	a.buildReserved()
	a.buildUsed()
	return a, nil
}

// window is the range of offsets this pool may allocate from: WNodeOffset for
// NodeCount addresses, both already absolute inside the prefix. The end is
// exclusive here.
func (a *Allocator) window() (start, end uint64) {
	start = 0
	if a.plan.WNodeOffset > 0 {
		start = uint64(a.plan.WNodeOffset)
	}
	end = start + uint64(a.plan.NodeCount)
	if end > a.total {
		end = a.total
	}
	return start, end
}

// inWindow reports whether an offset belongs to this pool. Addresses outside it
// belong to another pool that declared a neighbouring window.
func (a *Allocator) inWindow(off uint64) bool {
	return off >= a.windowStart && off < a.windowEnd
}

func (a *Allocator) buildReserved() {
	a.reserveOffset(1) // the subnet's gateway: never shifted by vmIPOffset
	if a.plan.KubeAPILB > 0 {
		a.reserveOffset(a.plan.KubeAPILB)
	}
	if a.plan.IngressLB > 0 {
		a.reserveOffset(a.plan.IngressLB)
	}
	if a.plan.DVMOffset != 0 {
		a.reserveOffset(a.plan.DVMOffset)
	}
	if a.plan.LBNodeOffset > 0 {
		count := int(a.plan.LBNodeCount)
		if count < 1 {
			count = 1
		}
		for i := 0; i < count; i++ {
			a.reserveOffset(a.plan.LBNodeOffset + i)
		}
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

// offsetOf is the position of addr within the prefix, the same number the
// plan's offsets are expressed in.
func (a *Allocator) offsetOf(addr netip.Addr) uint64 {
	if !a.prefix.Contains(addr) {
		return ^uint64(0)
	}
	base := a.prefix.Masked().Addr().As4()
	have := addr.As4()
	return uint64(binary.BigEndian.Uint32(have[:]) - binary.BigEndian.Uint32(base[:]))
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
		if !a.inWindow(a.offsetOf(addr)) {
			continue
		}
		if a.available(addr) {
			a.used[addr] = struct{}{}
			a.ledger.Used = append(a.ledger.Used, addr.String())
			return addr.String(), nil
		}
	}

	for off := a.windowStart; off < a.windowEnd; off++ {
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
	return "", fmt.Errorf("no free IPs in the pool's window of %s (wNodeOffset %d, nodeCount %d)",
		a.plan.CIDR, a.plan.WNodeOffset, a.plan.NodeCount)
}

// Ordinal returns the Terraform-style worker ordinal for ip: the offset from
// the start of the prefix relative to WNodeOffset, starting at 1. The
// controller derives worker names from this value, so a name is always tied to
// the IP it was allocated with, even when a freed address is reused.
func (a *Allocator) Ordinal(ip string) (int, error) {
	addr, err := netip.ParseAddr(ip)
	if err != nil {
		return 0, fmt.Errorf("parse ip %q: %w", ip, err)
	}
	if !a.prefix.Contains(addr) {
		return 0, fmt.Errorf("ip %s is outside %s", ip, a.prefix)
	}
	baseBytes := a.prefix.Masked().Addr().As4()
	addrBytes := addr.As4()
	base := binary.BigEndian.Uint32(baseBytes[:])
	off := int(uint32(binary.BigEndian.Uint32(addrBytes[:]) - base))
	return off - a.plan.WNodeOffset + 1, nil
}

// Adopt records ip as allocated without going through Allocate. The reconciler
// calls it for every VM it finds in vCD, so a lost, partial or stale status can
// never lead to the same address being handed out twice.
func (a *Allocator) Adopt(ip string) error {
	addr, err := netip.ParseAddr(ip)
	if err != nil {
		return fmt.Errorf("parse ip %q: %w", ip, err)
	}
	if !a.prefix.Contains(addr) {
		return fmt.Errorf("ip %s is outside %s", ip, a.prefix)
	}
	if _, ok := a.used[addr]; !ok {
		a.used[addr] = struct{}{}
		a.ledger.Used = append(a.ledger.Used, addr.String())
	}
	a.removeFree(addr)
	if ord, err := a.Ordinal(ip); err == nil && ord > a.ledger.NextIndex {
		a.ledger.NextIndex = ord
	}
	return nil
}

func (a *Allocator) removeFree(addr netip.Addr) {
	kept := a.ledger.Free[:0]
	for _, s := range a.ledger.Free {
		if s != addr.String() {
			kept = append(kept, s)
		}
	}
	a.ledger.Free = kept
}

// Reserve keeps ip out of the Allocate pool without a VM behind it. The
// reconciler calls it for a slot whose VM disappeared outside the loop: the
// address stays allocated so the identity (address plus the worker name derived
// from its ordinal) survives until the operator writes the slot off.
func (a *Allocator) Reserve(ip string) error {
	addr, err := netip.ParseAddr(ip)
	if err != nil {
		return fmt.Errorf("parse ip %q: %w", ip, err)
	}
	if !a.prefix.Contains(addr) {
		return fmt.Errorf("ip %s is outside %s", ip, a.prefix)
	}
	if _, ok := a.reserved[addr]; ok {
		return fmt.Errorf("ip %s is reserved for infrastructure", ip)
	}
	if _, ok := a.used[addr]; !ok {
		a.used[addr] = struct{}{}
		a.ledger.Used = append(a.ledger.Used, addr.String())
	}
	a.removeFree(addr)
	sort.Strings(a.ledger.Used)
	if ord, err := a.Ordinal(ip); err == nil && ord > a.ledger.NextIndex {
		a.ledger.NextIndex = ord
	}
	return nil
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
