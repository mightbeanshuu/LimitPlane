// Throughput benchmark for LimitPlane's Redis+Lua fixed-window limiter.
// Measures sustained limiter checks/sec (the gateway's request-processing rate).
import { createClient } from "redis";
import { RedisLuaFixedWindowLimiter } from "./src/algorithms/redisLuaFixedWindowLimiter.js";

const TOTAL = Number(process.env.TOTAL || 200000);   // total checks
const CONC = Number(process.env.CONC || 100);        // concurrent in-flight
const TENANTS = Number(process.env.TENANTS || 50);   // distinct keys (multi-tenant)

async function main() {
  const client = createClient({ url: "redis://127.0.0.1:6379" });
  await client.connect();
  const limiter = new RedisLuaFixedWindowLimiter({ client });

  // clean slate
  const keys = [];
  for (let i = 0; i < TENANTS; i++) keys.push(`bench:tenant:${i}`);
  await client.del(keys);

  // warm-up
  for (let i = 0; i < 2000; i++) {
    await limiter.check({ key: keys[i % TENANTS], limit: 1e12, windowMs: 60000 });
  }
  await client.del(keys);

  let done = 0, allowed = 0;
  const t0 = process.hrtime.bigint();

  async function worker() {
    while (true) {
      const n = done++;
      if (n >= TOTAL) return;
      const r = await limiter.check({ key: keys[n % TENANTS], limit: 1e12, windowMs: 60000 });
      if (r.allowed) allowed++;
    }
  }
  await Promise.all(Array.from({ length: CONC }, worker));

  const t1 = process.hrtime.bigint();
  const secs = Number(t1 - t0) / 1e9;
  const rps = TOTAL / secs;

  console.log(JSON.stringify({
    total_checks: TOTAL,
    concurrency: CONC,
    tenants: TENANTS,
    elapsed_s: +secs.toFixed(3),
    throughput_rps: Math.round(rps),
    allowed
  }, null, 2));

  await client.del(keys);
  await client.quit();
}
main().catch((e) => { console.error(e); process.exit(1); });
