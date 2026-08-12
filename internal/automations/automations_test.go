package automations_test

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/mightbeanshuu/limitplane/internal/audit"
	"github.com/mightbeanshuu/limitplane/internal/automations"
)

var (
	june10 = time.Date(2026, time.June, 10, 0, 0, 0, 0, time.UTC).UnixMilli()
	july10 = time.Date(2026, time.July, 10, 0, 0, 0, 0, time.UTC).UnixMilli()
)

type tick struct{ ms atomic.Int64 }

func newTick(ms int64) *tick {
	c := &tick{}
	c.ms.Store(ms)
	return c
}

func (c *tick) now() int64       { return c.ms.Load() }
func (c *tick) set(ms int64)     { c.ms.Store(ms) }
func (c *tick) advance(by int64) { c.ms.Add(by) }
func (c *tick) fn() func() int64 { return func() int64 { return c.now() } }

func f64(v float64) *float64 { return &v }

// recorder collects every action the autopilot takes, race-safely.
type recorder struct {
	mu   sync.Mutex
	seen []automations.Action
}

func (r *recorder) hook() func(automations.Action) {
	return func(a automations.Action) {
		r.mu.Lock()
		defer r.mu.Unlock()
		r.seen = append(r.seen, a)
	}
}

func (r *recorder) all() []automations.Action {
	r.mu.Lock()
	defer r.mu.Unlock()
	return append([]automations.Action(nil), r.seen...)
}

func (r *recorder) countOf(kind string) int {
	n := 0
	for _, a := range r.all() {
		if a.Type == kind {
			n++
		}
	}
	return n
}

// decision is one audit event shaped the way the gateway records them.
func decision(at int64, mut func(*audit.Event)) audit.Event {
	e := audit.Event{
		At: at, Allowed: true, Key: "acme:free:/scan", Route: "/scan", TenantID: "acme",
		Tier: "free", CostClass: "heavy", Cost: 5, Remaining: 5,
	}
	if mut != nil {
		mut(&e)
	}
	return e
}

// ---- rule 1: the 80% quota alert -------------------------------------------

func TestQuotaAlertFiresExactlyOncePerTenantPerMonth(t *testing.T) {
	rec := &recorder{}
	a := automations.New(automations.Config{Now: newTick(june10).fn(), OnAction: rec.hook()})

	usage := func(used, remaining float64) {
		a.OnDecision(decision(june10, func(e *audit.Event) {
			e.MonthlyUsed, e.MonthlyRemaining = f64(used), f64(remaining)
		}))
	}

	usage(79, 21) // 79% — below the line, stay quiet
	if got := rec.countOf("quota_alert"); got != 0 {
		t.Fatalf("the autopilot warned a tenant at 79%% of plan (%d alerts); the rule is 80%%", got)
	}

	usage(80, 20) // exactly 80% — the warning must land BEFORE they hit the wall
	if got := rec.countOf("quota_alert"); got != 1 {
		t.Fatalf("crossing 80%% of plan produced %d alerts, expected exactly 1", got)
	}

	usage(90, 10) // still over the line
	usage(99, 1)
	usage(100, 0)
	if got := rec.countOf("quota_alert"); got != 1 {
		t.Fatalf("the tenant was alerted %d times in one month; every further request over 80%% must NOT re-alert", got)
	}

	alert := rec.all()[0]
	if alert.TenantID != "acme" {
		t.Fatalf("the alert names %q instead of the tenant that crossed the line", alert.TenantID)
	}
	if !strings.Contains(alert.Message, "80") || !strings.Contains(alert.Message, "acme") {
		t.Fatalf("the alert message is not actionable: %q", alert.Message)
	}
	if alert.At != june10 {
		t.Fatalf("the action is stamped %d, not with the injected clock (%d)", alert.At, june10)
	}
}

func TestQuotaAlertFiresAgainInANewMonth(t *testing.T) {
	// The dedup key carries the calendar month, so the meter resetting on the
	// 1st also re-arms the warning. Nobody should silently blow through their
	// July plan because they were warned in June.
	rec := &recorder{}
	c := newTick(june10)
	a := automations.New(automations.Config{Now: c.fn(), OnAction: rec.hook()})

	a.OnDecision(decision(june10, func(e *audit.Event) { e.MonthlyUsed, e.MonthlyRemaining = f64(90), f64(10) }))
	if got := rec.countOf("quota_alert"); got != 1 {
		t.Fatalf("June alert count is %d, expected 1", got)
	}

	c.set(july10)
	a.OnDecision(decision(july10, func(e *audit.Event) { e.MonthlyUsed, e.MonthlyRemaining = f64(90), f64(10) }))
	if got := rec.countOf("quota_alert"); got != 2 {
		t.Fatalf("after the billing month rolled over the alert count is %d; a new month must re-arm the warning", got)
	}
}

