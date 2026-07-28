// The demo gateway — a real HTTP server wearing the LimitPlane layer,
// now with plans, monthly quotas, and Stripe billing automation.
//
// Run it:            node src/server.js
// Real Stripe mode:  STRIPE_SECRET_KEY=sk_test_... STRIPE_WEBHOOK_SECRET=whsec_... \
//                    STRIPE_PRICE_PRO=price_... node src/server.js
//
// Demo the whole billing loop with no Stripe account:
//   curl -s localhost:3000/v1/billing/plans
//   curl -s -X POST localhost:3000/v1/billing/simulate \
//     -H "content-type: application/json" -d '{"apiKey":"demo-free-key","plan":"pro"}'
//   -> the very next request from demo-free-key is limited as PRO. That jump,
//      with zero restarts, is exactly what the Stripe webhook does in prod.

import http from "node:http";
import { createLimitPlane } from "./gateway/limitPlane.js";
import { classifyText } from "./demo/nsfwStub.js";
import { createBilling, TenantStore, PLANS } from "./billing/stripeBilling.js";

// ---- Policy: tiers mirror the plan catalog so billing and limiting agree ----
// capacity/refill = burst protection (seconds); monthlyQuota = the plan (month).
const tenants = {
  "demo-free-key": { tenantId: "acme-free", tier: "free" },
  "demo-pro-key": { tenantId: "globex-pro", tier: "pro" },
  "demo-ent-key": { tenantId: "initech-ent", tier: "enterprise" },
};

const policy = {
  tiers: {
    free: { capacity: 10, refillPerSecond: 1, monthlyQuota: PLANS.free.monthlyQuota },
    pro: { capacity: 50, refillPerSecond: 5, monthlyQuota: PLANS.pro.monthlyQuota },
    enterprise: { capacity: 300, refillPerSecond: 30, monthlyQuota: PLANS.enterprise.monthlyQuota },
  },
  routes: {
    "/v1/demo/nsfw-check": { costClass: "heavy" },
    "/v1/demo/echo": { costClass: "standard" },
    "*": { costClass: "light" },
  },
  tenants, // NOTE: the SAME object the TenantStore mutates — that's the automation
};

const lp = createLimitPlane({ policy });

// ---- Billing: live if env keys are set, honest demo mode if not -------------
const tenantStore = new TenantStore({ tenants, file: process.env.TENANTS_FILE });
const billing = createBilling({
  secretKey: process.env.STRIPE_SECRET_KEY,
  webhookSecret: process.env.STRIPE_WEBHOOK_SECRET,
  tenantStore,
  prices: {
    pro: process.env.STRIPE_PRICE_PRO,
    enterprise: process.env.STRIPE_PRICE_ENTERPRISE,
  },
});

// ---- Tiny helpers (no framework, so we do it by hand) ------------------------
function readRawBody(req) {
  return new Promise((resolve) => {
    let raw = "";
    req.on("data", (chunk) => (raw += chunk));
    req.on("end", () => resolve(raw));
  });
}

function parseJson(raw) {
  try {
    return JSON.parse(raw || "{}");
  } catch {
    return {};
  }
}

function sendJson(res, statusCode, body) {
  res.statusCode = statusCode;
  res.setHeader("Content-Type", "application/json");
  res.end(JSON.stringify(body, null, 2));
}

const server = http.createServer(async (req, res) => {
  const route = (req.url ?? "/").split("?")[0];

  // ---- Billing routes: NOT rate limited. Never lock a customer out of the
  // door where they pay you or the page that tells them why they're blocked.
  if (route === "/v1/billing/plans") {
    return sendJson(res, 200, PLANS);
  }

  if (route === "/v1/billing/checkout" && req.method === "POST") {
    const body = parseJson(await readRawBody(req));
    try {
      const session = await billing.createCheckoutSession({
        apiKey: req.headers["x-api-key"],
        plan: body.plan,
        successUrl: body.successUrl ?? "https://limitplane.vercel.app/?upgraded=1",
        cancelUrl: body.cancelUrl ?? "https://limitplane.vercel.app/",
      });
      return sendJson(res, 200, session);
    } catch (err) {
      return sendJson(res, 400, { error: "checkout_failed", message: err.message });
    }
  }

  if (route === "/v1/billing/webhook" && req.method === "POST") {
    // Signature FIRST — before parsing, before trusting a single byte.
    const raw = await readRawBody(req);
    if (!billing.verifySignature(raw, req.headers["stripe-signature"])) {
      return sendJson(res, 400, { error: "bad_signature" });
    }
    const outcome = billing.handleEvent(parseJson(raw));
    return sendJson(res, 200, { received: true, ...outcome });
  }

  if (route === "/v1/billing/simulate" && req.method === "POST") {
    // Local demo of the webhook automation. Disabled the moment real Stripe
    // is configured — in prod, ONLY a signed webhook may flip tiers.
    if (billing.liveMode) {
      return sendJson(res, 403, { error: "disabled_in_live_mode", message: "Use real Stripe checkout + webhooks." });
    }
    const body = parseJson(await readRawBody(req));
    const outcome = billing.handleEvent({
      type: "checkout.session.completed",
      data: { object: { metadata: { apiKey: body.apiKey, plan: body.plan } } },
    });
    return sendJson(res, outcome.handled ? 200 : 400, outcome);
  }

  // Admin peek at the diary — also outside the limiter, for debugging.
  if (route === "/v1/admin/audit") {
    return sendJson(res, 200, lp.audit.recent(20));
  }

  // >>> THE LAYER: one call guards every route below this line. <<<
  const decision = await lp.middleware(req, res);
  if (!decision.allowed) return; // 429 already sent by the layer

  if (route === "/v1/demo/nsfw-check" && req.method === "POST") {
    const body = parseJson(await readRawBody(req));
    const verdict = classifyText(body.text); // the "expensive AI call" (stub)
    return sendJson(res, 200, verdict);
  }

  if (route === "/v1/demo/echo" && req.method === "POST") {
    const body = parseJson(await readRawBody(req));
    return sendJson(res, 200, { echo: body });
  }

  if (route === "/v1/demo/ping") {
    return sendJson(res, 200, {
      pong: true,
      tier: decision.tier,
      monthly: decision.monthly, // show your plan meter on every ping
    });
  }

  sendJson(res, 404, { error: "not_found" });
});

const PORT = process.env.PORT ?? 3000;
server.listen(PORT, () => {
  console.log(`LimitPlane demo gateway on http://localhost:${PORT}`);
  console.log(`Billing: ${billing.liveMode ? "LIVE Stripe" : "demo mode (POST /v1/billing/simulate to flip tiers)"}`);
  console.log(`Keys: demo-free-key | demo-pro-key | demo-ent-key (or none = anonymous free tier)`);
});
