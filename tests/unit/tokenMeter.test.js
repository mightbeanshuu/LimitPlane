import { test } from "node:test";
import assert from "node:assert/strict";
import { createTokenMeter, estimateUnits, dollarsFor } from "../../src/ai/tokenMeter.js";

test("units and dollars scale with real token counts", () => {
  assert.equal(estimateUnits(1), 1); // never free
  assert.equal(estimateUnits(250), 3); // ceil(250/100)
  assert.ok(dollarsFor(1000, 1000) > 0);
  assert.ok(dollarsFor(2000, 0) < dollarsFor(0, 2000)); // output costs more than input
});

test("offline mode meters a deterministic estimate", async () => {
  const meter = createTokenMeter({}); // no key
  const r = await meter.complete({ prompt: "hello world this is a test prompt" });
  assert.equal(r.simulated, true);
  assert.ok(r.usage.totalTokens > 0);
  assert.equal(r.units, estimateUnits(r.usage.totalTokens));
});

test("live mode reads real usage from the API response", async () => {
  const meter = createTokenMeter({
    apiKey: "gsk_x",
    fetchImpl: async () => ({
      json: async () => ({
        choices: [{ message: { content: "hi there" } }],
        usage: { prompt_tokens: 120, completion_tokens: 80, total_tokens: 200 },
      }),
    }),
  });
  const r = await meter.complete({ prompt: "anything" });
  assert.equal(r.text, "hi there");
  assert.equal(r.usage.totalTokens, 200);
  assert.equal(r.units, 2); // ceil(200/100)
  assert.ok(r.usd > 0);
});