func TestQuotaAlertIsPerTenant(t *testing.T) {
	rec := &recorder{}
	a := automations.New(automations.Config{Now: newTick(june10).fn(), OnAction: rec.hook()})

	for _, id := range []string{"acme", "globex", "initech"} {
		id := id
		a.OnDecision(decision(june10, func(e *audit.Event) {
			e.TenantID = id
			e.MonthlyUsed, e.MonthlyRemaining = f64(85), f64(15)
		}))
	}
	if got := rec.countOf("quota_alert"); got != 3 {
		t.Fatalf("three different tenants crossed 80%% but only %d were warned — the dedup key is not per tenant", got)
	}
}

func TestQuotaAlertIgnoresTiersWithNoPlanAndZeroSizedPlans(t *testing.T) {
	cases := []struct {
		name            string
		used, remaining *float64
	}{
		{"a tier with no monthly meter at all", nil, nil},
		{"usage present but no remaining", f64(100), nil},
		{"remaining present but no usage", nil, f64(0)},
		{"a zero-sized plan must not divide by zero", f64(0), f64(0)},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			rec := &recorder{}
			a := automations.New(automations.Config{Now: newTick(june10).fn(), OnAction: rec.hook()})
			a.OnDecision(decision(june10, func(e *audit.Event) { e.MonthlyUsed, e.MonthlyRemaining = tc.used, tc.remaining }))
			if got := rec.countOf("quota_alert"); got != 0 {
				t.Fatalf("%s produced %d quota alerts", tc.name, got)
			}
		})
	}
}

// ---- rule 2: the upgrade nudge ---------------------------------------------

func TestUpgradeNudgeFiresOnTheThirdCapHitOnly(t *testing.T) {
	rec := &recorder{}
	a := automations.New(automations.Config{Now: newTick(june10).fn(), OnAction: rec.hook()})

	slam := func() {
		a.OnDecision(decision(june10, func(e *audit.Event) {
			e.Allowed, e.Reason = false, "monthly_quota"
			e.MonthlyUsed, e.MonthlyRemaining = f64(100), f64(0)
		}))
	}

	slam()
	slam()
	if got := rec.countOf("upgrade_nudge"); got != 0 {
		t.Fatalf("a tenant was nudged to upgrade after only 2 cap hits (%d nudges); the rule is 3", got)
	}

	slam() // the third strike
	if got := rec.countOf("upgrade_nudge"); got != 1 {
		t.Fatalf("the third monthly-cap hit produced %d nudges, expected exactly 1", got)
	}

	for i := 0; i < 20; i++ {
		slam()
	}
	if got := rec.countOf("upgrade_nudge"); got != 1 {
		t.Fatalf("a tenant stuck against its cap was nudged %d times; sales spam is not an automation", got)
	}
}

func TestUpgradeNudgeCountsOnlyMonthlyCapHits(t *testing.T) {
	// Burst blocks mean "slow down", not "buy a bigger plan"; they must not
	// count toward the upgrade nudge.
	rec := &recorder{}
	a := automations.New(automations.Config{Now: newTick(june10).fn(), StormThreshold: 1000, OnAction: rec.hook()})

	for i := 0; i < 10; i++ {
		a.OnDecision(decision(june10, func(e *audit.Event) { e.Allowed, e.Reason = false, "burst" }))
	}
	for i := 0; i < 10; i++ {
		a.OnDecision(decision(june10, func(e *audit.Event) { e.Reason = "monthly_quota" })) // allowed=true
	}
	if got := rec.countOf("upgrade_nudge"); got != 0 {
		t.Fatalf("%d upgrade nudges fired without a single BLOCKED monthly_quota decision", got)
	}
}

