// Automations — the gateway's autopilot.
//
// The audit log WRITES DOWN what happened; this file ACTS on it, with no
// human in the loop. It watches the same decision events and runs three
// rules, each a thing an on-call engineer would otherwise do by hand:
//
//   1. QUOTA ALERT     tenant crosses 80% of their monthly plan
//                      -> warn them BEFORE they hit the wall (once/month)
//   2. UPGRADE NUDGE   tenant slams into the monthly cap 3 times
//                      -> they clearly need a bigger plan; nudge them (once/month)
//   3. STORM COOLDOWN  a client gets burst-blocked 10 times in 60 seconds
//                      -> that's a retry-loop bug or an attack, not a user;
//                         auto-ban them for 5 minutes, auto-unban after.
//                         The gateway consults isBanned() on every request.
//
// Every action is logged, and optionally POSTed to a webhook URL (Slack,
// Discord, or your own endpoint) so alerts reach a human EVEN THOUGH no
// human had to act. Deterministic and zero-dep, like everything else here:
// the clock and fetch are injectable, rules are plain counters.

export function createAutomations({
  alertWebhookUrl, // optional: where to POST action notifications
  explainer, // optional AI explainer: writes a human note on each action
  getRecentEvents = () => [], // how to fetch audit context for the explainer
  fetchImpl = fetch, // injectable for tests
  now = () => Date.now(), // injectable clock, house rule
  stormThreshold = 10, // burst blocks...
  stormWindowMs = 60_000, // ...within this window...
  cooldownMs = 300_000, // ...earn this ban (5 min)
  onAction, // optional hook: gets every action (for tests / metrics)
  onNote, // optional hook: fires when an action's AI note arrives (later, async)
} = {}) {
  const actions = []; // everything the autopilot has done, newest last
  const firedOnce = new Set(); // dedup: "tenant:month:rule" fired already
  const capHits = new Map(); // tenantId -> { month, count } monthly-cap slams
  const stormLog = new Map(); // tenantId -> [timestamps of recent burst blocks]
  const bans = new Map(); // tenantId -> banned-until timestamp

  // Which calendar month an event belongs to — used to auto-reset dedup and
  // counters, same trick as the monthly limiter (new month id = fresh start).
  function monthOf(t) {
    const d = new Date(t);
    return `${d.getUTCFullYear()}-${d.getUTCMonth()}`;
  }

  function act(action) {
    const record = { at: now(), ...action };
    actions.push(record);
    if (actions.length > 500) actions.shift(); // ring buffer, like the audit log
    if (onAction) onAction(record);

    // Notify asynchronously; a slow LLM or dead webhook must never block a
    // request. If an explainer is wired, its note rides along on the alert
    // and is stamped onto the record for /v1/admin/automations to show.
    const notify = async () => {
      if (explainer) {
        record.aiNote = await explainer.explain(record, getRecentEvents());
        if (onNote) onNote(record); // dashboards can update the entry live
      }
      if (alertWebhookUrl) {
        await fetchImpl(alertWebhookUrl, {
          method: "POST",
          headers: { "Content-Type": "application/json" },
          body: JSON.stringify({ text: record.aiNote ?? record.message, ...record }), // `text` = Slack/Discord friendly
        });
      }
    };
    if (explainer || alertWebhookUrl) notify().catch(() => {});
    return record;
  }

  // The gateway asks this on EVERY request, before spending any meter.
  // Returns 0 when not banned, else milliseconds of ban left (auto-unban:
  // when the clock passes `until`, this returns 0 and the ban just ends).
  function banRemainingMs(tenantId) {
    const until = bans.get(tenantId);
    if (!until) return 0;
    const left = until - now();
    if (left <= 0) {
      bans.delete(tenantId); // expired — clean up
      return 0;
    }
    return left;
  }

  // Feed me every audit event; I'll decide if anything needs doing.
  function onDecision(event) {
    const { tenantId, at } = event;
    if (event.reason === "auto_cooldown") return; // don't react to our own bans

    // ---- Rule 1: quota alert at 80% ----------------------------------------
    if (event.monthlyRemaining !== undefined && event.monthlyUsed !== undefined) {
      const quota = event.monthlyUsed + event.monthlyRemaining;
      const dedupKey = `${tenantId}:${monthOf(at)}:80pct`;
      if (quota > 0 && event.monthlyUsed / quota >= 0.8 && !firedOnce.has(dedupKey)) {
        firedOnce.add(dedupKey);
        act({
          type: "quota_alert",
          tenantId,
          message: `${tenantId} has used ${event.monthlyUsed}/${quota} (80%+) of this month's plan.`,
        });
      }
    }

    // ---- Rule 2: upgrade nudge after 3 monthly-cap slams --------------------
    if (!event.allowed && event.reason === "monthly_quota") {
      const month = monthOf(at);
      let rec = capHits.get(tenantId);
      if (!rec || rec.month !== month) rec = { month, count: 0 }; // new month, fresh count
      rec.count += 1;
      capHits.set(tenantId, rec);

      const dedupKey = `${tenantId}:${month}:nudge`;
      if (rec.count === 3 && !firedOnce.has(dedupKey)) {
        firedOnce.add(dedupKey);
        act({
          type: "upgrade_nudge",
          tenantId,
          message: `${tenantId} hit their monthly cap 3 times — they need a bigger plan. Send them /v1/billing/plans.`,
        });
      }
    }

    // ---- Rule 3: burst storm -> auto cooldown -------------------------------
    if (!event.allowed && event.reason === "burst") {
      const hits = stormLog.get(tenantId) ?? [];
      hits.push(at);
      // Only keep hits inside the window — old ones don't count toward a storm.
      const fresh = hits.filter((t) => at - t < stormWindowMs);
      stormLog.set(tenantId, fresh);

      if (fresh.length >= stormThreshold && !bans.has(tenantId)) {
        bans.set(tenantId, now() + cooldownMs);
        stormLog.delete(tenantId); // reset the evidence; the ban took over
        act({
          type: "auto_cooldown",
          tenantId,
          cooldownMs,
          message: `${tenantId} was burst-blocked ${stormThreshold}x in ${stormWindowMs / 1000}s (retry loop or attack). Auto-banned for ${cooldownMs / 60000} min; auto-unbans itself.`,
        });
      }
    }
  }

  // Manual overrides — the human is BACK in the loop when they want to be.
  // Same ban map the storm rule uses, so the gateway needs no new wiring.
  function ban(tenantId, ms = cooldownMs, by = "operator") {
    bans.set(tenantId, now() + ms);
    return act({ type: "manual_ban", tenantId, message: `${by} banned ${tenantId} for ${Math.round(ms / 1000)}s.` });
  }
  function unban(tenantId, by = "operator") {
    const was = bans.delete(tenantId);
    return act({ type: "manual_unban", tenantId, message: was ? `${by} lifted the cooldown on ${tenantId}.` : `${by} tried to unban ${tenantId}, but it wasn't banned.` });
  }

  return {
    onDecision,
    banRemainingMs,
    ban,
    unban,
    recent: (n = 50) => actions.slice(-n).reverse(), // newest first, like the audit log
  };
}
