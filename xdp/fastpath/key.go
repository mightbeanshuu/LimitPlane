// Package fastpath loads LimitPlane's XDP program and gives Go a typed handle
// on its maps: the ban list, the per-IP token buckets, the config and the
// drop counters. See ../bpf/fastpath.c for the kernel side.
package fastpath

import (
	"fmt"
	"net/netip"
)

// Key is the 16-byte address the kernel maps are keyed by. IPv4 is stored
// IPv4-mapped (::ffff:a.b.c.d) so one map holds both families — the C side
// builds exactly the same bytes from the packet header.
type Key [16]byte

// KeyFor converts an address to its map key.
func KeyFor(a netip.Addr) Key {
	return Key(a.As16())
}

// Addr converts a map key back to an address, unmapping IPv4.
func (k Key) Addr() netip.Addr {
	return netip.AddrFrom16(k).Unmap()
}

// ParseKey parses "10.0.0.1" or "fd00::1" into a key.
func ParseKey(s string) (Key, error) {
	a, err := netip.ParseAddr(s)
	if err != nil {
		return Key{}, fmt.Errorf("fastpath: %q is not an IP address", s)
	}
	return KeyFor(a.WithZone("")), nil
}
