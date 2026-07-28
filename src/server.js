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
import { MonthlyQuotaLimiter } from "./algorithms/monthlyQuotaLimiter.js";
import { createWsHub } from "./gateway/wsHub.js";
import { createExplainer } from "./ai/aiExplainer.js";
import { createMemory } from "./ai/memoryStore.js";
import { createFingerprints } from "./gateway/fingerprint.js";
import { parseUA } from "./gateway/userAgent.js";
import { createTokenMeter } from "./ai/tokenMeter.js";
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
  onAction: (a) => {
    memory.remember(`${when(a.at)} autopilot ${a.type}: ${a.message}`, { kind: "action", type: a.type });
    push("action", a); // autopilot panel updates live
  },
  onNote: (a) => {
    memory.remember(`${when(a.at)} AI note on ${a.type} (${a.tenantId ?? ""}): ${a.aiNote}`, { kind: "note", type: a.type });
    push("action_note", a); // the Groq note streams in when ready
  },
});
const stats = createStats(); // live counters for the dashboard

// ---- Behavioral fingerprinting -------------------------------------------------
// Classifies each client (human / ai_agent / crawler / retry_bug) from the
// timing + path signatures we already record, and hands the limiter an
// adaptive lane. Label flips are worth remembering (RAG) and pushing (UI).
const fingerprints = createFingerprints({
  onLabelChange: (tenantId, next, prev) => {
    memory.remember(`${when(Date.now())} ${tenantId} reclassified ${prev} -> ${next.label} (cv ${next.features.cv}, paths ${next.features.pathSpread}, blocked ${next.features.blockRatio})`, { kind: "fingerprint" });
    push("fingerprint", { tenantId, label: next.label, confidence: next.confidence, features: next.features });
  },
});

// ---- RAG incident memory -------------------------------------------------------
// Every NOTABLE event (blocks, autopilot actions, billing, onboarding) becomes
// a retrievable document. The chatbot searches this before answering, so it
// can cite real history instead of guessing. Allowed requests are noise and
// stay out. See src/ai/memoryStore.js for the whole RAG pipeline.
const memory = createMemory({ file: new URL("../.memory.jsonl", import.meta.url).pathname });
const tokenMeter = createTokenMeter({ apiKey: process.env.GROQ_API_KEY }); // INNOVATION #3
const when = (t) => new Date(t).toISOString().slice(0, 16).replace("T", " ");

// ---- Visitors: unique IPs, geolocated for the live map ------------------------
// Geo comes from ip-api.com (free, server-side, cached per IP). Private and
// localhost addresses get labeled instead of looked up.
const visitors = new Map(); // ip -> { ip, count, lastSeen, city, country, lat, lon, device, os, browser }
function isPrivateIp(ip) {
  return !ip || ip === "unknown" || /^(::1|::ffff:127\.|127\.|10\.|192\.168\.|172\.(1[6-9]|2\d|3[01])\.|::ffff:10\.|::ffff:192\.168\.|fe80|fc|fd)/.test(ip);
}
function trackVisitor(ip, ua) {
  if (!ip) return;
  let v = visitors.get(ip);
  if (!v) {
    v = { ip, count: 0, geo: isPrivateIp(ip) ? "local" : "pending" };
    visitors.set(ip, v);
    if (visitors.size > 500) visitors.delete(visitors.keys().next().value); // ring-ish cap
    if (v.geo === "pending") {
      fetch(`http://ip-api.com/json/${ip}?fields=status,city,country,lat,lon`)
        .then((r) => r.json())
        .then((g) => {
          if (g.status === "success") Object.assign(v, { city: g.city, country: g.country, lat: g.lat, lon: g.lon, geo: "ok" });
          else v.geo = "unknown";
        })
        .catch(() => { v.geo = "unknown"; });
    }
  }
  if (ua && !v.os) {
    const p = parseUA(ua); // exact machine: macOS / iOS / Windows + browser
    Object.assign(v, { device: p.device, os: p.os, browser: p.browser });
  }
  v.count += 1;
  v.lastSeen = Date.now();
}

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

