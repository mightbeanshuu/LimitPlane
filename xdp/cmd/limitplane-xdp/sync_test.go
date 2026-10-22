package main

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"net/netip"
	"testing"
	"time"

	"github.com/mightbeanshuu/limitplane/xdp/fastpath"
)

func key(s string) fastpath.Key {
	k, err := fastpath.ParseKey(s)
	if err != nil {
		panic(err)
	}
	return k
}

func TestOnlyAnonymousTenantsBecomeKernelBans(t *testing.T) {
	got := desiredBans([]activeBan{
		{TenantID: "anon:203.0.113.7", RemainingMs: 5000},
		{TenantID: "acme", RemainingMs: 5000},          // named tenant: stays in Go
		{TenantID: "anon:2001:db8::9", RemainingMs: 1}, // v6 works too
		{TenantID: "anon:not-an-ip", RemainingMs: 5000},
		{TenantID: "anon:198.51.100.1", RemainingMs: 0}, // already lapsed
	}, nil, 100)

	if len(got) != 2 {
		t.Fatalf("got %d bans, want 2: %v", len(got), got)
	}
	if got[key("203.0.113.7")] != 5*time.Second {
		t.Fatalf("v4 ban duration = %v", got[key("203.0.113.7")])
	}
	if _, ok := got[key("2001:db8::9")]; !ok {
		t.Fatal("v6 ban missing")
	}
}

func TestNeverListAndLoopbackAreNeverBanned(t *testing.T) {
	never, err := parsePrefixes("10.0.0.0/8, 192.0.2.5")
	if err != nil {
		t.Fatal(err)
	}
	got := desiredBans([]activeBan{
		{TenantID: "anon:10.1.2.3", RemainingMs: 5000},
		{TenantID: "anon:192.0.2.5", RemainingMs: 5000},
		{TenantID: "anon:127.0.0.1", RemainingMs: 5000},
		{TenantID: "anon:::1", RemainingMs: 5000},
		{TenantID: "anon:192.0.2.6", RemainingMs: 5000},
	}, never, 100)
	if len(got) != 1 {
		t.Fatalf("got %v, want only 192.0.2.6", got)
	}
	if _, ok := got[key("192.0.2.6")]; !ok {
		t.Fatal("192.0.2.6 should be banned")
	}
}

func TestMaxCapsTheBanList(t *testing.T) {
	got := desiredBans([]activeBan{
		{TenantID: "anon:192.0.2.1", RemainingMs: 1000},
		{TenantID: "anon:192.0.2.2", RemainingMs: 1000},
		{TenantID: "anon:192.0.2.3", RemainingMs: 1000},
	}, nil, 2)
	if len(got) != 2 {
		t.Fatalf("got %d, want the cap of 2", len(got))
	}
}

func TestReconcileRemovesOnlyWhatItOwns(t *testing.T) {
	owned := map[fastpath.Key]bool{key("192.0.2.1"): true, key("192.0.2.2"): true}
	want := map[fastpath.Key]time.Duration{key("192.0.2.2"): time.Second, key("192.0.2.3"): time.Second}

	add, remove := reconcile(owned, want)
	if len(add) != 2 {
		t.Fatalf("add = %v", add)
	}
	if len(remove) != 1 || remove[0] != key("192.0.2.1") {
		t.Fatalf("remove = %v, want only the lapsed 192.0.2.1", remove)
	}
}

func TestGatewayClientLogsInAgainAfter401(t *testing.T) {
	logins := 0
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/v1/auth/login":
			logins++
			_ = json.NewEncoder(w).Encode(map[string]string{"token": "fresh"})
		case "/v1/admin/autopilot":
			if r.Header.Get("Authorization") != "Bearer fresh" {
				w.WriteHeader(http.StatusUnauthorized)
				return
			}
			_ = json.NewEncoder(w).Encode(map[string]any{
				"activeBans": []activeBan{{TenantID: "anon:192.0.2.9", RemainingMs: 3000}},
			})
		}
	}))
	defer srv.Close()

	g := &gatewayClient{base: srv.URL, token: "expired", email: "a@b.c", password: "pw", http: srv.Client()}
	bans, err := g.bans(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if logins != 1 || len(bans) != 1 || bans[0].TenantID != "anon:192.0.2.9" {
		t.Fatalf("logins=%d bans=%v", logins, bans)
	}
}

func TestGatewayClientReadsTheProxyWithItsAdminKey(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/_limitplane/autopilot" || r.Header.Get("X-LimitPlane-Admin") != "k" {
			w.WriteHeader(http.StatusNotFound)
			return
		}
		_ = json.NewEncoder(w).Encode(map[string]any{
			"activeBans": []activeBan{{TenantID: "anon:198.51.100.4", RemainingMs: 2000}},
		})
	}))
	defer srv.Close()

	g := &gatewayClient{base: srv.URL, adminKey: "k", http: srv.Client()}
	bans, err := g.bans(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if len(bans) != 1 || bans[0].TenantID != "anon:198.51.100.4" {
		t.Fatalf("bans = %v", bans)
	}

	g.adminKey = "wrong"
	if _, err := g.bans(context.Background()); err == nil {
		t.Fatal("a wrong admin key must be an error, not an empty ban list")
	}
}

func TestParsePrefixesAcceptsBareAddresses(t *testing.T) {
	p, err := parsePrefixes("192.0.2.5,2001:db8::/32")
	if err != nil {
		t.Fatal(err)
	}
	if !p[0].Contains(netip.MustParseAddr("192.0.2.5")) || p[0].Bits() != 32 {
		t.Fatalf("bare v4 → %v", p[0])
	}
	if !p[1].Contains(netip.MustParseAddr("2001:db8::1")) {
		t.Fatalf("v6 prefix → %v", p[1])
	}
}
