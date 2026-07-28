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

## Monthly plans + Stripe billing

LimitPlane runs TWO meters per request, in order:

1. **Monthly plan meter** (`monthlyQuotaLimiter.js`) — "does your plan have
   units left this month?" Hard cap, calendar-month window (UTC), resets
   automatically on the 1st because the month id changes. Protects the
   customer's wallet: a runaway script can never generate a surprise bill.
2. **Burst bucket** (`tokenBucketLimiter.js`) — "are you going too fast right
   now?" Protects your servers. A request the plan rejected never drains
   burst tokens.

Cost classes apply to both: 1 light request = 1 unit, 1 heavy AI call = 5.
Tiers opt in by setting `monthlyQuota`; without it, only burst applies.

### The billing automation loop

```
customer clicks upgrade ─▶ Stripe Checkout (hosted; you never see cards)
                                │ payment succeeds
                                ▼
        Stripe signs + POSTs /v1/billing/webhook
                                │ HMAC signature verified (node:crypto, no SDK)
                                ▼
         tenant's tier flips in the TenantStore
                                ▼
   the very next request is limited under the new plan — no restart.
   Cancellations & failed payments flow back the same way → auto-downgrade.
```

Run it live:

```bash
STRIPE_SECRET_KEY=sk_test_... STRIPE_WEBHOOK_SECRET=whsec_... \
STRIPE_PRICE_PRO=price_... node src/server.js
```

Demo the loop with no Stripe account (endpoint auto-disables in live mode):

```bash
curl -s localhost:3000/v1/billing/plans
curl -s -X POST localhost:3000/v1/billing/simulate \
  -H "content-type: application/json" -d '{"apiKey":"demo-free-key","plan":"pro"}'
curl -s localhost:3000/v1/demo/ping -H "x-api-key: demo-free-key"   # now pro, 50k units
```

The Stripe integration (`src/billing/stripeBilling.js`) is deliberately
zero-SDK: raw HTTPS + form encoding for Checkout, one HMAC for webhook
signatures. Read it once and Stripe stops being magic. The pricing model is
the "Zapier pattern" from Stripe's own SaaS guidance: flat monthly price,
hard monthly cap, upgrade prompt at the cap. Stripe's newer Meters API
(pay-per-use invoicing) is the natural later evolution.

## Live dashboard

```bash
npm start          # then open http://localhost:3000/dashboard
```

Glass-and-neumorphism control room for the gateway: KPI tiles, a 60-second
traffic chart, one card per connected site (tier chip, monthly plan meter,
live cooldown badge), the decision feed, and the autopilot's actions with
their AI notes. Poll-based (1.5s), single self-contained HTML file
(`src/dashboard/dashboard.html`), no build step. Built-in traffic buttons
let you demo everything: light pings, heavy scans, a retry-storm that gets
auto-banned, and the free→pro upgrade flowing through the billing simulate
endpoint. The proxy serves the same file at `/_limitplane/dashboard`, with
data guarded by your `adminKey`.

## Automations: the autopilot

`src/gateway/automations.js` watches the decision stream and acts with no
human in the loop:

| Rule | Trigger | Action |
|---|---|---|
| Quota alert | tenant crosses 80% of their monthly plan | alert (once/month) |
| Upgrade nudge | tenant slams into the monthly cap 3 times | "they need a bigger plan" alert (once/month) |
| Storm cooldown | 10 burst-blocks within 60s from one client | auto-ban 5 min, auto-unban after |

The gateway consults `banRemainingMs()` before spending any meter, so a
banned client gets an instant 429 (`temporarily_blocked`) without touching
tokens. Every action is logged (`GET /v1/admin/automations`) and optionally
POSTed to `ALERT_WEBHOOK_URL` (Slack/Discord-compatible: `text` field).

**AI incident notes.** Set `GROQ_API_KEY` (run `node --env-file=.env
src/server.js`) and every autopilot action gets an LLM-written note based on
the real audit facts — "looks like a retry-loop bug, tell them to add
backoff" instead of "blocked: over limit". `GET /v1/admin/explain` does the
same on demand for the latest blocked request. Deterministic-first, per
house rules: no key (or a dead network) falls back to honest template notes,
and an explanation can never break a request. The model sees audit metadata
only — never request bodies, never secrets. See `.env.example`.

**Hot config reload (proxy).** In config-file mode, `bin/limitplane.js`
watches the file and swaps in edited policy live (bad JSON keeps the old
policy and says so). Bans survive reloads; meters reset.

## How to read this codebase (start here when you come back later)

Read in this order — each file is short and comments explain every step:

| Order | File | What it is |
|---|---|---|
| 1 | `src/algorithms/tokenBucketLimiter.js` | The muscle: a coin jar per key, refills over time |
| 2 | `src/gateway/policyEngine.js` | The brain: who + what → key, budget, price |
| 3 | `src/gateway/limitPlane.js` | The layer: glues brain to muscle, sets headers, sends 429s |
| 4 | `src/gateway/auditLog.js` | The diary: every decision written down with facts |
| 5 | `src/algorithms/monthlyQuotaLimiter.js` | The phone plan: hard monthly cap, auto-resets on the 1st |
| 6 | `src/billing/stripeBilling.js` | Plans, Stripe checkout, webhook automation (zero-SDK) |
| 7 | `src/server.js` | A real site wearing the layer + billing routes |

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