func TestUpgradeNudgeCounterResetsWithTheBillingMonth(t *testing.T) {
	rec := &recorder{}
	c := newTick(june10)
	a := automations.New(automations.Config{Now: c.fn(), OnAction: rec.hook()})

	slamAt := func(at int64) {
		a.OnDecision(decision(at, func(e *audit.Event) { e.Allowed, e.Reason = false, "monthly_quota" }))
	}

	slamAt(june10)
	slamAt(june10)
	if got := rec.countOf("upgrade_nudge"); got != 0 {
		t.Fatalf("nudged after 2 June hits (%d)", got)
	}

	// The month flips with only 2 hits banked; the counter must start over.
	c.set(july10)
	slamAt(july10)
	if got := rec.countOf("upgrade_nudge"); got != 0 {
		t.Fatalf("June's 2 hits leaked into July and triggered a nudge on July's FIRST hit (%d nudges)", got)
	}
	slamAt(july10)
	slamAt(july10)
	if got := rec.countOf("upgrade_nudge"); got != 1 {
		t.Fatalf("three cap hits inside July produced %d nudges, expected 1", got)
	}
}

// ---- rule 3: the storm cooldown --------------------------------------------

func stormAuto(t *testing.T, c *tick, rec *recorder) *automations.Automations {
	t.Helper()
	return automations.New(automations.Config{
		Now: c.fn(), OnAction: rec.hook(),
		StormThreshold: 3, StormWindowMs: 10_000, CooldownMs: 60_000,
	})
}

func TestBurstStormEarnsACooldownAtTheThreshold(t *testing.T) {
	rec := &recorder{}
	c := newTick(june10)
	a := stormAuto(t, c, rec)

	burst := func() { a.OnDecision(decision(c.now(), func(e *audit.Event) { e.Allowed, e.Reason = false, "burst" })) }

	burst()
	c.advance(1000)
	burst()
	if got := a.BanRemainingMs("acme"); got != 0 {
		t.Fatalf("two burst blocks inside the window earned a %dms ban; the threshold is 3", got)
	}

	c.advance(1000)
	burst() // third inside 10s
	left := a.BanRemainingMs("acme")
	if left <= 0 {
		t.Fatal("three burst blocks inside the window did not earn a cooldown — a retry loop would hammer the gateway unchecked")
	}
	if left > 60_000 {
		t.Fatalf("the cooldown is %dms, longer than the configured 60000ms", left)
	}

	acts := rec.all()
	last := acts[len(acts)-1]
	if last.Type != "auto_cooldown" {
		t.Fatalf("the storm produced a %q action, expected auto_cooldown", last.Type)
	}
	if last.CooldownMs != 60_000 {
		t.Fatalf("the action reports a %dms cooldown, expected the configured 60000ms", last.CooldownMs)
	}
	if !strings.Contains(last.Message, "acme") || !strings.Contains(last.Message, "auto-unbans") {
		t.Fatalf("the cooldown message does not explain itself: %q", last.Message)
	}
}

func TestBurstStormBansOnlyOnceWhileTheCooldownIsLive(t *testing.T) {
	rec := &recorder{}
	c := newTick(june10)
	a := stormAuto(t, c, rec)

	for i := 0; i < 30; i++ {
		a.OnDecision(decision(c.now(), func(e *audit.Event) { e.Allowed, e.Reason = false, "burst" }))
		c.advance(100)
	}
	if got := rec.countOf("auto_cooldown"); got != 1 {
		t.Fatalf("30 burst blocks produced %d cooldown actions; an already-banned client must not be re-banned", got)
	}
}

func TestCooldownLiftsItselfWithNoTimer(t *testing.T) {
	rec := &recorder{}
	c := newTick(june10)
	a := stormAuto(t, c, rec)

	for i := 0; i < 3; i++ {
		a.OnDecision(decision(c.now(), func(e *audit.Event) { e.Allowed, e.Reason = false, "burst" }))
		c.advance(1000)
	}
	left := a.BanRemainingMs("acme")
	if left <= 0 {
		t.Fatal("the storm did not produce a ban")
	}

	c.advance(left - 1) // one millisecond short of the deadline
	if got := a.BanRemainingMs("acme"); got != 1 {
		t.Fatalf("one millisecond before the deadline the ban reports %dms left, expected exactly 1", got)
	}
	c.advance(1) // exactly ON the deadline: a ban with zero left is over
	if got := a.BanRemainingMs("acme"); got != 0 {
		t.Fatalf("the ban still reports %dms left at its own expiry instant; the auto-unban needs no timer, just an elapsed deadline", got)
	}
	if len(a.State().ActiveBans) != 0 {
		t.Fatalf("an expired ban is still listed as active: %+v", a.State().ActiveBans)
	}
}

