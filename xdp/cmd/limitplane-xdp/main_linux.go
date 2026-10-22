//go:build linux

// Command limitplane-xdp puts LimitPlane's kernel fast path on a network
// interface and keeps its ban list in step with the gateway's autopilot.
//
//	limitplane-xdp -iface eth0                     # attach + sync, until Ctrl-C
//	LIMITPLANE_ADMIN_KEY=k limitplane-xdp -iface eth0 -gateway http://127.0.0.1:3000
//	                                               # sync from the reverse proxy
//	limitplane-xdp ban   10.0.0.9 -for 5m          # operator ban, by hand
//	limitplane-xdp unban 10.0.0.9
//	limitplane-xdp stats                           # passed / dropped counters
//
// Two layers, one rule each: the kernel drops what is obviously bad (a banned
// address, a per-IP packet flood) before the stack spends anything on it; the
// Go gateway behind it still decides everything that needs meaning — tenants,
// cost classes, monthly plans.
package main

import (
	"context"
	"flag"
	"fmt"
	"log/slog"
	"net/http"
	"net/netip"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/mightbeanshuu/limitplane/xdp/fastpath"
)

const defaultPin = "/sys/fs/bpf/limitplane"

func main() {
	if len(os.Args) > 1 {
		switch os.Args[1] {
		case "ban", "unban", "stats":
			if err := operator(os.Args[1], os.Args[2:]); err != nil {
				fmt.Fprintln(os.Stderr, "limitplane-xdp:", err)
				os.Exit(1)
			}
			return
		}
	}
	if err := run(os.Args[1:]); err != nil {
		slog.Error("limitplane-xdp", "err", err)
		os.Exit(1)
	}
}

func run(args []string) error {
	fs := flag.NewFlagSet("limitplane-xdp", flag.ExitOnError)
	iface := fs.String("iface", "", "network interface to attach to (required)")
	gateway := fs.String("gateway", "http://localhost:3000", "LimitPlane gateway or proxy to read autopilot cooldowns from; empty disables sync")
	rate := fs.Uint64("rate", 2000, "per-IP packet budget, packets/second (0 disables the flood check)")
	burst := fs.Uint64("burst", 4000, "per-IP burst, packets")
	every := fs.Duration("sync", time.Second, "how often to read the gateway's cooldowns")
	metricsAddr := fs.String("metrics-addr", ":9091", "Prometheus metrics address; empty disables")
	pin := fs.String("pin", defaultPin, "bpffs directory for the pinned maps")
	never := fs.String("never", "", "comma-separated IPs/CIDRs that are never banned (loopback always is)")
	maxBans := fs.Int("max-bans", 10000, "most gateway bans copied into the kernel")
	_ = fs.Parse(args)
	if *iface == "" {
		fs.Usage()
		return fmt.Errorf("-iface is required")
	}
	neverList, err := parsePrefixes(*never)
	if err != nil {
		return fmt.Errorf("-never: %w", err)
	}

	fp, err := fastpath.Attach(*iface, *pin)
	if err != nil {
		return err
	}
	defer fp.Close()
	if err := fp.SetLimit(*rate, *burst); err != nil {
		return err
	}
	slog.Info("attached", "iface", *iface, "mode", fp.Mode, "rate_pps", *rate, "burst", *burst, "pin", *pin)

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	if *metricsAddr != "" {
		srv := &http.Server{Addr: *metricsAddr, Handler: metricsHandler(&fp.Maps), ReadHeaderTimeout: 5 * time.Second}
		go func() {
			if err := srv.ListenAndServe(); err != nil && err != http.ErrServerClosed {
				slog.Error("metrics server", "err", err)
			}
		}()
		defer srv.Close()
	}

	if *gateway == "" {
		<-ctx.Done()
		slog.Info("detaching")
		return nil
	}

	g := &gatewayClient{
		base:     *gateway,
		adminKey: os.Getenv("LIMITPLANE_ADMIN_KEY"), // set when -gateway is the reverse proxy
		token:    os.Getenv("LIMITPLANE_TOKEN"),
		email:    os.Getenv("LIMITPLANE_EMAIL"),
		password: os.Getenv("LIMITPLANE_PASSWORD"),
		http:     &http.Client{Timeout: 5 * time.Second},
	}
	owned := map[fastpath.Key]bool{}
	tick := time.NewTicker(*every)
	defer tick.Stop()
	for {
		syncOnce(ctx, g, &fp.Maps, owned, neverList, *maxBans)
		select {
		case <-ctx.Done():
			slog.Info("detaching")
			return nil
		case <-tick.C:
		}
	}
}

// syncOnce copies the gateway's current cooldowns into the kernel. When the
// gateway is unreachable the kernel keeps the bans it has: each one carries
// its own expiry, so they lapse on time without the agent.
func syncOnce(ctx context.Context, g *gatewayClient, m *fastpath.Maps, owned map[fastpath.Key]bool, never []netip.Prefix, max int) {
	bans, err := g.bans(ctx)
	if err != nil {
		if ctx.Err() == nil {
			slog.Warn("sync: gateway unreachable, keeping current bans", "err", err)
		}
		return
	}
	add, remove := reconcile(owned, desiredBans(bans, never, max))
	for k, d := range add {
		if err := m.Ban(k, d); err != nil {
			slog.Warn("sync: ban", "ip", k.Addr(), "err", err)
			continue
		}
		if !owned[k] {
			slog.Info("kernel ban", "ip", k.Addr(), "for", d.Round(time.Second))
		}
		owned[k] = true
	}
	for _, k := range remove {
		if err := m.Unban(k); err != nil {
			slog.Warn("sync: unban", "ip", k.Addr(), "err", err)
			continue
		}
		slog.Info("kernel unban", "ip", k.Addr())
		delete(owned, k)
	}
}

// operator implements the ban/unban/stats subcommands against a running
// agent's pinned maps.
func operator(cmd string, args []string) error {
	fs := flag.NewFlagSet(cmd, flag.ExitOnError)
	pin := fs.String("pin", defaultPin, "bpffs directory for the pinned maps")
	dur := fs.Duration("for", 0, "ban duration (0 = until unban)")
	// Allow the address before or after the flags: `ban 1.2.3.4 -for 5m`.
	var addr string
	if len(args) > 0 && len(args[0]) > 0 && args[0][0] != '-' {
		addr, args = args[0], args[1:]
	}
	_ = fs.Parse(args)
	if addr == "" && fs.NArg() > 0 {
		addr = fs.Arg(0)
	}
	m, err := fastpath.OpenPinned(*pin)
	if err != nil {
		return err
	}
	switch cmd {
	case "stats":
		c, err := m.Read()
		if err != nil {
			return err
		}
		keys, err := m.BannedKeys()
		if err != nil {
			return err
		}
		fmt.Printf("passed=%d dropped_banned=%d dropped_rate=%d banned_ips=%d\n", c.Passed, c.DroppedBanned, c.DroppedRate, len(keys))
		return nil
	}
	if addr == "" {
		return fmt.Errorf("usage: limitplane-xdp %s <ip>", cmd)
	}
	k, err := fastpath.ParseKey(addr)
	if err != nil {
		return err
	}
	if cmd == "ban" {
		return m.Ban(k, *dur)
	}
	return m.Unban(k)
}
