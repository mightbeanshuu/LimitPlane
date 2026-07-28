import { test } from "node:test";
import assert from "node:assert/strict";
import { createHmac } from "node:crypto";
import { createBilling, TenantStore, PLANS } from "../../src/billing/stripeBilling.js";
import { PolicyEngine } from "../../src/gateway/policyEngine.js";

const WHSEC = "whsec_test_secret";

// Build a validly-signed webhook exactly the way Stripe does it.
function sign(rawBody, secret = WHSEC, ts = 1750000000) {
  const v1 = createHmac("sha256", secret).update(`${ts}.${rawBody}`).digest("hex");
  return `t=${ts},v1=${v1}`;
}

test("webhook signature: accepts Stripe's HMAC, rejects tampering and strangers", () => {
  const billing = createBilling({ webhookSecret: WHSEC, tenantStore: new TenantStore() });
  const body = JSON.stringify({ type: "checkout.session.completed" });

  assert.equal(billing.verifySignature(body, sign(body)), true);
  assert.equal(billing.verifySignature(body + "x", sign(body)), false); // body tampered
  assert.equal(billing.verifySignature(body, sign(body, "whsec_wrong")), false); // wrong secret
  assert.equal(billing.verifySignature(body, undefined), false); // no header at all
});

test("automation: checkout.session.completed flips the tier the limiter sees", () => {
  // The store wraps the SAME object the policy engine reads — that's the loop.
  const tenants = { "key-a": { tenantId: "acme", tier: "free" } };
  const store = new TenantStore({ tenants });
  const billing = createBilling({ tenantStore: store });
  const engine = new PolicyEngine({
    tiers: { free: { capacity: 1, refillPerSecond: 1 }, pro: { capacity: 1, refillPerSecond: 1 } },
    tenants,
  });

  assert.equal(engine.identify({ apiKey: "key-a" }).tier, "free"); // before payment

  const result = billing.handleEvent({
    type: "checkout.session.completed",
    data: { object: { metadata: { apiKey: "key-a", plan: "pro" } } },
  });

  assert.equal(result.handled, true);
  assert.equal(engine.identify({ apiKey: "key-a" }).tier, "pro"); // after — no restart
});

test("automation: cancellation and failed payment both drop the tenant to free", () => {
  const store = new TenantStore({ tenants: { "key-a": { tenantId: "acme", tier: "pro" } } });
  const billing = createBilling({ tenantStore: store });

  billing.handleEvent({
    type: "customer.subscription.deleted",
    data: { object: { metadata: { apiKey: "key-a" } } },
  });
  assert.equal(store.get("key-a").tier, "free");

  store.setTier("key-a", "pro"); // re-upgrade, then bounce the card
  billing.handleEvent({
    type: "invoice.payment_failed",
    data: { object: { metadata: { apiKey: "key-a" } } },
  });
  assert.equal(store.get("key-a").tier, "free");
});

test("events with missing metadata or unknown types are ignored, never crash", () => {
  const billing = createBilling({ tenantStore: new TenantStore() });
  assert.equal(billing.handleEvent({ type: "checkout.session.completed", data: { object: {} } }).handled, false);
  assert.equal(billing.handleEvent({ type: "some.future.event", data: { object: {} } }).handled, false);
});

test("checkout: demo mode is honest, live mode returns Stripe's payment URL", async () => {
  const store = new TenantStore();

  // No secret key -> simulated, with instructions.
  const demo = createBilling({ tenantStore: store });
  const sim = await demo.createCheckoutSession({ apiKey: "k", plan: "pro" });
  assert.equal(sim.simulated, true);

  // With a key -> calls Stripe's REST API (faked here) and returns the URL.
  let captured;
  const live = createBilling({
    secretKey: "sk_test_x",
    tenantStore: store,
    prices: { pro: "price_123" },
    fetchImpl: async (url, opts) => {
      captured = { url, body: opts.body.toString(), auth: opts.headers.Authorization };
      return { ok: true, json: async () => ({ url: "https://checkout.stripe.com/pay/cs_x" }) };
    },
  });
  const session = await live.createCheckoutSession({
    apiKey: "key-a", plan: "pro",
    successUrl: "https://x/ok", cancelUrl: "https://x/no",
  });

  assert.equal(session.url, "https://checkout.stripe.com/pay/cs_x");
  assert.equal(captured.auth, "Bearer sk_test_x");
  assert.match(captured.body, /metadata%5BapiKey%5D=key-a/); // who to upgrade on webhook
  assert.match(captured.body, /price.*price_123/);
});

test("plan catalog: quotas grow with price, free is genuinely free", () => {
  assert.equal(PLANS.free.usdPerMonth, 0);
  assert.ok(PLANS.pro.monthlyQuota > PLANS.free.monthlyQuota);
  assert.ok(PLANS.enterprise.monthlyQuota > PLANS.pro.monthlyQuota);
});
