import { test } from "node:test";
import assert from "node:assert/strict";
import { createMemory } from "../../src/ai/memoryStore.js";

const T0 = Date.UTC(2026, 6, 29);

function seeded() {
  const mem = createMemory({ now: () => T0 });
  mem.remember("visualise.vercel.app blocked on /graphs/: monthly_quota exhausted, 50000/50000 used, tier pro");
  mem.remember("anon:203.0.113.42 auto_cooldown: burst-blocked 10x in 60s on /v1/demo/nsfw-check, banned 5 min");
  mem.remember("acme-shop.com connected on the free tier by rita@acme.dev");
  mem.remember("quota_alert: bellwether crossed 80% of this month's plan");
  return mem;
}

test("retrieval ranks by shared rare tokens, not by common words", () => {
  const mem = seeded();
  const hits = mem.search("when did visualise hit its monthly quota?");
  assert.equal(hits[0].text.includes("visualise.vercel.app"), true); // the right doc wins
  assert.ok(hits[0].score > (hits[1]?.score ?? 0));

  const ipHits = mem.search("what happened with 203.0.113.42");
  assert.equal(ipHits[0].text.includes("auto_cooldown"), true); // ip is one token, heavy IDF
});

test("no shared tokens means no results, never noise", () => {
  const mem = seeded();
  assert.deepEqual(mem.search("zebra quantum pancakes"), []);
  assert.deepEqual(mem.search(""), []);
});

test("hostnames and paths survive tokenization whole", () => {
  const mem = createMemory({ now: () => T0 });
  mem.remember("beacon from visualise.vercel.app on /graphs/dfs-patterns/");
  assert.equal(mem.search("visualise.vercel.app").length, 1);
  assert.equal(mem.search("/graphs/dfs-patterns/").length, 1);
});

test("maxDocs cap forgets the oldest and keeps IDF honest", () => {
  const mem = createMemory({ now: () => T0, maxDocs: 3 });
  mem.remember("first event alpha");
  mem.remember("second event beta");
  mem.remember("third event gamma");
  mem.remember("fourth event delta"); // pushes out "first"
  assert.equal(mem.size(), 3);
  assert.deepEqual(mem.search("alpha"), []); // forgotten for real
  assert.equal(mem.search("delta").length, 1);
});
