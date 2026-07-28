#!/usr/bin/env node
// LimitPlane proxy — attach the layer to ANY site, whatever it's written in.
//
// Your site doesn't import anything, doesn't change a line of code. You just
// put this little gateway IN FRONT of it, like a bouncer at the door:
//
//   internet ──▶ limitplane (port 3000) ──▶ your real site (port 8080)
//
// The one-liner (no clone, no config file — defaults kick in):
//   npx github:mightbeanshuu/LimitPlane --upstream http://localhost:8080
//
// Quick tuning with flags:
//   --rpm 120              every visitor gets 120 requests/minute (default 60)
//   --heavy /api/scan,/ai  price these routes as heavy AI calls (5 tokens each)
//   --port 3000            where the proxy listens
//
// Full control (tiers, tenants, API keys) with a config file:
//   1. cp limitplane.config.example.json limitplane.config.json
//   2. node bin/limitplane.js --config limitplane.config.json
//   3. Send traffic to the proxy port instead of your site's port.
//
// Requests that pass the limiter are forwarded untouched (method, headers,
// body, streaming — all piped through). Blocked requests get the 429 +
// Retry-After from the layer and NEVER touch your site — that's the point:
// the expensive work is protected before it happens.

import http from "node:http";
import https from "node:https";
import { readFileSync, watchFile } from "node:fs";
import { createLimitPlane, createAutomations, createExplainer } from "../src/index.js";

// ---- 1. Flags and (optional) config file ------------------------------------
// Tiny flag parser: --config <path> --port <n> --upstream <url> --rpm <n> --heavy <paths>
const args = process.argv.slice(2);
function flag(name) {
  const i = args.indexOf(`--${name}`);
  return i === -1 ? undefined : args[i + 1];
}

if (args.includes("--help") || args.includes("-h")) {
  console.log(`LimitPlane — AI-aware rate-limiting proxy for any site

Usage:
  npx github:mightbeanshuu/LimitPlane --upstream http://localhost:8080

Flags:
  --upstream <url>   the site to protect (required unless in config)
  --port <n>         proxy port (default 3000)
  --rpm <n>          requests/minute per visitor, default policy only (default 60)
  --heavy <paths>    comma-separated routes priced as heavy AI calls (5 tokens)
  --config <path>    full policy file (tiers, tenants, API keys) — overrides defaults
  --alert-webhook <url>  POST autopilot alerts (quota, abuse cooldowns) here

Config-file mode hot-reloads on edit. Set GROQ_API_KEY for AI-written alert notes.
Docs: https://limitplane.vercel.app`);
  process.exit(0);
}

// The config file is OPTIONAL. No file = a sensible default policy:
// every visitor (keyed by IP) gets an --rpm budget, all routes cheap
// unless listed in --heavy. Enough to protect any site in one command.
const explicitConfigPath = flag("config"); // user asked for this exact file
const configPath = explicitConfigPath ?? "limitplane.config.json";
let config;
let configFromFile = false; // defaults mode has nothing on disk to watch
try {
  config = JSON.parse(readFileSync(configPath, "utf8"));
  configFromFile = true;
  console.log(`Using policy from ${configPath} (edits hot-reload, no restart)`);
} catch (err) {
  if (explicitConfigPath) {
    // They named a file and it's unreadable — that's an error, not a fallback.
    console.error(`Could not read config at "${configPath}": ${err.message}`);
    console.error(`Start from the example: cp limitplane.config.example.json limitplane.config.json`);
    process.exit(1);
  }

  // Default policy: rpm/60 tokens refill per second, full burst allowed.
  const rpm = Number(flag("rpm") ?? 60);
  const heavyRoutes = (flag("heavy") ?? "").split(",").filter(Boolean);
  config = {
    policy: {
      tiers: { free: { capacity: rpm, refillPerSecond: rpm / 60 } },
      routes: {
        ...Object.fromEntries(heavyRoutes.map((r) => [r, { costClass: "heavy" }])),
        "*": { costClass: "light" },
      },
      tenants: {}, // no API keys in default mode — everyone is anon-by-IP
    },
  };
  console.log(
    `No config file — using defaults: ${rpm} req/min per visitor` +
      (heavyRoutes.length ? `, heavy routes: ${heavyRoutes.join(", ")}` : "")
  );
}

// CLI flags win over the file, so quick experiments don't need edits.
const PORT = Number(flag("port") ?? config.port ?? 3000);
const UPSTREAM = flag("upstream") ?? config.upstream;

