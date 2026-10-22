package fastpath

import (
	"net/netip"
	"testing"
)

// The kernel builds the key from the raw header; these bytes must match it
// exactly or a ban written from Go would never match a packet.
func TestIPv4KeyIsMapped(t *testing.T) {
	k, err := ParseKey("10.77.0.3")
	if err != nil {
		t.Fatal(err)
	}
	want := Key{10: 0xff, 11: 0xff, 12: 10, 13: 77, 14: 0, 15: 3}
	if k != want {
		t.Fatalf("key = % x, want % x", k, want)
	}
	if got := k.Addr(); got != netip.MustParseAddr("10.77.0.3") {
		t.Fatalf("round trip = %v", got)
	}
}

func TestIPv6KeyIsVerbatim(t *testing.T) {
	k, err := ParseKey("fd77::2")
	if err != nil {
		t.Fatal(err)
	}
	if k.Addr() != netip.MustParseAddr("fd77::2") {
		t.Fatalf("round trip = %v", k.Addr())
	}
	if k[0] != 0xfd || k[1] != 0x77 || k[15] != 2 {
		t.Fatalf("key = % x", k)
	}
}

func TestParseKeyRejectsGarbage(t *testing.T) {
	if _, err := ParseKey("not-an-ip"); err == nil {
		t.Fatal("expected an error")
	}
}