// Shared monthly meter: the gateway AND the token-metering proxy charge the
// same per-tenant monthly budget, so LLM token spend and normal request units
// draw down one pool.
const monthlyMeter = new MonthlyQuotaLimiter();
const lp = createLimitPlane({
  policy,
  automations,
  fingerprints,
  monthly: monthlyMeter,
  onDecision: (e) => {
    stats.onDecision(e);
    trackVisitor(e.ip, e.ua); // the map learns about every client + its device/OS
    if (!e.allowed) {
      memory.remember(
        `${when(e.at)} ${e.tenantId} blocked on ${e.route}: ${e.reason} (tier ${e.tier}, cost ${e.cost}${e.monthlyUsed !== undefined ? `, monthly ${e.monthlyUsed} used` : ""}${e.ip ? `, ip ${e.ip}` : ""})`,
        { kind: "block", tenantId: e.tenantId }
      );
    }
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

// ---- AI autoban loop: give the autopilot a periodic second brain ------------
// Feeds fingerprinted repeat-blockers to Groq every 20s; it may ban clients
// the deterministic rules miss. Every AI ban is logged as type "ai_ban".
automations.enableAiReview({
  apiKey: process.env.GROQ_API_KEY,
  fetchImpl: fetch,
  getCandidates: () =>
    stats.snapshot({ banCheck: automations.banRemainingMs }).tenants.map((t) => ({
      tenantId: t.tenantId,
      label: fingerprints.get(t.tenantId)?.label ?? null,
      features: fingerprints.get(t.tenantId)?.features ?? null,
      blocked: t.blocked,
      ok: t.allowed,
    })),
});
setInterval(async () => {
  const r = await automations.runAiReview();
  if (r.banned?.length) push("stats", stats.snapshot({ banCheck: automations.banRemainingMs }));
}, 20_000);

// ---- Billing: live if env keys are set, honest demo mode if not -------------
const tenantStore = new TenantStore({
  tenants,
  file: process.env.TENANTS_FILE ?? new URL("../.tenants.json", import.meta.url).pathname, // manual adds survive restarts
});
// Fresh container (Render free tier wipes disk on deploy): re-seed the one
// production site so its beacons never go unclaimed.
if (Object.keys(tenants).length === 0) {
  tenantStore.setTier("lp_visualise_a91f3c", "pro", "visualise.vercel.app");
}
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
      ip: (req.headers["x-forwarded-for"] ?? "").split(",")[0].trim() || req.socket?.remoteAddress,
      route: path,
      ua: req.headers["user-agent"], // the visitor's real device/OS
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
    snap.tenants = snap.tenants.map((t) => ({ ...t, org: orgStore.orgOf(t.tenantId)?.name ?? null, fingerprint: fingerprints.get(t.tenantId)?.label ?? null }));
    // A visitor's behavior lives under whichever tenant their traffic hit:
    // anonymous clients are keyed anon:<ip>, keyed clients under their site.
    snap.visitors = [...visitors.values()].map((v) => ({
      ...v,
      label: (fingerprints.get(`anon:${v.ip}`) ?? fingerprints.get(v.ip))?.label ?? null,
    }));
    snap.uniqueVisitors = visitors.size;
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
  // ---- User management (platform staff) -------------------------------------
  if (route === "/v1/admin/users" && req.method === "GET") {
    if (!requireRole(req, res, ["admin", "viewer"])) return;
    // Every known account: platform staff + org members, with their orgs.
    const staff = [
      { email: "demo@limitplane.dev", platformRole: "admin", kind: "platform" },
      { email: "viewer@limitplane.dev", platformRole: "viewer", kind: "platform" },
    ];
    const orgUsers = Object.entries(orgStore.data.users).map(([email, u]) => ({
      email,
      platformRole: u.platformRole,
      kind: "member",
      orgs: orgStore.orgsFor(email).map((o) => ({ id: o.id, name: o.name, role: o.members[email] })),
    }));
    return sendJson(res, 200, { users: [...staff, ...orgUsers] });
  }
  if (route === "/v1/admin/users" && req.method === "POST") {
    if (!requireRole(req, res, ["admin"])) return;
    const body = parseJson(await readRawBody(req));
    if (!body.email || !body.password) return sendJson(res, 400, { error: "email and password required" });
    orgStore.createUser(body.email, body.password);
    return sendJson(res, 201, { email: body.email });
  }
  if (route === "/v1/admin/users/remove" && req.method === "POST") {
    if (!requireRole(req, res, ["admin"])) return;
    const body = parseJson(await readRawBody(req));
    if (!orgStore.data.users[body.email]) return sendJson(res, 404, { error: "unknown_user" });
    for (const o of orgStore.orgsFor(body.email)) orgStore.removeMember(o.id, body.email);
    delete orgStore.data.users[body.email];
    orgStore.save();
    return sendJson(res, 200, { removed: body.email });
  }

  // ---- Autopilot state: what it is and what it is doing ----------------------
  if (route === "/v1/admin/autopilot") {
    if (!requireRole(req, res, ["admin", "viewer", "user"])) return;
    return sendJson(res, 200, automations.state());
  }

  // ---- LLM token metering proxy (INNOVATION #3) ------------------------------
  // Real inference, charged to the tenant's monthly meter in REAL tokens+$.
  if (route === "/v1/ai/proxy" && req.method === "POST") {
    const body = parseJson(await readRawBody(req));
    const apiKey = req.headers["x-api-key"];
    const tenant = tenantStore.get(apiKey);
    if (!tenant) return sendJson(res, 401, { error: "unknown_api_key", message: "Connect the site first." });
    if (!body.prompt) return sendJson(res, 400, { error: "prompt required" });

    const result = await tokenMeter.complete({ prompt: body.prompt, maxTokens: body.maxTokens });
    // Charge the monthly meter with the REAL token cost (units), then record.
    const plan = policy.tiers[tenant.tier];
    let over = false;
    if (plan?.monthlyQuota !== undefined) {
      const m = monthlyMeter.check({ key: `${tenant.tenantId}:${tenant.tier}:monthly`, quota: plan.monthlyQuota, cost: result.units });
      over = !m.allowed;
      if (over) memory.remember(`${when(Date.now())} ${tenant.tenantId} token budget exhausted on /v1/ai/proxy (${result.usage.totalTokens} tokens)`, { kind: "block" });
    }
    if (over) return sendJson(res, 429, { error: "monthly_quota_exhausted", cost: result });
    return sendJson(res, 200, { reply: result.text, usage: result.usage, units: result.units, usd: result.usd });
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
    res.writeHead(200, { "Content-Type": "text/event-stream", "Cache-Control": "no-cache", Connection: "keep-alive", "Access-Control-Allow-Origin": "*" });
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
    memory.remember(`${when(Date.now())} site ${body.name} connected on ${tier} tier (org ${orgId}) by ${claims.sub}`, { kind: "admin" });
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
    memory.remember(`${when(Date.now())} site ${body.tenantId} disconnected by ${claims.sub}`, { kind: "admin" });
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
    memory.remember(`${when(Date.now())} ${body.tenantId} re-tiered to ${body.tier} by ${claims.sub}`, { kind: "admin" });
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

  // The ops chatbot: Groq answering questions grounded in LIVE gateway state.
  if (route === "/v1/ai/chat" && req.method === "POST") {
    const claims = requireRole(req, res, ["admin", "viewer", "user"]);
    if (!claims) return;
    if (!process.env.GROQ_API_KEY) return sendJson(res, 200, { reply: "AI chat is offline (no GROQ_API_KEY set on the server)." });
    const body = parseJson(await readRawBody(req));
    const history = (body.messages ?? []).slice(-8).map((m) => ({
      role: m.role === "assistant" ? "assistant" : "user",
      content: String(m.content).slice(0, 800),
    }));
    // Ground the model in what is ACTUALLY happening right now...
    const snap = stats.snapshot({ banCheck: automations.banRemainingMs });
    const context = {
      totals: snap.totals,
      uniqueVisitors: visitors.size,
      tenants: snap.tenants.map((t) => ({ id: t.tenantId, tier: t.tier, behavior: fingerprints.get(t.tenantId)?.label ?? null, ok: t.allowed, blocked: t.blocked, monthly: t.monthly, bannedMs: t.bannedMs })),
      recentDecisions: lp.audit.recent(10),
      autopilot: automations.recent(5),
      plans: PLANS,
    };
    // ...AND in what happened before: RAG retrieval over the incident memory.
    // Staff only — org users would see other orgs' incidents in there.
    const lastQuestion = history.filter((m) => m.role === "user").at(-1)?.content ?? "";
    const sources = claims.role === "user" ? [] : memory.search(lastQuestion, 6);
    try {
      const r = await fetch("https://api.groq.com/openai/v1/chat/completions", {
        method: "POST",
        headers: { Authorization: `Bearer ${process.env.GROQ_API_KEY}`, "Content-Type": "application/json" },
        body: JSON.stringify({
          model: "llama-3.3-70b-versatile",
          temperature: 0.4,
          max_tokens: 300,
          messages: [
            { role: "system", content: "You are LimitPlane's ops assistant. Answer using ONLY the live state JSON and the retrieved history documents provided. When history documents [Hn] support your answer, mention the date from them. Be concise and concrete (numbers, tenant names, reasons). If the data cannot answer, say so plainly. No markdown headers." },
            { role: "system", content: "LIVE STATE: " + JSON.stringify(context) },
            { role: "system", content: "RETRIEVED HISTORY: " + JSON.stringify(sources.map((h, i) => `[H${i + 1}] ${h.text}`)) },
            ...history,
          ],
        }),
      });
      const d = await r.json();
      return sendJson(res, 200, {
        reply: d.choices?.[0]?.message?.content?.trim() ?? "No answer.",
        sources, // the retrieved documents, so the UI can show receipts
        memorySize: memory.size(),
      });
    } catch {
      return sendJson(res, 200, { reply: "AI chat hit a network error; try again.", sources: [] });
    }
  }

  // Raw memory search, for debugging the RAG pipeline by hand.
  if (route === "/v1/admin/memory") {
    if (!requireRole(req, res, ["admin", "viewer"])) return;
    const q = new URL(req.url, "http://x").searchParams.get("q") ?? "";
    return sendJson(res, 200, { size: memory.size(), hits: memory.search(q, 10) });
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