if (!UPSTREAM) {
  console.error(`Config needs "upstream": the URL of the site to protect (e.g. "http://localhost:8080")`);
  process.exit(1);
}
if (!config.policy?.tiers?.free) {
  // identify() sends strangers to the free tier — without it they'd crash.
  console.error(`policy.tiers must include a "free" tier (it's the fallback for unknown API keys)`);
  process.exit(1);
}

const upstream = new URL(UPSTREAM);
const transport = upstream.protocol === "https:" ? https : http; // pick the right pipe

// ---- 2. Build the layer (rebuildable, for hot reload) ------------------------
// `lp` is a `let` on purpose: editing the config file swaps in a fresh
// gateway below, and every in-flight request just uses whichever one exists.
let lp;

// The autopilot lives OUTSIDE the rebuild: bans and alert history survive a
// policy reload (an abuser shouldn't get unbanned because you tweaked a tier).
const automations = createAutomations({
  alertWebhookUrl: flag("alert-webhook") ?? config.alertWebhookUrl,
  explainer: createExplainer({ apiKey: process.env.GROQ_API_KEY }),
  getRecentEvents: () => (lp ? lp.audit.recent(10) : []),
});

function buildGateway(cfg) {
  return createLimitPlane({ policy: cfg.policy, automations });
}
lp = buildGateway(config);

// ---- 2b. Hot reload: edit the config file, limits change live ---------------
// Polling watch (watchFile) because it works on every OS and editor. Note the
// honest tradeoff: a reload builds fresh limiters, so burst/monthly meters
// restart — fine for policy tweaks, visible if you reload constantly.
if (configFromFile) {
  watchFile(configPath, { interval: 2000 }, () => {
    try {
      const fresh = JSON.parse(readFileSync(configPath, "utf8"));
      if (!fresh.policy?.tiers?.free) throw new Error(`policy.tiers needs a "free" tier`);
      lp = buildGateway(fresh);
      console.log(`[reload] ${configPath} changed — new policy live (meters reset)`);
    } catch (err) {
      console.error(`[reload] kept OLD policy, new one is broken: ${err.message}`);
    }
  });
}

// ---- 3. The bouncer-then-forward server -------------------------------------
const server = http.createServer(async (req, res) => {
  // Optional admin peeks (decision diary + autopilot actions), guarded by a
  // shared secret. 404 to strangers so the endpoints don't advertise themselves.
  if (req.url?.startsWith("/_limitplane/")) {
    if (!config.adminKey || req.headers["x-limitplane-admin"] !== config.adminKey) {
      res.statusCode = 404;
      return res.end();
    }
    res.setHeader("Content-Type", "application/json");
    if (req.url.startsWith("/_limitplane/automations")) {
      return res.end(JSON.stringify(automations.recent(50), null, 2));
    }
    return res.end(JSON.stringify(lp.audit.recent(50), null, 2));
  }

  // THE LAYER: allowed -> keep going; blocked -> 429 already sent, stop here.
  const decision = await lp.middleware(req, res);
  if (!decision.allowed) return;

  // Forward the request to the real site, exactly as it arrived.
  const proxyReq = transport.request(
    {
      hostname: upstream.hostname,
      port: upstream.port || (upstream.protocol === "https:" ? 443 : 80),
      path: req.url,
      method: req.method,
      headers: { ...req.headers, host: upstream.host }, // fix Host for the upstream
    },
    (proxyRes) => {
      // Copy the site's answer back out (status + headers + streamed body).
      // Our X-RateLimit-* headers set earlier survive alongside these.
      res.writeHead(proxyRes.statusCode ?? 502, proxyRes.headers);
      proxyRes.pipe(res);
    }
  );

  proxyReq.on("error", (err) => {
    // The site behind us is down or unreachable — say so honestly.
    if (!res.headersSent) {
      res.statusCode = 502;
      res.setHeader("Content-Type", "application/json");
    }
    res.end(JSON.stringify({ error: "bad_gateway", message: `Upstream unreachable: ${err.message}` }));
  });

  req.pipe(proxyReq); // stream the request body through, no buffering
});

server.listen(PORT, () => {
  console.log(`LimitPlane proxy on http://localhost:${PORT} → protecting ${UPSTREAM}`);
  console.log(`Tiers: ${Object.keys(config.policy.tiers).join(", ")} | routes priced: ${Object.keys(config.policy.routes ?? {}).join(", ") || "(all light)"}`);
});