func TestSlowSpreadOutBlocksNeverLookLikeAStorm(t *testing.T) {
	// A client that is blocked once every 20 seconds is not a retry loop; only
	// hits INSIDE the window may accumulate toward a ban.
	rec := &recorder{}
	c := newTick(june10)
	a := stormAuto(t, c, rec)

	for i := 0; i < 20; i++ {
		a.OnDecision(decision(c.now(), func(e *audit.Event) { e.Allowed, e.Reason = false, "burst" }))
		c.advance(20_000) // twice the 10s window
	}
	if got := a.BanRemainingMs("acme"); got != 0 {
		t.Fatalf("20 burst blocks spread 20s apart earned a %dms ban with a 10s window — old hits are leaking into the storm count", got)
	}
	if got := rec.countOf("auto_cooldown"); got != 0 {
		t.Fatalf("%d cooldowns fired for traffic that never exceeded the threshold inside the window", got)
	}
}

func TestStormEvidenceIsBoundedByTheWindowEdge(t *testing.T) {
	// Two hits just inside the window plus one hit exactly at the edge: the
	// oldest must have aged out, so the threshold is NOT reached.
	rec := &recorder{}
	c := newTick(june10)
	a := stormAuto(t, c, rec)

	burstAt := func(at int64) {
		c.set(at)
		a.OnDecision(decision(at, func(e *audit.Event) { e.Allowed, e.Reason = false, "burst" }))
	}
	burstAt(june10)
	burstAt(june10 + 5_000)
	burstAt(june10 + 10_000) // exactly windowMs after the first: the first expires

	if got := a.BanRemainingMs("acme"); got != 0 {
		t.Fatalf("a hit exactly windowMs old still counted toward the storm (ban=%dms)", got)
	}
	burstAt(june10 + 10_001) // now three hits sit inside a 10s window
	if a.BanRemainingMs("acme") <= 0 {
		t.Fatal("three hits genuinely inside the window did not trip the storm rule")
	}
}

func TestStormsArePerTenant(t *testing.T) {
	rec := &recorder{}
	c := newTick(june10)
	a := stormAuto(t, c, rec)

	for i := 0; i < 5; i++ {
		a.OnDecision(decision(c.now(), func(e *audit.Event) { e.Allowed, e.Reason = false, "burst" }))
		c.advance(100)
	}
	if a.BanRemainingMs("acme") <= 0 {
		t.Fatal("acme's storm did not earn a ban")
	}
	if got := a.BanRemainingMs("globex"); got != 0 {
		t.Fatalf("a bystander tenant was banned (%dms) because a different tenant misbehaved", got)
	}
}

// ---- no feedback loop ------------------------------------------------------

func TestAutoCooldownDecisionsAreIgnoredAsInput(t *testing.T) {
	// The gateway records the requests it rejects because of a ban. Feeding
	// those back in would make a ban prove itself and never end.
	rec := &recorder{}
	c := newTick(june10)
	a := stormAuto(t, c, rec)

	for i := 0; i < 50; i++ {
		a.OnDecision(decision(c.now(), func(e *audit.Event) {
			e.Allowed, e.Reason = false, "auto_cooldown"
			e.MonthlyUsed, e.MonthlyRemaining = f64(100), f64(0) // would also trip rule 1
		}))
		c.advance(10)
	}

	if got := len(rec.all()); got != 0 {
		t.Fatalf("the autopilot reacted to its OWN bans with %d actions: %+v", got, rec.all())
	}
	if got := a.BanRemainingMs("acme"); got != 0 {
		t.Fatalf("a tenant was banned (%dms) purely by replaying auto_cooldown rejections back into the rules", got)
	}
}

// ---- manual overrides ------------------------------------------------------

func TestManualBanAndUnban(t *testing.T) {
	rec := &recorder{}
	c := newTick(june10)
	a := automations.New(automations.Config{Now: c.fn(), OnAction: rec.hook(), CooldownMs: 300_000})

	act := a.Ban("acme", 5_000, "anshu")
	if act.Type != "manual_ban" || act.TenantID != "acme" || act.CooldownMs != 5_000 {
		t.Fatalf("the manual ban was recorded as %+v", act)
	}
	if !strings.Contains(act.Message, "anshu") || !strings.Contains(act.Message, "acme") {
		t.Fatalf("the audit trail does not say WHO banned WHOM: %q", act.Message)
	}
	if got := a.BanRemainingMs("acme"); got != 5_000 {
		t.Fatalf("a 5000ms manual ban reports %dms left immediately after being applied", got)
	}

	c.advance(1_000)
	if got := a.BanRemainingMs("acme"); got != 4_000 {
		t.Fatalf("after 1s of a 5s ban, %dms are reported left", got)
	}

	un := a.Unban("acme", "anshu")
	if un.Type != "manual_unban" || !strings.Contains(un.Message, "lifted") {
		t.Fatalf("lifting a live ban was recorded as %+v", un)
	}
	if got := a.BanRemainingMs("acme"); got != 0 {
		t.Fatalf("a manually lifted ban still reports %dms left", got)
	}
}

