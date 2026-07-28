// The demo gateway — a real HTTP server wearing the LimitPlane layer.
//
// Run it:            node src/server.js
// Then hammer it:    curl -s -X POST localhost:3000/v1/demo/nsfw-check \
//                      -H "x-api-key: demo-free-key" \
//                      -H "content-type: application/json" \
//                      -d '{"text":"totally normal product review"}'
// Third call in a row -> 429 with Retry-After (free tier = 2 heavy scans).
//
// Note the shape: the server knows NOTHING about rate limiting. It calls
// `await lp.middleware(req, res)` once at the top; if that returns
// allowed: false the 429 was already sent and we just stop. That one line
// is the whole "add it to your site" story.

import http from "node:http";
import { createLimitPlane } from "./gateway/limitPlane.js";
import { demoPolicy } from "./demo/policy.demo.js";
import { classifyText } from "./demo/nsfwStub.js";

const lp = createLimitPlane({ policy: demoPolicy });

// Tiny helper: read a JSON body from a raw Node request (no framework here).
function readJsonBody(req) {
  return new Promise((resolve) => {
    let raw = "";
    req.on("data", (chunk) => (raw += chunk)); // collect the pieces
    req.on("end", () => {
      try {
        resolve(JSON.parse(raw || "{}")); // parse or...
      } catch {
        resolve({}); // ...treat garbage as empty
      }
    });
  });
}

function sendJson(res, statusCode, body) {
  res.statusCode = statusCode;
  res.setHeader("Content-Type", "application/json");
  res.end(JSON.stringify(body, null, 2));
}

const server = http.createServer(async (req, res) => {
  const route = (req.url ?? "/").split("?")[0];

  // Admin peek at the diary — NOT rate limited, so you can always debug.
  if (route === "/v1/admin/audit") {
    return sendJson(res, 200, lp.audit.recent(20));
  }

  // >>> THE LAYER: one call guards every route below this line. <<<
  const decision = await lp.middleware(req, res);
  if (!decision.allowed) return; // 429 already sent by the layer

  if (route === "/v1/demo/nsfw-check" && req.method === "POST") {
    const body = await readJsonBody(req);
    const verdict = classifyText(body.text); // the "expensive AI call" (stub)
    return sendJson(res, 200, verdict);
  }

  if (route === "/v1/demo/echo" && req.method === "POST") {
    const body = await readJsonBody(req);
    return sendJson(res, 200, { echo: body });
  }

  if (route === "/v1/demo/ping") {
    return sendJson(res, 200, { pong: true, tier: decision.tier });
  }

  sendJson(res, 404, { error: "not_found" });
});

const PORT = process.env.PORT ?? 3000;
server.listen(PORT, () => {
  console.log(`LimitPlane demo gateway on http://localhost:${PORT}`);
  console.log(`Keys: demo-free-key | demo-pro-key | demo-ent-key (or none = anonymous free tier)`);
});
