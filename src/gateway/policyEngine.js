// PolicyEngine — the "rulebook" of the gateway.
//
// The limiters in src/algorithms/ are dumb muscles: give them a key and a
// budget and they say yes/no. THIS file is the brain that decides, for each
// incoming request:
//   1. WHO is asking          -> tenant + tier (free / pro / enterprise)
//   2. WHAT they are asking   -> route (/v1/demo/nsfw-check vs /v1/demo/ping)
//   3. HOW EXPENSIVE it is    -> AI cost class (light / standard / heavy)
// ...and turns that into ONE limiter call: a key + a capacity + a cost.
//
// Key shape (from sessions.md design): tenant:tier:route
// Cost classes make the gateway "AI-aware": a heavy AI inference request
// (like an NSFW image/text scan) spends MORE tokens from the same bucket
// than a cheap request, so one expensive scan can't hide behind the same
// price tag as a ping.

// How many tokens each class of request costs. One heavy AI call = 5 pings.
export const COST_CLASSES = {
  light: 1, // cheap: health checks, metadata reads
  standard: 2, // normal: typical API work
  heavy: 5, // expensive: AI inference (NSFW scan, GenAI call)
};

// Deterministic default classifier: the route config TELLS us the class.
// This is a plain function on purpose — later (Session 23, Policy Copilot)
// an LLM-backed classifier can be swapped in without touching anything else.
export function classifyCost(routeConfig) {
  const costClass = routeConfig?.costClass ?? "light"; // unknown routes are cheap
  if (!(costClass in COST_CLASSES)) {
    throw new Error(`Unknown cost class: ${costClass}`); // fail loudly, never silently
  }
  return costClass;
}

export class PolicyEngine {
  constructor(policy) {
    // tiers: how big each tenant tier's coin jar is and how fast it refills.
    // e.g. { free: { capacity: 20, refillPerSecond: 1 }, ... }
    this.tiers = policy?.tiers;
    // routes: per-route settings, mainly the AI cost class.
    // e.g. { "/v1/demo/nsfw-check": { costClass: "heavy" }, "*": { costClass: "light" } }
    this.routes = policy?.routes ?? {};
    // tenants: apiKey -> { tenantId, tier }. This is the demo "user database".
    this.tenants = policy?.tenants ?? {};

    if (!this.tiers || Object.keys(this.tiers).length === 0) {
      throw new Error("PolicyEngine needs at least one tier"); // no rulebook, no gateway
    }
  }

  // Look up who owns this API key. Unknown/missing keys become an anonymous
  // free-tier tenant keyed by their IP, so strangers still get rate limited
  // (each stranger gets their OWN small jar — they don't share one).
  identify({ apiKey, ip = "unknown" }) {
    const tenant = apiKey ? this.tenants[apiKey] : undefined;
    if (tenant) return tenant; // known customer
    return { tenantId: `anon:${ip}`, tier: "free" }; // stranger -> smallest jar
  }

  // The core translation: (who, what) -> one limiter instruction.
  resolve({ tenantId, tier, route }) {
    const tierConfig = this.tiers[tier];
    if (!tierConfig) {
      throw new Error(`Unknown tier: ${tier}`); // typo in config should explode in dev
    }

    const routeConfig = this.routes[route] ?? this.routes["*"]; // exact match, else wildcard
    const costClass = classifyCost(routeConfig); // light / standard / heavy
    const cost = COST_CLASSES[costClass]; // tokens this request will spend

    return {
      key: `${tenantId}:${tier}:${route}`, // one jar per tenant+tier+route
      capacity: tierConfig.capacity, // jar size
      refillRatePerMs: tierConfig.refillPerSecond / 1000, // limiters think in ms
      cost, // tokens to spend
      costClass, // kept for headers + audit log
    };
  }
}
