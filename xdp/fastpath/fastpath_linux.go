//go:build linux

package fastpath

import (
	"errors"
	"fmt"
	"net"
	"os"
	"path/filepath"
	"time"

	"github.com/cilium/ebpf"
	"github.com/cilium/ebpf/link"
	"golang.org/x/sys/unix"
)

// Mirrors of the C structs. Field order and widths must match fastpath.c.
type ban struct{ UntilNs uint64 }
type config struct{ RatePps, Burst uint64 }

// Counter slots, matching the enum in fastpath.c.
const (
	CounterPassed = iota
	CounterDroppedBanned
	CounterDroppedRate
)

// Counters is the sum of every CPU's slots.
type Counters struct {
	Passed, DroppedBanned, DroppedRate uint64
}

// Maps are the program's pinned maps. Opening them needs only the pin
// directory, so the ban/unban/stats subcommands work while the agent runs.
type Maps struct {
	Banned, Buckets, Config, Counters *ebpf.Map
}

// Fastpath is a loaded and attached program.
type Fastpath struct {
	Maps
	objs fastpathObjects
	link link.Link
	Mode string // "native" or "generic"
}

// Attach loads the program, pins its maps under pinDir (reusing them if they
// are already there, so bans survive an agent restart) and attaches it to
// iface. Native (driver) mode is tried first; drivers without XDP support fall
// back to generic mode, which runs after skb allocation and so saves less.
func Attach(iface, pinDir string) (*Fastpath, error) {
	ifc, err := net.InterfaceByName(iface)
	if err != nil {
		return nil, fmt.Errorf("fastpath: interface %q: %w", iface, err)
	}
	if err := os.MkdirAll(pinDir, 0o700); err != nil {
		return nil, fmt.Errorf("fastpath: pin dir: %w", err)
	}

	fp := &Fastpath{}
	opts := &ebpf.CollectionOptions{Maps: ebpf.MapOptions{PinPath: pinDir}}
	if err := loadFastpathObjects(&fp.objs, opts); err != nil {
		var ve *ebpf.VerifierError
		if errors.As(err, &ve) {
			return nil, fmt.Errorf("fastpath: verifier rejected the program:\n%+v", ve)
		}
		return nil, fmt.Errorf("fastpath: load: %w", err)
	}
	fp.Maps = Maps{fp.objs.Banned, fp.objs.Buckets, fp.objs.Config, fp.objs.Counters}

	for _, mode := range []struct {
		name  string
		flags link.XDPAttachFlags
	}{{"native", link.XDPDriverMode}, {"generic", link.XDPGenericMode}} {
		l, err := link.AttachXDP(link.XDPOptions{
			Program:   fp.objs.LimitplaneFastpath,
			Interface: ifc.Index,
			Flags:     mode.flags,
		})
		if err == nil {
			fp.link, fp.Mode = l, mode.name
			return fp, nil
		}
		if mode.name == "generic" {
			fp.objs.Close()
			return nil, fmt.Errorf("fastpath: attach to %s: %w", iface, err)
		}
	}
	return fp, nil // unreachable
}

// Close detaches the program. The pinned maps stay, so a restarted agent
// picks up the same bans; remove pinDir to forget them.
func (f *Fastpath) Close() error {
	var errs []error
	if f.link != nil {
		errs = append(errs, f.link.Close())
	}
	errs = append(errs, f.objs.Close())
	return errors.Join(errs...)
}

// OpenPinned opens the maps a running agent pinned.
func OpenPinned(pinDir string) (*Maps, error) {
	m := &Maps{}
	for name, dst := range map[string]**ebpf.Map{
		"banned": &m.Banned, "buckets": &m.Buckets, "config": &m.Config, "counters": &m.Counters,
	} {
		mp, err := ebpf.LoadPinnedMap(filepath.Join(pinDir, name), nil)
		if err != nil {
			return nil, fmt.Errorf("fastpath: open pinned %s (is the agent running?): %w", name, err)
		}
		*dst = mp
	}
	return m, nil
}

// SetLimit configures the per-IP packet bucket. rate 0 disables it.
func (m *Maps) SetLimit(ratePPS, burst uint64) error {
	return m.Config.Put(uint32(0), config{RatePps: ratePPS, Burst: burst})
}

// Ban drops every packet from k for d (0 = until Unban). The expiry is
// written in CLOCK_MONOTONIC, the clock bpf_ktime_get_ns reads, so the kernel
// enforces it by itself.
func (m *Maps) Ban(k Key, d time.Duration) error {
	var until uint64
	if d > 0 {
		until = uint64(monotonicNow() + d.Nanoseconds())
	}
	return m.Banned.Put(k, ban{UntilNs: until})
}

// Unban removes k from the ban list. Unbanning an address that is not banned
// is not an error.
func (m *Maps) Unban(k Key) error {
	if err := m.Banned.Delete(k); err != nil && !errors.Is(err, ebpf.ErrKeyNotExist) {
		return err
	}
	return nil
}

// BannedKeys lists every address currently on the ban list.
func (m *Maps) BannedKeys() ([]Key, error) {
	var (
		out []Key
		k   Key
		v   ban
	)
	it := m.Banned.Iterate()
	for it.Next(&k, &v) {
		out = append(out, k)
	}
	return out, it.Err()
}

// Read sums the per-CPU counters.
func (m *Maps) Read() (Counters, error) {
	var c Counters
	for slot, dst := range []*uint64{&c.Passed, &c.DroppedBanned, &c.DroppedRate} {
		var perCPU []uint64
		if err := m.Counters.Lookup(uint32(slot), &perCPU); err != nil {
			return c, err
		}
		for _, v := range perCPU {
			*dst += v
		}
	}
	return c, nil
}

func monotonicNow() int64 {
	var ts unix.Timespec
	_ = unix.ClockGettime(unix.CLOCK_MONOTONIC, &ts)
	return ts.Nano()
}