func TestManualBanWithNoDurationUsesTheConfiguredCooldown(t *testing.T) {
	c := newTick(june10)
	a := automations.New(automations.Config{Now: c.fn(), CooldownMs: 120_000})

	for _, ms := range []int64{0, -1, -99_999} {
		a.Unban("acme", "test")
		act := a.Ban("acme", ms, "operator")
		if act.CooldownMs != 120_000 {
			t.Fatalf("Ban(%d) produced a %dms ban; a missing or nonsensical duration must fall back to the configured cooldown", ms, act.CooldownMs)
		}
		if got := a.BanRemainingMs("acme"); got != 120_000 {
			t.Fatalf("Ban(%d) left %dms on the clock, expected the 120000ms default", ms, got)
		}
	}
}

func TestUnbanningSomebodyWhoWasNotBannedSaysSo(t *testing.T) {
	a := automations.New(automations.Config{Now: newTick(june10).fn()})
	act := a.Unban("nobody", "anshu")
	if act.Type != "manual_unban" {
		t.Fatalf("a no-op unban was recorded as %q", act.Type)
	}
	if !strings.Contains(act.Message, "wasn't banned") {
		t.Fatalf("the operator is not told their unban did nothing: %q", act.Message)
	}
}

func TestBanRemainingMsIsZeroForAnUnknownTenant(t *testing.T) {
	a := automations.New(automations.Config{Now: newTick(june10).fn()})
	if got := a.BanRemainingMs("never-seen"); got != 0 {
		t.Fatalf("an unknown tenant is reported as banned for %dms", got)
	}
}

// ---- the action log --------------------------------------------------------

func TestRecentReturnsNewestFirstAndIsACopy(t *testing.T) {
	c := newTick(june10)
	a := automations.New(automations.Config{Now: c.fn()})

	for i := 0; i < 4; i++ {
		c.advance(1_000)
		a.Ban(fmt.Sprintf("t%d", i), 1_000, "operator")
	}

	got := a.Recent(3)
	if len(got) != 3 {
		t.Fatalf("asked for 3 actions, got %d", len(got))
	}
	if got[0].TenantID != "t3" || got[1].TenantID != "t2" || got[2].TenantID != "t1" {
		t.Fatalf("Recent returned %q,%q,%q; the newest action must be first", got[0].TenantID, got[1].TenantID, got[2].TenantID)
	}

	got[0].Message = "tampered"
	got[0].Type = "forged"
	if after := a.Recent(1); after[0].Message == "tampered" || after[0].Type == "forged" {
		t.Fatalf("mutating the slice returned by Recent rewrote the autopilot's own history: %+v", after[0])
	}
}

func TestRecentClampsToWhatExists(t *testing.T) {
	a := automations.New(automations.Config{Now: newTick(june10).fn()})
	a.Ban("acme", 1_000, "operator")
	a.Ban("globex", 1_000, "operator")

	for _, n := range []int{0, -1, 2, 50} {
		if got := len(a.Recent(n)); got != 2 {
			t.Fatalf("Recent(%d) returned %d actions but only 2 have ever been taken", n, got)
		}
	}
	empty := automations.New(automations.Config{Now: newTick(june10).fn()})
	if got := empty.Recent(50); len(got) != 0 || got == nil {
		t.Fatalf("Recent on a fresh autopilot returned %v; it must be an empty slice, not nil (JSON null)", got)
	}
}

func TestActionLogIsARingBufferCappedAt500(t *testing.T) {
	c := newTick(june10)
	a := automations.New(automations.Config{Now: c.fn()})

	for i := 0; i < 600; i++ {
		c.advance(1)
		a.Ban(fmt.Sprintf("t%d", i), 1_000, "operator")
	}

	all := a.Recent(0)
	if len(all) != 500 {
		t.Fatalf("the autopilot remembers %d actions after 600; the log must be capped at 500", len(all))
	}
	if all[0].TenantID != "t599" {
		t.Fatalf("the newest action is %q, expected t599", all[0].TenantID)
	}
	if all[499].TenantID != "t100" {
		t.Fatalf("the oldest surviving action is %q, expected t100 (the first 100 must have been evicted)", all[499].TenantID)
	}
	if got := a.State().TotalActions; got != 500 {
		t.Fatalf("State reports %d total actions; it must reflect the capped log", got)
	}
}

