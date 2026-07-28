// Stats — the live counters behind the dashboard.
//
// The audit log answers "what happened?"; this answers "what's happening?"
// It eats the same decision events and keeps three running views:
//   totals     — checks / allowed / blocked since the process started
//   perTenant  — one live card per connected site (tier, counts, plan meter)
//   series     — per-second allowed/blocked buckets for the last 2 minutes,
//                which is what draws the traffic chart
// All plain counters in Maps. No database, no timers: buckets are pruned
// lazily whenever a new event lands.

export function createStats({ now = () => Date.now() } = {}) {
  const startedAt = now();
  const totals = { checks: 0, allowed: 0, blocked: 0, byReason: {} };
  const perTenant = new Map(); // tenantId -> live card data
  const buckets = new Map(); // epoch-second -> { allowed, blocked }
  const KEEP_SECONDS = 120;

  function onDecision(e) {
    totals.checks += 1;
    totals[e.allowed ? "allowed" : "blocked"] += 1;
    if (!e.allowed && e.reason) {
      totals.byReason[e.reason] = (totals.byReason[e.reason] ?? 0) + 1;
    }

    // The tenant card: latest tier wins (upgrades show up instantly).
    let t = perTenant.get(e.tenantId);
    if (!t) {
      t = { tenantId: e.tenantId, checks: 0, allowed: 0, blocked: 0 };
      perTenant.set(e.tenantId, t);
    }
    t.tier = e.tier;
    t.checks += 1;
    t[e.allowed ? "allowed" : "blocked"] += 1;
    t.lastSeen = e.at;
    if (e.monthlyUsed !== undefined) {
      // quota = used + remaining AT THAT MOMENT — no separate lookup needed.
      t.monthly = { used: e.monthlyUsed, quota: e.monthlyUsed + e.monthlyRemaining };
    }

    // The chart bucket for this second.
    const sec = Math.floor(e.at / 1000);
    let b = buckets.get(sec);
    if (!b) {
      b = { allowed: 0, blocked: 0 };
      buckets.set(sec, b);
      // Lazy prune: drop buckets older than the window we ever draw.
      for (const k of buckets.keys()) {
        if (sec - k > KEEP_SECONDS) buckets.delete(k);
      }
    }
    b[e.allowed ? "allowed" : "blocked"] += 1;
  }

  // One JSON blob the dashboard polls. `banCheck` lets the caller stamp
  // live ban status on each tenant (stats itself knows nothing about bans).
  function snapshot({ banCheck } = {}) {
    const nowSec = Math.floor(now() / 1000);
    const series = [];
    for (let s = nowSec - 59; s <= nowSec; s++) {
      const b = buckets.get(s) ?? { allowed: 0, blocked: 0 };
      series.push({ secAgo: nowSec - s, ...b });
    }

    const tenants = [...perTenant.values()]
      .sort((a, b) => b.lastSeen - a.lastSeen)
      .slice(0, 24) // a dashboard, not a database dump
      .map((t) => ({ ...t, bannedMs: banCheck ? banCheck(t.tenantId) : 0 }));

    return { startedAt, uptimeMs: now() - startedAt, totals, tenants, series };
  }

  // A removed site shouldn't haunt the dashboard with a stale card.
  function forget(tenantId) {
    return perTenant.delete(tenantId);
  }

  return { onDecision, snapshot, forget };
}
