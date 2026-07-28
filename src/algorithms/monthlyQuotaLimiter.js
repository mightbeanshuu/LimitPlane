// MonthlyQuotaLimiter — the "phone plan" limiter.
//
// The token bucket answers "are you going TOO FAST right now?" (seconds).
// This one answers "have you used up YOUR PLAN this month?" (billing cycle).
// Real SaaS uses both together: Zapier gives you monthly tasks (this file)
// AND stops you hammering the API in a burst (token bucket).
//
// It's a fixed window, like fixedWindowLimiter, but the window is a CALENDAR
// MONTH (UTC). No timers, no cron: the reset is automatic because the month
// id changes — first request in June sees "2026-5" != "2026-4" and starts a
// fresh meter. Usage that belongs to a dead month just gets overwritten.

export class MonthlyQuotaLimiter {
  constructor({ now = () => Date.now() } = {}) {
    this.now = now; // injectable clock, house rule
    this.meters = new Map(); // key -> { month, used }
  }

  // Which calendar month are we in? "2026-6" style id (UTC so every server
  // on Earth agrees on when the month flips).
  monthId(t = this.now()) {
    const d = new Date(t);
    return `${d.getUTCFullYear()}-${d.getUTCMonth()}`;
  }

  // When does the meter reset? First instant of next month, UTC.
  resetsAt(t = this.now()) {
    const d = new Date(t);
    return Date.UTC(d.getUTCFullYear(), d.getUTCMonth() + 1, 1);
  }

  check({ key, quota, cost = 1 }) {
    const month = this.monthId();
    let meter = this.meters.get(key);

    if (!meter || meter.month !== month) {
      meter = { month, used: 0 }; // new month = fresh meter (the auto-reset)
      this.meters.set(key, meter);
    }

    const allowed = meter.used + cost <= quota; // would this push past the plan?

    if (allowed) {
      meter.used += cost; // spend it
    }

    return {
      allowed,
      used: meter.used,
      remaining: Math.max(quota - meter.used, 0),
      resetsAt: this.resetsAt(), // client can show "resets on the 1st"
    };
  }
}