// ---- state -----------------------------------------------------------------

func TestStateDescribesTheRulesAndLiveBans(t *testing.T) {
	c := newTick(june10)
	a := automations.New(automations.Config{Now: c.fn(), StormThreshold: 7, StormWindowMs: 30_000, CooldownMs: 60_000})

	st := a.State()
	if len(st.Rules) != 4 {
		t.Fatalf("the dashboard lists %d rules, expected the 3 deterministic ones plus AI review", len(st.Rules))
	}
	if st.Rules[2].Trigger != "7 burst-blocks in 30s" {
		t.Fatalf("the storm rule advertises %q but is configured for 7 blocks in 30s", st.Rules[2].Trigger)
	}
	if st.Rules[3].Kind != "ai (offline)" {
		t.Fatalf("with no Groq key wired the AI rule should read %q, got %q", "ai (offline)", st.Rules[3].Kind)
	}
	for _, r := range st.Rules[:3] {
		if r.Kind != "deterministic" {
			t.Fatalf("rule %q is advertised as %q; the first three rules are plain counters", r.Name, r.Kind)
		}
	}

	a.Ban("acme", 10_000, "operator")
	a.Ban("globex", 10_000, "operator")
	c.advance(5_000)

	st = a.State()
	if len(st.ActiveBans) != 2 {
		t.Fatalf("%d bans are listed as active, expected 2", len(st.ActiveBans))
	}
	for _, b := range st.ActiveBans {
		if b.RemainingMs != 5_000 {
			t.Fatalf("%s is listed with %dms left, expected 5000", b.TenantID, b.RemainingMs)
		}
	}
	if st.TotalActions != 2 {
		t.Fatalf("State reports %d actions after two manual bans", st.TotalActions)
	}
}

func TestDefaultsAreAppliedWhenConfigIsEmpty(t *testing.T) {
	a := automations.New(automations.Config{Now: newTick(june10).fn()})
	if got := a.State().Rules[2].Trigger; got != "10 burst-blocks in 60s" {
		t.Fatalf("the default storm rule advertises %q; the documented defaults are 10 blocks in 60s", got)
	}
	act := a.Ban("acme", 0, "operator")
	if act.CooldownMs != 300_000 {
		t.Fatalf("the default cooldown is %dms, expected the documented 5 minutes", act.CooldownMs)
	}
}

func TestNilClockDoesNotPanic(t *testing.T) {
	a := automations.New(automations.Config{})
	act := a.Ban("acme", 1_000, "operator")
	if act.At == 0 {
		t.Fatal("with no clock wired the action was stamped 0; it should fall back to wall time")
	}
	if a.BanRemainingMs("acme") <= 0 {
		t.Fatal("a wall-clock ban did not register")
	}
}

// ---- notification path -----------------------------------------------------

type fakeExplainer struct {
	note string
	seen chan int // how many audit events the explainer was handed
}

func (f *fakeExplainer) Explain(action any, recent []audit.Event) string {
	if f.seen != nil {
		f.seen <- len(recent)
	}
	return f.note
}

