// The demo rulebook — one place to see the whole multi-tenant story.
//
// Three tiers, three demo API keys, and routes priced by AI cost class.
// Numbers are small on purpose so you can FEEL the limiter in a terminal:
// a free tenant gets exactly 2 NSFW scans (2 x 5 = 10 tokens) before a 429.

export const demoPolicy = {
  tiers: {
    // capacity = jar size in tokens; refillPerSecond = how fast it comes back
    free: { capacity: 10, refillPerSecond: 1 }, // 2 heavy scans, then wait
    pro: { capacity: 50, refillPerSecond: 5 }, // 10 heavy scans
    enterprise: { capacity: 300, refillPerSecond: 30 }, // basically never blocked in a demo
  },

  routes: {
    "/v1/demo/nsfw-check": { costClass: "heavy" }, // AI inference = 5 tokens
    "/v1/demo/echo": { costClass: "standard" }, // normal work = 2 tokens
    "*": { costClass: "light" }, // everything else = 1 token
  },

  // Demo "customer database": apiKey -> who they are.
  tenants: {
    "demo-free-key": { tenantId: "acme-free", tier: "free" },
    "demo-pro-key": { tenantId: "globex-pro", tier: "pro" },
    "demo-ent-key": { tenantId: "initech-ent", tier: "enterprise" },
  },
};
