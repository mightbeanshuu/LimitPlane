// tokenMeter — INNOVATION #3: make "AI-aware" literal.
//
// So far "heavy = 5 units" is a static price tag. The real cost driver of an
// AI endpoint is TOKENS. This proxies a real LLM call (Groq), reads the
// actual token usage back from the response, and charges the tenant's
// monthly meter with real units + real dollars — hard monthly dollar caps,
// not request counts.
//
//   client -> LimitPlane /v1/ai/proxy -> Groq -> usage.total_tokens
//                                              -> units = ceil(tokens / UNIT_TOKENS)
//                                              -> $ = in*inRate + out*outRate
//
// The hard part (and the lesson) is that the cost is only known AFTER the
// call. So we pre-authorize a small hold, make the call, then reconcile the
// meter with the true token count — the same pattern a gas pump uses.

// Groq llama-3.3-70b public pricing (USD per 1M tokens), Jul 2026.
const PRICE = { inPerM: 0.59, outPerM: 0.79 };
const UNIT_TOKENS = 100; // 1 LimitPlane unit = 100 tokens billed

export function estimateUnits(tokens) {
  return Math.max(1, Math.ceil(tokens / UNIT_TOKENS));
}

export function dollarsFor(inTokens, outTokens) {
  return (inTokens * PRICE.inPerM + outTokens * PRICE.outPerM) / 1_000_000;
}

export function createTokenMeter({ apiKey, model = "llama-3.3-70b-versatile", fetchImpl = fetch } = {}) {
  const live = Boolean(apiKey);

  // Call the model, return the answer PLUS the true billed cost.
  async function complete({ prompt, maxTokens = 256 }) {
    if (!live) {
      // deterministic offline estimate so the meter still demos without a key
      const inTok = Math.ceil(String(prompt).length / 4);
      const outTok = Math.min(maxTokens, 80);
      return {
        text: "(token meter demo — set GROQ_API_KEY for real inference)",
        usage: { inTokens: inTok, outTokens: outTok, totalTokens: inTok + outTok },
        units: estimateUnits(inTok + outTok),
        usd: Number(dollarsFor(inTok, outTok).toFixed(6)),
        simulated: true,
      };
    }
    const r = await fetchImpl("https://api.groq.com/openai/v1/chat/completions", {
      method: "POST",
      headers: { Authorization: `Bearer ${apiKey}`, "Content-Type": "application/json" },
      body: JSON.stringify({ model, max_tokens: maxTokens, messages: [{ role: "user", content: String(prompt).slice(0, 4000) }] }),
    });
    const d = await r.json();
    const u = d.usage ?? {};
    const inTokens = u.prompt_tokens ?? 0, outTokens = u.completion_tokens ?? 0;
    const totalTokens = u.total_tokens ?? inTokens + outTokens;
    return {
      text: d.choices?.[0]?.message?.content?.trim() ?? "",
      usage: { inTokens, outTokens, totalTokens },
      units: estimateUnits(totalTokens), // what the monthly meter is charged
      usd: Number(dollarsFor(inTokens, outTokens).toFixed(6)),
    };
  }

  return { live, complete, UNIT_TOKENS, PRICE };
}
