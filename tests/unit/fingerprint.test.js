import { test } from "node:test";
import assert from "node:assert/strict";
import { createFingerprints } from "../../src/gateway/fingerprint.js";

// Feed a synthetic client and read back its label + lane.
function feed(fp, id, events) {
  for (const e of events) fp.observe({ tenantId: id, ...e });
  return fp.laneFor(id);
}

test("warming until there is enough signal", () => {
  const fp = createFingerprints();
  const lane = feed(fp, "new", [{ at: 0, route: "/", allowed: true }, { at: 100, route: "/", allowed: true }]);
  assert.equal(lane.label, "warming");
  assert.equal(lane.burstMult, 1.0);
});

test("a metronome that ignores 429s is a retry_bug", () => {
  const fp = createFingerprints();
  const events = [];
  for (let i = 0; i < 14; i++) events.push({ at: i * 200, route: "/api/scan", allowed: i % 2 !== 0 }); // steady, ~50% blocked, no backoff
  const lane = feed(fp, "bug", events);
  assert.equal(lane.label, "retry_bug");
  assert.ok(lane.burstMult < 1); // squeezed
});

test("steady, wide-ranging, fast traffic is a crawler", () => {
  const fp = createFingerprints();
  const events = [];
  for (let i = 0; i < 14; i++) events.push({ at: i * 1000, route: `/page/${i}`, allowed: true }); // every gap ~1s, all-new paths
  const lane = feed(fp, "spider", events);
  assert.equal(lane.label, "crawler");
});

test("steady but well-behaved traffic on few paths is an ai_agent", () => {
  const fp = createFingerprints();
  const events = [];
  for (let i = 0; i < 14; i++) events.push({ at: i * 800, route: "/api/run", allowed: true }); // metronome, one path, no blocks
  const lane = feed(fp, "agent", events);
  assert.equal(lane.label, "ai_agent");
  assert.ok(lane.refillMult > 1); // rewarded with steady throughput
});

test("irregular human-timed clicking is a human and gets roomier bursts", () => {
  const fp = createFingerprints();
  const gaps = [1200, 400, 3000, 250, 5000, 800, 1500, 300, 4200, 600, 2000, 900, 1800]; // jittery
  let t = 0;
  const events = gaps.map((g, i) => ({ at: (t += g), route: i % 3 === 0 ? "/a" : "/b", allowed: true }));
  const lane = feed(fp, "person", events);
  assert.equal(lane.label, "human");
  assert.ok(lane.burstMult > 1);
});

test("label changes fire the hook once, with previous label", () => {
  const changes = [];
  const fp = createFingerprints({ onLabelChange: (id, next, prev) => changes.push([id, prev, next.label]) });
  const events = [];
  for (let i = 0; i < 14; i++) events.push({ at: i * 800, route: "/api/run", allowed: true });
  feed(fp, "agent", events);
  assert.ok(changes.some(([, prev, next]) => prev === "warming" && next === "ai_agent"));
});
