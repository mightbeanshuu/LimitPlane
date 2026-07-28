# LimitPlane

**An AI-aware rate-limiting layer you drop in front of any site.**

Site: **https://limitplane.vercel.app**

Modern APIs have a new problem: not all requests cost the same. A health-check
ping costs microseconds; an AI inference call (an NSFW image/text scan, a GenAI
completion) costs real GPU money. A limiter that counts them equally lets one
tenant's heavy AI traffic starve everyone. LimitPlane prices requests by **AI
cost class** and budgets them per **tenant + tier + route**.

```
            ┌─────────────────────────────────────────────┐
 request ──▶│                LIMITPLANE LAYER             │──▶ your routes
            │                                             │
            │ 1. WHO?   x-api-key → tenant + tier         │
            │ 2. WHAT?  route → cost class (light 1 /     │
            │           standard 2 / heavy 5 tokens)      │
            │ 3. CHECK  token bucket per                  │
            │           tenant:tier:route                 │
            │ 4. TELL   X-RateLimit-* headers always      │
            │ 5. BLOCK  429 + Retry-After + plain-English │
            │           reason, logged to the audit diary │
            └─────────────────────────────────────────────┘
```

## Attach to ANY site (proxy mode — one command, no code changes)

Your site can be Python, PHP, Rails, static files, anything. LimitPlane runs
as a tiny gateway in front of it; blocked requests never reach your servers.

```bash
npx github:mightbeanshuu/LimitPlane --upstream http://localhost:8080
```

That's it — every visitor now gets 60 requests/minute (keyed by IP). Tune it
with flags, no config file needed:

```bash
npx github:mightbeanshuu/LimitPlane --upstream http://localhost:8080 \
  --rpm 120 --heavy /api/ai-scan,/api/generate   # heavy routes cost 5 tokens
```

For the full multi-tenant story (tiers, API keys, per-customer budgets),
use a config file instead:

```bash
git clone https://github.com/mightbeanshuu/LimitPlane && cd LimitPlane
cp limitplane.config.example.json limitplane.config.json   # point "upstream" at your site
node bin/limitplane.js --config limitplane.config.json
# LimitPlane proxy on http://localhost:3000 → protecting http://localhost:8080
```

With `adminKey` set in the config, `GET /_limitplane/audit` (header
`x-limitplane-admin`) shows the last 50 allow/block decisions.

## Add it to a Node site (middleware mode — the whole install)

```bash
npm install github:mightbeanshuu/LimitPlane
```

```js
import { createLimitPlane } from "limitplane";

const lp = createLimitPlane({
  policy: {
    tiers: {
      free: { capacity: 10, refillPerSecond: 1 },
      pro:  { capacity: 50, refillPerSecond: 5 },
    },
    routes: {
      "/v1/scan": { costClass: "heavy" },   // AI inference: 5 tokens
      "*":        { costClass: "light" },   // everything else: 1 token
    },
    tenants: {
      "customer-api-key": { tenantId: "acme", tier: "pro" },
    },
  },
});

// Express / connect / most Node frameworks:
app.use(lp.middleware);

// Plain node:http (see src/server.js for the full demo):
const decision = await lp.middleware(req, res);
if (!decision.allowed) return; // 429 already sent
```

Unknown API keys aren't rejected — they become anonymous free-tier tenants
keyed by IP, so strangers are limited too, each in their own bucket.

## Try the demo

```bash
node src/server.js
```

```bash
# Free tier holds 10 tokens; an NSFW scan costs 5 → exactly 2 scans, then 429:
curl -s -X POST localhost:3000/v1/demo/nsfw-check \
  -H "x-api-key: demo-free-key" -H "content-type: application/json" \
  -d '{"text":"some page text to classify"}'
# → { "label": "safe", "confidence": 0.02, "model": "stub-keyword-v0" }
# 3rd call → HTTP 429, Retry-After: 5, and a message that explains itself.

curl -s localhost:3000/v1/admin/audit   # the decision diary
```

The NSFW classifier is a **deterministic keyword stub** (`src/demo/nsfwStub.js`)
— it exists so the demo has a realistically-shaped expensive AI endpoint to
protect. Swapping in a real model is a one-function change; the gateway only
knows the route is "heavy".

## How to read this codebase (start here when you come back later)

Read in this order — each file is short and comments explain every step:

| Order | File | What it is |
|---|---|---|
| 1 | `src/algorithms/tokenBucketLimiter.js` | The muscle: a coin jar per key, refills over time |
| 2 | `src/gateway/policyEngine.js` | The brain: who + what → key, budget, price |
| 3 | `src/gateway/limitPlane.js` | The layer: glues brain to muscle, sets headers, sends 429s |
| 4 | `src/gateway/auditLog.js` | The diary: every decision written down with facts |
| 5 | `src/server.js` | A real site wearing the layer (one `await lp.middleware` line) |

Six more limiter algorithms live in `src/algorithms/` (fixed window, sliding
window log/counter, leaky bucket, and two distributed Redis variants — the Lua
one does INCR+EXPIRE atomically in one round trip). Any of them can be swapped
into `createLimitPlane({ limiter })`; use a Redis limiter when you run more
than one server so all replicas share the same buckets.

## Tests & benchmark

```bash
npm test                  # unit: all algorithms + policy engine + middleware
npm run test:integration  # needs a local redis-server
node bench.js             # measured 181k limiter checks/sec (50 tenants, conc 100)
```

## Design notes

- **Injectable clocks everywhere** — every limiter and the audit log accept
  `now`, so tests fast-forward time instead of sleeping.
- **Fail loudly** — unknown tiers/cost classes throw at config time; a typo in
  the policy should explode in dev, never silently mis-limit in prod.
- **Deterministic-first AI seams** — cost classification and the NSFW stub are
  plain functions behind stable interfaces; LLM-backed versions (the planned
  Policy Copilot) are pure swap-ins, nothing breaks without an API key.
- **Heavy can't starve light** — buckets are per-route, and within a route
  heavy requests pay 5×, so cheap traffic keeps flowing while expensive AI
  traffic hits its ceiling first.
