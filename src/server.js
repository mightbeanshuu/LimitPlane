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
import { readFileSync } from "node:fs";
import { createLimitPlane } from "./gateway/limitPlane.js";
import { createAutomations } from "./gateway/automations.js";
import { createStats } from "./gateway/stats.js";
import { createWsHub } from "./gateway/wsHub.js";
import { createExplainer } from "./ai/aiExplainer.js";
import { classifyText } from "./demo/nsfwStub.js";
import { createBilling, TenantStore, PLANS } from "./billing/stripeBilling.js";
import { createAuth, sign } from "./auth/jwt.js";
import { OrgStore } from "./gateway/orgStore.js";
import { randomBytes } from "node:crypto";

// The dashboard is one self-contained HTML file, read once at boot.
const dashboardHtml = readFileSync(new URL("./dashboard/dashboard.html", import.meta.url), "utf8");
const adminHtml = readFileSync(new URL("./dashboard/admin.html", import.meta.url), "utf8");
const logoSvg = readFileSync(new URL("../LimitPlane_Logo.svg", import.meta.url), "utf8");

// ---- Policy: tiers mirror the plan catalog so billing and limiting agree ----
// capacity/refill = burst protection (seconds); monthlyQuota = the plan (month).
// Tenants start EMPTY: sites are onboarded manually from the dashboard
// (POST /v1/admin/sites) and persisted to TENANTS_FILE across restarts.
const tenants = {};

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

// ---- The autopilot + its AI voice --------------------------------------------
// explainer: LLM-written incident notes when GROQ_API_KEY is set (run with
// `node --env-file=.env src/server.js`), honest template notes when it isn't.
const explainer = createExplainer({ apiKey: process.env.GROQ_API_KEY });
const automations = createAutomations({
  alertWebhookUrl: process.env.ALERT_WEBHOOK_URL, // e.g. a Slack webhook
  explainer,
  getRecentEvents: () => lp.audit.recent(10), // audit context for the AI
  onAction: (a) => push("action", a), // autopilot panel updates live
  onNote: (a) => push("action_note", a), // the Groq note streams in when ready
});
const stats = createStats(); // live counters for the dashboard

// ---- Real-time: Server-Sent Events ------------------------------------------
// Every decision and autopilot action is PUSHED to connected dashboards the
// millisecond it happens — no polling delay. SSE over plain node:http: one
// long-lived response per client, "event: <type>\ndata: <json>\n\n" frames,
// and the browser's EventSource reconnects by itself if the wire drops.
const sseClients = new Set();
function sseBroadcast(type, data) {
  if (sseClients.size === 0) return; // nobody watching, zero cost
  const frame = `event: ${type}\ndata: ${JSON.stringify(data)}\n\n`;
  for (const client of sseClients) client.write(frame);
}

// WebSocket hub: same events, a real two-way socket. Token rides the query
// string of the upgrade request; the JWT guard decides who gets a socket.
const wsHub = createWsHub({
  authorize: (req, url) =>
    auth.guard({ headers: { authorization: `Bearer ${url.searchParams.get("token")}` } }, ["admin", "viewer"]),
});

// One voice, two wires: everything real-time goes out over WS AND SSE.
function push(type, data) {
  wsHub.broadcast(type, data);
  sseBroadcast(type, data);
}
setInterval(() => {
  push("stats", stats.snapshot({ banCheck: automations.banRemainingMs }));
}, 2000);

const lp = createLimitPlane({
  policy,
  automations,
  onDecision: (e) => {
    stats.onDecision(e);
    push("decision", e); // live feed row, instantly
  },
});

// ---- Role-based access (JWT) --------------------------------------------------
// admin: everything. viewer: read-only dashboard data. Secret from env in
// prod; a random one per boot otherwise (restarting logs everyone out —
// correct default for a demo, never a hardcoded secret in the repo).
const JWT_SECRET = process.env.JWT_SECRET ?? randomBytes(32).toString("hex");
const auth = createAuth({
  secret: JWT_SECRET,
  users: {
    "demo@limitplane.dev": { password: process.env.DASH_ADMIN_PASSWORD ?? "demo123", role: "admin" },
    "viewer@limitplane.dev": { password: process.env.DASH_VIEWER_PASSWORD ?? "viewer123", role: "viewer" },
  },
});

