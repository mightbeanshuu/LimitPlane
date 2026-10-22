package main

// The sync half of the agent: ask the gateway which clients its autopilot has
// put in cooldown, and work out what the kernel ban list should look like.
// Nothing here touches BPF, so it is tested on any OS.

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/netip"
	"strings"
	"time"

	"github.com/mightbeanshuu/limitplane/xdp/fastpath"
)

// activeBan is one entry of GET /v1/admin/autopilot's "activeBans".
type activeBan struct {
	TenantID    string `json:"tenantId"`
	RemainingMs int64  `json:"remainingMs"`
}

// gatewayClient reads the autopilot state. The endpoint needs a JWT; it is
// either given (LIMITPLANE_TOKEN) or obtained by logging in, and a 401 from an
// expired token triggers one fresh login.
type gatewayClient struct {
	base            string
	token           string
	email, password string
	http            *http.Client
}

var errUnauthorized = errors.New("gateway answered 401: set LIMITPLANE_TOKEN, or LIMITPLANE_EMAIL and LIMITPLANE_PASSWORD")

func (g *gatewayClient) bans(ctx context.Context) ([]activeBan, error) {
	if g.token == "" && g.email != "" {
		if err := g.login(ctx); err != nil {
			return nil, err
		}
	}
	bans, err := g.fetch(ctx)
	if errors.Is(err, errUnauthorized) && g.email != "" {
		if err := g.login(ctx); err != nil {
			return nil, err
		}
		bans, err = g.fetch(ctx)
	}
	return bans, err
}

func (g *gatewayClient) fetch(ctx context.Context) ([]activeBan, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, g.base+"/v1/admin/autopilot", nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Authorization", "Bearer "+g.token)
	res, err := g.http.Do(req)
	if err != nil {
		return nil, err
	}
	defer res.Body.Close()
	if res.StatusCode == http.StatusUnauthorized {
		return nil, errUnauthorized
	}
	if res.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("gateway answered %s", res.Status)
	}
	var state struct {
		ActiveBans []activeBan `json:"activeBans"`
	}
	if err := json.NewDecoder(res.Body).Decode(&state); err != nil {
		return nil, fmt.Errorf("decode autopilot state: %w", err)
	}
	return state.ActiveBans, nil
}

func (g *gatewayClient) login(ctx context.Context) error {
	body, _ := json.Marshal(map[string]string{"email": g.email, "password": g.password})
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, g.base+"/v1/auth/login", bytes.NewReader(body))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")
	res, err := g.http.Do(req)
	if err != nil {
		return err
	}
	defer res.Body.Close()
	var out struct {
		Token string `json:"token"`
	}
	if res.StatusCode != http.StatusOK || json.NewDecoder(res.Body).Decode(&out) != nil || out.Token == "" {
		return fmt.Errorf("gateway login failed: %s", res.Status)
	}
	g.token = out.Token
	return nil
}

// desiredBans turns the gateway's cooldowns into kernel bans.
//
// Only anonymous tenants become kernel bans. An anonymous tenant is
// "anon:<ip>", so its identity IS the address. A named tenant is an API key,
// and many keys can sit behind one office NAT — banning that address in the
// kernel would punish every other customer behind it, so those stay in the Go
// layer where the key is visible.
//
// never lists addresses that must not be banned whatever the gateway says
// (loopback, the gateway's own proxy, monitoring). The gateway derives
// anonymous identities from X-Forwarded-For, which a client can forge, so this
// list and max are the guard rails against a forged header getting somebody
// else's address dropped at the NIC.
func desiredBans(bans []activeBan, never []netip.Prefix, max int) map[fastpath.Key]time.Duration {
	out := make(map[fastpath.Key]time.Duration)
	for _, b := range bans {
		ip, ok := strings.CutPrefix(b.TenantID, "anon:")
		if !ok || b.RemainingMs <= 0 {
			continue
		}
		addr, err := netip.ParseAddr(ip)
		if err != nil {
			continue
		}
		addr = addr.Unmap().WithZone("")
		if neverBan(addr, never) {
			continue
		}
		if len(out) >= max {
			break
		}
		out[fastpath.KeyFor(addr)] = time.Duration(b.RemainingMs) * time.Millisecond
	}
	return out
}

func neverBan(a netip.Addr, never []netip.Prefix) bool {
	if a.IsLoopback() || a.IsUnspecified() {
		return true
	}
	for _, p := range never {
		if p.Contains(a) {
			return true
		}
	}
	return false
}

// reconcile works out what to change. owned is the set of bans this agent
// wrote earlier; only those are ever removed, so a ban an operator added by
// hand with `limitplane-xdp ban` is left alone.
func reconcile(owned map[fastpath.Key]bool, want map[fastpath.Key]time.Duration) (add map[fastpath.Key]time.Duration, remove []fastpath.Key) {
	add = make(map[fastpath.Key]time.Duration)
	for k, d := range want {
		add[k] = d // re-put refreshes the expiry if the gateway extended it
	}
	for k := range owned {
		if _, still := want[k]; !still {
			remove = append(remove, k)
		}
	}
	return add, remove
}

func parsePrefixes(s string) ([]netip.Prefix, error) {
	var out []netip.Prefix
	for _, part := range strings.Split(s, ",") {
		part = strings.TrimSpace(part)
		if part == "" {
			continue
		}
		if !strings.Contains(part, "/") {
			a, err := netip.ParseAddr(part)
			if err != nil {
				return nil, err
			}
			part = netip.PrefixFrom(a, a.BitLen()).String()
		}
		p, err := netip.ParsePrefix(part)
		if err != nil {
			return nil, err
		}
		out = append(out, p.Masked())
	}
	return out, nil
}