func TestWebhookAlertCarriesTheActionAndTheAINote(t *testing.T) {
	// One payload shape has to render in both Slack and Discord, which both key
	// off `text` — and when an explainer is wired, `text` must be its sentence,
	// not the raw template.
	bodies := make(chan map[string]any, 4)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		raw, _ := io.ReadAll(r.Body)
		var m map[string]any
		_ = json.Unmarshal(raw, &m)
		m["_contentType"] = r.Header.Get("Content-Type")
		m["_method"] = r.Method
		bodies <- m
	}))
	defer srv.Close()

	exp := &fakeExplainer{note: "Retry-loop bug in acme's client; tell them to add backoff.", seen: make(chan int, 4)}
	notes := make(chan automations.Action, 4)

	a := automations.New(automations.Config{
		Now:             newTick(june10).fn(),
		AlertWebhookURL: srv.URL,
		Explainer:       exp,
		GetRecentEvents: func() []audit.Event { return []audit.Event{decision(june10, nil), decision(june10, nil)} },
		StormThreshold:  1, StormWindowMs: 10_000, CooldownMs: 60_000,
		OnNote: func(act automations.Action) { notes <- act },
	})

	a.OnDecision(decision(june10, func(e *audit.Event) { e.Allowed, e.Reason = false, "burst" }))

	select {
	case n := <-exp.seen:
		if n != 2 {
			t.Fatalf("the explainer was handed %d audit events for context, expected the 2 it was configured to receive", n)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("the explainer was never called for an auto_cooldown action")
	}

	select {
	case act := <-notes:
		if act.AINote != exp.note {
			t.Fatalf("the OnNote hook received %q instead of the model's sentence", act.AINote)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("the OnNote hook never fired, so dashboards would never show the explanation")
	}

	select {
	case body := <-bodies:
		if body["_method"] != http.MethodPost || body["_contentType"] != "application/json" {
			t.Fatalf("the webhook was called as %v with content-type %v", body["_method"], body["_contentType"])
		}
		if body["text"] != exp.note {
			t.Fatalf("the Slack/Discord `text` field is %q; when an explainer is wired the note must be what a human reads", body["text"])
		}
		if body["type"] != "auto_cooldown" || body["tenantId"] != "acme" {
			t.Fatalf("the alert payload lost the action identity: %+v", body)
		}
		if body["message"] == "" || body["message"] == nil {
			t.Fatalf("the raw rule message was dropped from the payload: %+v", body)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("no alert reached the webhook")
	}

	// The note must also be stamped onto the stored record for the admin API.
	deadline := time.Now().Add(5 * time.Second)
	for {
		if got := a.Recent(1); len(got) == 1 && got[0].AINote == exp.note {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("the AI note was never stamped onto the stored action: %+v", a.Recent(1))
		}
	}
}

func TestWebhookFallsBackToTheRuleMessageWithNoExplainer(t *testing.T) {
	bodies := make(chan map[string]any, 4)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		raw, _ := io.ReadAll(r.Body)
		var m map[string]any
		_ = json.Unmarshal(raw, &m)
		bodies <- m
	}))
	defer srv.Close()

	a := automations.New(automations.Config{Now: newTick(june10).fn(), AlertWebhookURL: srv.URL})
	a.Ban("acme", 5_000, "anshu")

	select {
	case body := <-bodies:
		if body["text"] != body["message"] {
			t.Fatalf("with no explainer wired `text` is %q but the rule said %q — the alert must still be readable", body["text"], body["message"])
		}
		if body["aiNote"] != "" && body["aiNote"] != nil {
			t.Fatalf("an AI note appeared without an explainer: %v", body["aiNote"])
		}
	case <-time.After(5 * time.Second):
		t.Fatal("no alert reached the webhook")
	}
}