// ---- Organizations: the Vercel-shaped hierarchy -------------------------------
// platform (superadmin) -> orgs -> members (owner/admin/viewer) -> sites.
// Org members log in like anyone else; their JWT role is "user" and what
// they can SEE is resolved fresh per request from the org directory.
const orgStore = new OrgStore({ file: new URL("../.orgs.json", import.meta.url).pathname });
if (!orgStore.data.orgs["org_anshu-labs"]) {
  orgStore.createOrg("Anshu Labs", "demo@limitplane.dev"); // default org, platform admin owns it
}

// Can this user manage the given org? (platform admin manages everything)
function canManageOrg(claims, orgId) {
  if (claims.role === "admin") return true;
  return ["owner", "admin"].includes(orgStore.roleIn(orgId, claims.sub));
}

// ---- Billing: live if env keys are set, honest demo mode if not -------------
const tenantStore = new TenantStore({
  tenants,
  file: process.env.TENANTS_FILE ?? new URL("../.tenants.json", import.meta.url).pathname, // manual adds survive restarts
});
// Adopt orphans AFTER the tenant file has loaded — sites connected before the
// org layer existed land in the default org instead of floating ownerless.
for (const t of Object.values(tenants)) {
  if (!orgStore.orgOf(t.tenantId)) orgStore.addSite("org_anshu-labs", t.tenantId);
}
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
  // Auth still gates everything; CORS just lets the HOSTED dashboard talk to
  // a locally running gateway (its "real live mode" bridge).
  res.setHeader("Access-Control-Allow-Origin", "*");
  res.end(JSON.stringify(body, null, 2));
}

// The role gate for protected routes: returns the JWT claims when allowed,
// or sends the 401 itself and returns null so the caller can just `return`.
function requireRole(req, res, roles) {
  const claims = auth.guard(req, roles);
  if (!claims) {
    sendJson(res, 401, { error: "unauthorized", message: `Needs a valid Bearer token with role: ${roles.join(" or ")}. POST /v1/auth/login first.` });
    return null;
  }
  return claims;
}

