// Stripe billing — plans, checkout, and the automation loop.
//
// The pricing model here is the "Zapier pattern" (per Stripe's own SaaS
// guidance): flat monthly price per tier, HARD monthly unit cap, and at the
// cap you block + prompt an upgrade. No surprise bills — Stripe's docs call
// out dev tools where "a runaway script generates a $4,000 bill when the
// customer expected $40". Hard caps make that impossible.
//
// THE AUTOMATION LOOP (no human in it):
//
//   customer clicks upgrade ──▶ Stripe Checkout (hosted payment page)
//                                     │ payment succeeds
//                                     ▼
//   Stripe calls POST /v1/billing/webhook  (signed with a shared secret)
//                                     │ signature verified
//                                     ▼
//   handleEvent() flips the tenant's tier in the TenantStore
//                                     ▼
//   the VERY NEXT request is limited under the new plan. Nothing restarted,
//   nobody paged. Downgrades/cancellations flow back the same way.
//
// Zero dependencies on purpose: Stripe's API is plain HTTPS + form encoding,
// and webhook signatures are one HMAC. Seeing it raw is the lesson; swapping
// in the official `stripe` npm package later changes nothing structural.

import { createHmac, timingSafeEqual } from "node:crypto";
import { readFileSync, writeFileSync } from "node:fs";

// The plan catalog: what each tier costs and what it buys, in one place.
// monthlyQuota is in UNITS (cost-class weighted: 1 light = 1, 1 heavy = 5).
export const PLANS = {
  free: { tier: "free", usdPerMonth: 0, monthlyQuota: 1_000, blurb: "Try it out" },
  pro: { tier: "pro", usdPerMonth: 19, monthlyQuota: 50_000, blurb: "For real products" },
  enterprise: { tier: "enterprise", usdPerMonth: 199, monthlyQuota: 1_000_000, blurb: "For platforms" },
};

// TenantStore — the "customer database" the automation writes to.
// It wraps the SAME `tenants` object the PolicyEngine reads, so a tier flip
// here is instantly visible to the limiter — that's the whole trick.
// Optional `file` gives crash-safe persistence (JSON on every change).
export class TenantStore {
  constructor({ tenants = {}, file } = {}) {
    this.tenants = tenants;
    this.file = file;
    if (file) {
      try {
        Object.assign(this.tenants, JSON.parse(readFileSync(file, "utf8"))); // reload saved state
      } catch {
        /* no file yet — first run */
      }
    }
  }

  get(apiKey) {
    return this.tenants[apiKey];
  }

  setTier(apiKey, tier, tenantId) {
    const existing = this.tenants[apiKey];
    this.tenants[apiKey] = { tenantId: tenantId ?? existing?.tenantId ?? apiKey, tier };
    if (this.file) writeFileSync(this.file, JSON.stringify(this.tenants, null, 2)); // persist
    return this.tenants[apiKey];
  }
}

export function createBilling({
  secretKey, // sk_test_... / sk_live_... — absent = local demo mode
  webhookSecret, // whsec_... — used to verify Stripe really sent the webhook
  tenantStore, // where tier changes land
  prices = {}, // plan name -> Stripe Price id (price_...), created in the Stripe dashboard
  fetchImpl = fetch, // injectable for tests, same trick as the clocks
} = {}) {
  if (!tenantStore) throw new Error("createBilling needs a tenantStore");

  const liveMode = Boolean(secretKey); // are we talking to real Stripe?

  // ---- Checkout: give the customer a Stripe-hosted payment page ------------
  // We never touch card numbers; Stripe hosts the form, we get a URL.
  // metadata carries WHO is upgrading so the webhook can find them later.
  async function createCheckoutSession({ apiKey, plan, successUrl, cancelUrl }) {
    if (!PLANS[plan]) throw new Error(`Unknown plan: ${plan}`);
    if (!liveMode) {
      // Demo mode: no Stripe account wired up. Say so honestly instead of
      // pretending — the simulate endpoint exists for local demos.
      return {
        simulated: true,
        message: "No STRIPE_SECRET_KEY set. Use POST /v1/billing/simulate to demo the tier flip locally.",
      };
    }

    // Stripe's API takes form-encoded bodies (not JSON) — that's just how it is.
    const body = new URLSearchParams({
      mode: "subscription",
      "line_items[0][price]": prices[plan],
      "line_items[0][quantity]": "1",
      success_url: successUrl,
      cancel_url: cancelUrl,
      "metadata[apiKey]": apiKey,
      "metadata[plan]": plan,
    });

    const res = await fetchImpl("https://api.stripe.com/v1/checkout/sessions", {
      method: "POST",
      headers: { Authorization: `Bearer ${secretKey}` },
      body,
    });
    const session = await res.json();
    if (!res.ok) throw new Error(`Stripe error: ${session.error?.message}`);
    return { url: session.url }; // send the customer here to pay
  }

  // ---- Webhook signature check ----------------------------------------------
  // Stripe signs every webhook: header "stripe-signature: t=<ts>,v1=<hmac>".
  // The hmac is HMAC-SHA256("<ts>.<raw body>", webhookSecret). If it doesn't
  // match, someone ELSE is knocking — anyone can POST to a public URL, and an
  // unverified webhook would let a stranger upgrade themselves for free.
  function verifySignature(rawBody, sigHeader) {
    if (!webhookSecret || !sigHeader) return false;
    const parts = Object.fromEntries(sigHeader.split(",").map((p) => p.split("=")));
    if (!parts.t || !parts.v1) return false;

    const expected = createHmac("sha256", webhookSecret)
      .update(`${parts.t}.${rawBody}`)
      .digest("hex");

    const a = Buffer.from(expected);
    const b = Buffer.from(parts.v1);
    return a.length === b.length && timingSafeEqual(a, b); // constant-time compare
  }

  // ---- The automation: Stripe events -> tier flips ---------------------------
  // Returns a small record of what it did, for the audit trail.
  function handleEvent(event) {
    const type = event.type;
    const obj = event.data?.object ?? {};

    if (type === "checkout.session.completed") {
      // Payment went through — promote them to what they paid for.
      const { apiKey, plan } = obj.metadata ?? {};
      if (!apiKey || !PLANS[plan]) return { handled: false, why: "missing metadata" };
      tenantStore.setTier(apiKey, PLANS[plan].tier);
      return { handled: true, action: `upgraded ${apiKey} -> ${plan}` };
    }

    if (type === "customer.subscription.deleted") {
      // Subscription cancelled/expired — back to free, automatically.
      const apiKey = obj.metadata?.apiKey;
      if (!apiKey) return { handled: false, why: "missing metadata" };
      tenantStore.setTier(apiKey, "free");
      return { handled: true, action: `downgraded ${apiKey} -> free` };
    }

    if (type === "invoice.payment_failed") {
      // Card bounced — suspend to free until they fix payment. (A gentler
      // policy would grace-period this; that's a config choice, not code.)
      const apiKey = obj.metadata?.apiKey;
      if (!apiKey) return { handled: false, why: "missing metadata" };
      tenantStore.setTier(apiKey, "free");
      return { handled: true, action: `suspended ${apiKey} -> free (payment failed)` };
    }

    return { handled: false, why: `ignored event type ${type}` }; // not ours to care about
  }

  return { liveMode, createCheckoutSession, verifySignature, handleEvent, PLANS };
}