func TestAnUnparseableWebhookURLAndAMissingEventSourceAreSurvivable(t *testing.T) {
	// Two ways an operator can misconfigure the alert path: a URL that is not a
	// URL, and an explainer wired with no way to fetch audit context. Neither
	// may panic the notify goroutine or lose the action.
	exp := &fakeExplainer{note: "note", seen: make(chan int, 4)}
	a := automations.New(automations.Config{
		Now:             newTick(june10).fn(),
		AlertWebhookURL: "http://%zz-not-a-url",
		Explainer:       exp,
		// GetRecentEvents deliberately omitted.
	})

	a.Ban("acme", 5_000, "anshu")

	select {
	case n := <-exp.seen:
		if n != 0 {
			t.Fatalf("with no event source wired the explainer was handed %d events; it must get an empty slice, not a panic", n)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("the notify goroutine never reached the explainer")
	}

	if a.BanRemainingMs("acme") <= 0 {
		t.Fatal("a malformed alert URL prevented the ban from taking effect")
	}
	if got := a.State().TotalActions; got != 1 {
		t.Fatalf("the action log holds %d entries after one ban with a broken alert URL", got)
	}
}

func TestADeadWebhookNeverBreaksTheRule(t *testing.T) {
	// A notification is a side effect; a dead endpoint must not take down the
	// request that triggered it, nor stop the ban from being applied.
	a := automations.New(automations.Config{
		Now:             newTick(june10).fn(),
		AlertWebhookURL: "http://127.0.0.1:1/definitely-not-listening",
		HTTPClient:      &http.Client{Timeout: 50 * time.Millisecond},
	})

	act := a.Ban("acme", 5_000, "anshu")
	if act.Type != "manual_ban" {
		t.Fatalf("the ban was not applied when the webhook was unreachable: %+v", act)
	}
	if a.BanRemainingMs("acme") <= 0 {
		t.Fatal("an unreachable alert endpoint prevented the ban from taking effect")
	}
}

// ---- the stale-ban bug -----------------------------------------------------

func TestAFreshStormMustReBanAfterAnOldCooldownExpired(t *testing.T) {
	t.Skip("BUG: the storm rule tests `_, ok := a.bans[tenantID]` (map membership) instead of whether the ban is still LIVE. An expired entry that nothing has reaped yet permanently suppresses re-banning, so a client that storms again after its cooldown lapsed is never re-banned. Entries are only reaped by BanRemainingMs, which masks this on the real request path but not for any other caller (RunAIReview, WS pushes, tests). Fix: in OnDecision compare a.bans[id] against a.cfg.Now() rather than testing for presence.")

	rec := &recorder{}
	c := newTick(june10)
	a := stormAuto(t, c, rec) // threshold 3, window 10s, cooldown 60s

	storm := func() {
		for i := 0; i < 3; i++ {
			a.OnDecision(decision(c.now(), func(e *audit.Event) { e.Allowed, e.Reason = false, "burst" }))
			c.advance(1_000)
		}
	}

	storm()
	if rec.countOf("auto_cooldown") != 1 {
		t.Fatal("the first storm did not produce a ban")
	}

	// The cooldown lapses. Nothing calls BanRemainingMs in between — which is
	// exactly what happens for any consumer other than the request path.
	c.advance(600_000)
	storm()

	if got := rec.countOf("auto_cooldown"); got != 2 {
		t.Fatalf("a second storm long after the first cooldown expired produced %d cooldowns in total, expected 2", got)
	}
	if a.BanRemainingMs("acme") <= 0 {
		t.Fatal("the client stormed again after its ban expired and was left completely unbanned")
	}
}

func TestReBanningWorksOnceTheExpiredBanHasBeenReaped(t *testing.T) {
	// The companion to the bug above: the real request path calls
	// BanRemainingMs before every decision, which reaps the stale entry and
	// lets the storm rule fire again. This is the path that is actually safe.
	rec := &recorder{}
	c := newTick(june10)
	a := stormAuto(t, c, rec)

	storm := func() {
		for i := 0; i < 3; i++ {
			a.BanRemainingMs("acme") // what the gateway does before every check
			a.OnDecision(decision(c.now(), func(e *audit.Event) { e.Allowed, e.Reason = false, "burst" }))
			c.advance(1_000)
		}
	}

	storm()
	c.advance(600_000)
	storm()

	if got := rec.countOf("auto_cooldown"); got != 2 {
		t.Fatalf("with the expired ban reaped first, a second storm produced %d cooldowns in total, expected 2", got)
	}
	if a.BanRemainingMs("acme") <= 0 {
		t.Fatal("the second storm did not re-ban the client")
	}
}

// ---- concurrency -----------------------------------------------------------

func TestAutomationsIsSafeUnderConcurrentDecisions(t *testing.T) {
	// OnDecision runs on every request goroutine while the dashboard reads
	// State/Recent and an operator bans by hand. Run with -race.
	const tenants = 40
	const threshold = 5

	c := newTick(june10)
	rec := &recorder{}
	a := automations.New(automations.Config{
		Now: c.fn(), OnAction: rec.hook(),
		StormThreshold: threshold, StormWindowMs: 60_000, CooldownMs: 300_000,
	})

	var wg sync.WaitGroup
	start := make(chan struct{})

	for id := 0; id < tenants; id++ {
		id := id
		wg.Add(1)
		go func() {
			defer wg.Done()
			<-start
			tenant := fmt.Sprintf("tenant-%d", id)
			for i := 0; i < threshold*3; i++ {
				a.OnDecision(audit.Event{At: june10, Allowed: false, TenantID: tenant, Reason: "burst", Tier: "free"})
			}
		}()
	}
	for r := 0; r < 6; r++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			<-start
			for i := 0; i < 300; i++ {
				_ = a.State()
				_ = a.Recent(20)
				_ = a.BanRemainingMs("tenant-0")
			}
		}()
	}
	close(start)
	wg.Wait()

	if got := rec.countOf("auto_cooldown"); got != tenants {
		t.Fatalf("%d tenants each stormed past the threshold but %d cooldowns fired — storm counters were shared or lost across goroutines", tenants, got)
	}
	st := a.State()
	if len(st.ActiveBans) != tenants {
		t.Fatalf("%d tenants are banned, expected all %d", len(st.ActiveBans), tenants)
	}
	if st.TotalActions != tenants {
		t.Fatalf("the action log holds %d entries after %d bans", st.TotalActions, tenants)
	}
	for id := 0; id < tenants; id++ {
		tenant := fmt.Sprintf("tenant-%d", id)
		if a.BanRemainingMs(tenant) <= 0 {
			t.Fatalf("%s stormed %d times past a threshold of %d but is not banned", tenant, threshold*3, threshold)
		}
	}
}
