#!/usr/bin/env node
// LimitPlane proxy — attach the layer to ANY site, whatever it's written in.
//
// Your site doesn't import anything, doesn't change a line of code. You just
// put this little gateway IN FRONT of it, like a bouncer at the door:
//
//   internet ──▶ limitplane (port 3000) ──▶ your real site (port 8080)
//
// Steps to attach (the whole thing):
//   1. Write limitplane.config.json  (copy limitplane.config.example.json)
//   2. node bin/limitplane.js --config limitplane.config.json
//   3. Send traffic to the proxy port instead of your site's port.
//
// Requests that pass the limiter are forwarded untouched (method, headers,
// body, streaming — all piped through). Blocked requests get the 429 +
// Retry-After from the layer and NEVER touch your site — that's the point:
// the expensive work is protected before it happens.

import http from "node:http";
import https from "node:https";
import { readFileSync } from "node:fs";
import { createLimitPlane } from "../src/index.js";

// ---- 1. Read the config file -----------------------------------------------
// Tiny flag parser: --config <path> --port <n> --upstream <url>
const args = process.argv.slice(2);
function flag(name) {
  const i = args.indexOf(`--${name}`);
  return i === -1 ? undefined : args[i + 1];
}

const configPath = flag("config") ?? "limitplane.config.json";
let config;
try {
  config = JSON.parse(readFileSync(configPath, "utf8"));
} catch (err) {
  console.error(`Could not read config at "${configPath}": ${err.message}`);
  console.error(`Start from the example: cp limitplane.config.example.json limitplane.config.json`);
  process.exit(1);
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

// ---- 2. Build the layer ------------------------------------------------------
const lp = createLimitPlane({ policy: config.policy });

// ---- 3. The bouncer-then-forward server -------------------------------------
const server = http.createServer(async (req, res) => {
  // Optional admin peek at the decision diary, guarded by a shared secret.
  if (req.url?.startsWith("/_limitplane/audit")) {
    if (!config.adminKey || req.headers["x-limitplane-admin"] !== config.adminKey) {
      res.statusCode = 404; // pretend it doesn't exist to strangers
      return res.end();
    }
    res.setHeader("Content-Type", "application/json");
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
