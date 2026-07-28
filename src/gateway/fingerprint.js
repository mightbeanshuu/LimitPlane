// Fingerprint — WHO is really knocking: a human, a crawler, an AI agent,
// or somebody's broken retry loop?
//
// Static rate limits treat every client the same. But clients LEAVE
// SIGNATURES in data we already collect, no cookies or JS needed:
//
//   timing rhythm    humans are irregular; scripts are metronomes.
//                    Measured as CV (std dev / mean) of gaps between
//                    requests — CV near 0 = machine-steady.
//   path entropy     humans dwell on a few pages; crawlers sweep
//                    everything once. Measured as unique routes / requests.
//   backoff manners  after a 429, does the client slow down? Well-built
//                    agents respect Retry-After; retry bugs hammer on.
//   block ratio      how much of their traffic are we rejecting?
//
// Classification is scored rules, deterministic-first (an ML model can swap
// in behind classify() later). The label picks an ADAPTIVE LANE — the same
// tier limits, scaled to the behavior:
//
//   human      burst-friendly (clicks come in flurries)     x1.3 burst
//   ai_agent   steady throughput, smaller bursts            x0.8 burst, x1.2 refill
//   crawler    crawl-delay lane                             x0.5 burst, x0.6 refill
//   retry_bug  starved until the autopilot cools them down  x0.5 burst, x0.5 refill
//   warming    too few requests to judge yet                x1.0

const LANES = {
  human: { burstMult: 1.3, refillMult: 1.0 },
  ai_agent: { burstMult: 0.8, refillMult: 1.2 },
  crawler: { burstMult: 0.5, refillMult: 0.6 },
  retry_bug: { burstMult: 0.5, refillMult: 0.5 },
  warming: { burstMult: 1.0, refillMult: 1.0 },
};

export function createFingerprints({ window = 30, onLabelChange } = {}) {
  const clients = new Map(); // tenantId -> { events: [{at, route, allowed}], label }

  function observe(e) {
    let c = clients.get(e.tenantId);
    if (!c) {
      c = { events: [], label: "warming" };
      clients.set(e.tenantId, c);
      if (clients.size > 1000) clients.delete(clients.keys().next().value);
    }
    c.events.push({ at: e.at, route: e.route, allowed: e.allowed });
    if (c.events.length > window) c.events.shift();

    const next = classify(c.events);
    if (next.label !== c.label) {
      const prev = c.label;
      c.label = next.label;
      if (onLabelChange) onLabelChange(e.tenantId, next, prev); // memory + alerts hook
    }
    c.last = next;
  }

  function classify(events) {
    const n = events.length;
    if (n < 6) return { label: "warming", confidence: 0, features: { n } };

    // feature: rhythm — coefficient of variation of inter-arrival gaps
    const gaps = [];
    for (let i = 1; i < n; i++) gaps.push(Math.max(events[i].at - events[i - 1].at, 1));
    const mean = gaps.reduce((a, b) => a + b, 0) / gaps.length;
    const std = Math.sqrt(gaps.reduce((a, g) => a + (g - mean) ** 2, 0) / gaps.length);
    const cv = std / mean;

    // feature: exploration — how much of their traffic is NEW ground?
    const pathSpread = new Set(events.map((e) => e.route)).size / n;

    // feature: manners — average gap right after a block vs typical gap
    let postBlockGaps = 0, postBlockCount = 0;
    for (let i = 1; i < n; i++) {
      if (!events[i - 1].allowed) {
        postBlockGaps += events[i].at - events[i - 1].at;
        postBlockCount++;
      }
    }
    const backsOff = postBlockCount > 0 && postBlockGaps / postBlockCount > mean * 2;
    const blockRatio = events.filter((e) => !e.allowed).length / n;

    const features = {
      n,
      cv: Number(cv.toFixed(2)),
      pathSpread: Number(pathSpread.toFixed(2)),
      blockRatio: Number(blockRatio.toFixed(2)),
      backsOff,
      meanGapMs: Math.round(mean),
    };

    // scored rules, most damning first
    if (blockRatio > 0.4 && cv < 0.35 && !backsOff) {
      return { label: "retry_bug", confidence: 0.9, features }; // metronome that ignores 429s
    }
    if (pathSpread > 0.7 && cv < 0.6 && mean < 5000) {
      return { label: "crawler", confidence: 0.8, features }; // sweeping, never lingering
    }
    if (cv < 0.4 && mean < 10000) {
      return { label: "ai_agent", confidence: 0.7, features }; // machine-steady but behaved
    }
    return { label: "human", confidence: 0.6, features }; // irregular = alive
  }

  // What the gateway asks on every request.
  function laneFor(tenantId) {
    const c = clients.get(tenantId);
    const label = c?.label ?? "warming";
    return { label, ...LANES[label] };
  }

  function get(tenantId) {
    const c = clients.get(tenantId);
    return c ? { label: c.label, ...(c.last ?? {}) } : null;
  }

  return { observe, laneFor, get };
}