const server = http.createServer(async (req, res) => {
  const route = (req.url ?? "/").split("?")[0];

  // ---- Beacon: how a real deployed site reports its traffic ------------------
  // A one-line snippet on any page fires GET /b?k=<apiKey>&p=<path>. It's the
  // classic analytics-pixel trick: a simple no-cors GET, so it works from any
  // https site with zero CORS ceremony, and browsers treat localhost as a
  // trustworthy origin. Every hit runs through the REAL limiter.
  if (route === "/b") {
    const params = new URL(req.url, "http://x").searchParams;
    const path = (params.get("p") ?? "/").slice(0, 120); // real page path = real route
    const decision = await lp.check({
      apiKey: params.get("k") ?? undefined,
      ip: req.socket?.remoteAddress,
      route: path,
    });
    res.statusCode = decision.allowed ? 204 : 429;
    res.setHeader("Access-Control-Allow-Origin", "*"); // pixel responses are public anyway
    return res.end();
  }
  if (req.method === "OPTIONS") {
    res.writeHead(204, {
      "Access-Control-Allow-Origin": "*",
      "Access-Control-Allow-Methods": "GET,POST,OPTIONS",
      "Access-Control-Allow-Headers": "content-type,x-api-key,authorization",
      // Chrome sends a special preflight when a PUBLIC https site calls a
      // PRIVATE address (localhost). Without this header it silently blocks
      // the beacon — the classic "works in curl, dies in the browser" trap.
      "Access-Control-Allow-Private-Network": "true",
    });
    return res.end();
  }

  // ---- The live dashboard, the admin panel, and the logo ---------------------
  if (route === "/dashboard" || route === "/") {
    res.setHeader("Content-Type", "text/html; charset=utf-8");
    return res.end(dashboardHtml);
  }
  if (route === "/admin") {
    res.setHeader("Content-Type", "text/html; charset=utf-8");
    return res.end(adminHtml);
  }
  if (route === "/logo.svg") {
    res.setHeader("Content-Type", "image/svg+xml");
    return res.end(logoSvg);
  }
  if (route === "/v1/auth/login" && req.method === "POST") {
    const body = parseJson(await readRawBody(req));
    // Platform staff first (demo admin/viewer), then org members from the directory.
    let session = auth.login(body.email, body.password);
    if (!session && orgStore.verifyUser(body.email, body.password)) {
      session = { token: sign({ sub: body.email, role: "user" }, JWT_SECRET), role: "user", expiresInSec: 7200 };
    }
    if (!session) return sendJson(res, 401, { error: "bad_credentials" });
    const orgs = orgStore.orgsFor(body.email).map((o) => ({ id: o.id, name: o.name, role: o.members[body.email] }));
    return sendJson(res, 200, { ...session, orgs });
  }

  if (route === "/v1/admin/stats") {
    const claims = requireRole(req, res, ["admin", "viewer", "user"]);
    if (!claims) return;
    const snap = stats.snapshot({ banCheck: automations.banRemainingMs });
    // Org members see ONLY their org's sites; staff see the whole platform.
    if (claims.role === "user") {
      const visible = orgStore.visibleTenantIds(claims.sub);
      snap.tenants = snap.tenants.filter((t) => visible.has(t.tenantId));
    }
    snap.tenants = snap.tenants.map((t) => ({ ...t, org: orgStore.orgOf(t.tenantId)?.name ?? null }));
    return sendJson(res, 200, snap);
  }

  // ---- Organizations API ------------------------------------------------------
  if (route === "/v1/admin/orgs" && req.method === "GET") {
    const claims = requireRole(req, res, ["admin", "viewer", "user"]);
    if (!claims) return;
    if (claims.role === "admin" || claims.role === "viewer") {
      return sendJson(res, 200, { platform: true, orgs: orgStore.summary() });
    }
    const mine = orgStore.orgsFor(claims.sub).map((o) => ({
      id: o.id, name: o.name, members: o.members, sites: o.sites, role: o.members[claims.sub],
    }));
    return sendJson(res, 200, { platform: false, orgs: mine });
  }
  if (route === "/v1/admin/orgs" && req.method === "POST") {
    const claims = requireRole(req, res, ["admin", "user"]);
    if (!claims) return;
    const body = parseJson(await readRawBody(req));
    if (!body.name?.trim()) return sendJson(res, 400, { error: "name required" });
    const org = orgStore.createOrg(body.name.trim(), claims.sub);
    if (!org) return sendJson(res, 409, { error: "org_exists" });
    return sendJson(res, 201, org);
  }
  if (route === "/v1/admin/orgs/members" && req.method === "POST") {
    const claims = requireRole(req, res, ["admin", "user"]);
    if (!claims) return;
    const body = parseJson(await readRawBody(req));
    if (!canManageOrg(claims, body.orgId)) return sendJson(res, 403, { error: "not_your_org" });
    if (!body.email || !body.role) return sendJson(res, 400, { error: "email and role required" });
    if (!orgStore.data.users[body.email] && body.email !== "demo@limitplane.dev") {
      if (!body.password) return sendJson(res, 400, { error: "password required for a new user" });
      orgStore.createUser(body.email, body.password); // the "invite": account is born here
    }
    const org = orgStore.addMember(body.orgId, body.email, body.role);
    if (!org) return sendJson(res, 400, { error: "bad_org_or_role" });
    return sendJson(res, 200, { org: org.id, members: org.members });
  }
  if (route === "/v1/admin/orgs/members/remove" && req.method === "POST") {
    const claims = requireRole(req, res, ["admin", "user"]);
    if (!claims) return;
    const body = parseJson(await readRawBody(req));
    if (!canManageOrg(claims, body.orgId)) return sendJson(res, 403, { error: "not_your_org" });
    const ok = orgStore.removeMember(body.orgId, body.email);
    return sendJson(res, ok ? 200 : 400, ok ? { removed: body.email } : { error: "cannot_remove", message: "Unknown member, or the last owner." });
  }

  // The real-time wire. EventSource can't set headers, so the JWT rides the
  // query string; we wrap it in a pretend request for the same guard.
  if (route === "/v1/admin/live") {
    const token = new URL(req.url, "http://x").searchParams.get("token");
    if (!auth.guard({ headers: { authorization: `Bearer ${token}` } }, ["admin", "viewer"])) {
      return sendJson(res, 401, { error: "unauthorized" });
    }
    res.writeHead(200, { "Content-Type": "text/event-stream", "Cache-Control": "no-cache", Connection: "keep-alive" });
    res.write(`event: stats\ndata: ${JSON.stringify(stats.snapshot({ banCheck: automations.banRemainingMs }))}\n\n`);
    sseClients.add(res);
    req.on("close", () => sseClients.delete(res)); // tab closed = client gone
    return;
  }

  // ---- Real-time management (admin only): act on the system while it runs.
  if (route === "/v1/admin/ban" && req.method === "POST") {
    const claims = requireRole(req, res, ["admin"]);
    if (!claims) return;
    const body = parseJson(await readRawBody(req));
    if (!body.tenantId) return sendJson(res, 400, { error: "tenantId required" });
    return sendJson(res, 200, automations.ban(body.tenantId, (body.seconds ?? 300) * 1000, claims.sub));
  }
  if (route === "/v1/admin/unban" && req.method === "POST") {
    const claims = requireRole(req, res, ["admin"]);
    if (!claims) return;
    const body = parseJson(await readRawBody(req));
    if (!body.tenantId) return sendJson(res, 400, { error: "tenantId required" });
    return sendJson(res, 200, automations.unban(body.tenantId, claims.sub));
  }
  // ---- Manual site onboarding (admin): the "connect your site" flow ---------
  if (route === "/v1/admin/sites" && req.method === "POST") {
    const claims = requireRole(req, res, ["admin", "user"]);
    if (!claims) return;
    const body = parseJson(await readRawBody(req));
    if (!body.name) return sendJson(res, 400, { error: "name required", message: "e.g. visualise.vercel.app" });
    // Every site lives under an org; you can only connect into orgs you manage.
    const orgId = body.orgId ?? (claims.role === "admin" ? "org_anshu-labs" : orgStore.orgsFor(claims.sub)[0]?.id);
    if (!orgId || !canManageOrg(claims, orgId)) return sendJson(res, 403, { error: "not_your_org" });
    const tier = body.tier ?? "free";
    if (!policy.tiers[tier]) return sendJson(res, 400, { error: "unknown_tier" });
    if (Object.values(tenants).some((t) => t.tenantId === body.name)) {
      return sendJson(res, 409, { error: "already_connected" });
    }
    // The key IS the site's identity — random unless the caller brings one.
    const apiKey = body.apiKey ?? `lp_${randomBytes(9).toString("hex")}`;
    tenantStore.setTier(apiKey, tier, body.name);
    orgStore.addSite(orgId, body.name);
    return sendJson(res, 201, {
      site: body.name,
      tier,
      apiKey,
      // targetAddressSpace: Chrome gates public-site -> localhost behind a
      // Local Network Access permission; untagged requests are dropped silently.
      beacon: `<script>(function(){var u="http://localhost:${PORT}/b?k=${apiKey}&p="+encodeURIComponent(location.pathname);var f=function(o){return fetch(u,o)};try{f({mode:"no-cors",targetAddressSpace:"local"}).catch(function(){return f({mode:"no-cors",targetAddressSpace:"private"})}).catch(function(){return f({mode:"no-cors"})}).catch(function(){})}catch(e){try{f({mode:"no-cors"}).catch(function(){})}catch(e2){}}})()</script>`,
      note: "Paste the beacon into your site's pages (or a shared JS file). Every page view then flows through this gateway.",
    });
  }
  if (route === "/v1/admin/sites/remove" && req.method === "POST") {
    const claims = requireRole(req, res, ["admin", "user"]);
    if (!claims) return;
    const body = parseJson(await readRawBody(req));
    if (!body.tenantId) return sendJson(res, 400, { error: "tenantId required" });
    const owningOrg = orgStore.orgOf(body.tenantId);
    // No owning org = platform staff only; owned = that org's managers only.
    const allowed = owningOrg ? canManageOrg(claims, owningOrg.id) : claims.role === "admin";
    if (!allowed) return sendJson(res, 403, { error: "not_your_org" });
    orgStore.removeSite(body.tenantId); // out of the org
    const removed = tenantStore.removeByTenantId(body.tenantId); // keys gone
    stats.forget(body.tenantId); // card gone
    if (automations.banRemainingMs(body.tenantId) > 0) automations.unban(body.tenantId, claims.sub); // no ghost bans
    return sendJson(res, 200, { removed, tenantId: body.tenantId });
  }

  if (route === "/v1/admin/tenants" && req.method === "POST") {
    // Change a customer's tier live (the manual version of the Stripe flow).
    const claims = requireRole(req, res, ["admin"]);
    if (!claims) return;
    const body = parseJson(await readRawBody(req));
    const apiKey = Object.keys(tenants).find((k) => tenants[k].tenantId === body.tenantId);
    if (!apiKey) return sendJson(res, 404, { error: "unknown_tenant", message: "Anonymous visitors have no account to re-tier." });
    if (!policy.tiers[body.tier]) return sendJson(res, 400, { error: "unknown_tier" });
    return sendJson(res, 200, { updated: tenantStore.setTier(apiKey, body.tier), by: claims.sub });
  }
  if (route === "/v1/admin/tiers" && req.method === "POST") {
    // Edit plan limits while the gateway runs — the policy object is shared
    // with the engine, so the next request already uses the new numbers.
    const claims = requireRole(req, res, ["admin"]);
    if (!claims) return;
    const body = parseJson(await readRawBody(req));
    const tier = policy.tiers[body.tier];
    if (!tier) return sendJson(res, 400, { error: "unknown_tier" });
    for (const k of ["capacity", "refillPerSecond", "monthlyQuota"]) {
      if (body[k] !== undefined) {
        const v = Number(body[k]);
        if (!(v > 0)) return sendJson(res, 400, { error: "bad_value", field: k });
        tier[k] = v;
      }
    }
    return sendJson(res, 200, { tier: body.tier, now: tier, by: claims.sub });
  }

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
    // Flipping tiers is a MUTATION: admin role only. Viewers watch.
    if (!requireRole(req, res, ["admin"])) return;
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

  // Admin peek at the diary — org members see only their own sites' events.
  if (route === "/v1/admin/audit") {
    const claims = requireRole(req, res, ["admin", "viewer", "user"]);
    if (!claims) return;
    let events = lp.audit.recent(20);
    if (claims.role === "user") {
      const visible = orgStore.visibleTenantIds(claims.sub);
      events = events.filter((e) => visible.has(e.tenantId));
    }
    return sendJson(res, 200, events);
  }

  // What has the autopilot DONE? (alerts, nudges, cooldowns + AI notes)
  if (route === "/v1/admin/automations") {
    if (!requireRole(req, res, ["admin", "viewer"])) return;
    return sendJson(res, 200, { aiExplainer: explainer.liveMode ? "live" : "fallback", actions: automations.recent(20) });
  }

  // On-demand explanation of the latest blocked request, from real facts.
  if (route === "/v1/admin/explain") {
    if (!requireRole(req, res, ["admin", "viewer"])) return;
    const recent = lp.audit.recent(20);
    const lastBlocked = recent.find((e) => !e.allowed);
    if (!lastBlocked) return sendJson(res, 200, { explanation: "Nothing has been blocked recently." });
    const explanation = await explainer.explain(
      { type: "blocked_request", ...lastBlocked, message: `Blocked: ${lastBlocked.reason}` },
      recent
    );
    return sendJson(res, 200, { event: lastBlocked, explanation, source: explainer.liveMode ? "groq" : "fallback" });
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

// The WebSocket upgrade: /ws?token=<jwt> becomes a live two-way socket.
server.on("upgrade", (req, socket) => {
  if (!req.url?.startsWith("/ws")) return socket.destroy();
  wsHub.handleUpgrade(req, socket);
});

const PORT = process.env.PORT ?? 3000;
server.listen(PORT, () => {
  console.log(`LimitPlane demo gateway on http://localhost:${PORT}`);
  console.log(`Live dashboard: http://localhost:${PORT}/dashboard`);
  console.log(`Billing: ${billing.liveMode ? "LIVE Stripe" : "demo mode (POST /v1/billing/simulate to flip tiers)"}`);
  const connected = Object.values(tenants).map((t) => t.tenantId);
  console.log(`Connected sites: ${connected.length ? connected.join(", ") : "none yet — add one from the dashboard (admin)"}`);
});
