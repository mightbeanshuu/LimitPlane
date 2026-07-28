// AI Explainer — turns audit-log FACTS into a human sentence.
//
// This is Policy Copilot "Helper 2" from the original design: when something
// gets blocked, plain code can only say "blocked: over limit". An LLM reading
// the REAL numbers can say "47 requests in 8 seconds looks like a retry-loop
// bug, not an attack — check the client's error handling". Fresh sentence,
// specific to the situation, every time.
//
// House philosophy: DETERMINISTIC-FIRST. No API key = a decent template
// explanation, and nothing anywhere breaks. The LLM (Groq's OpenAI-compatible
// API, zero SDK, one fetch) is a pure swap-in on top. The model only ever
// receives audit metadata (tenant ids, counts, reasons) — never request
// bodies, never secrets.

const GROQ_URL = "https://api.groq.com/openai/v1/chat/completions";

export function createExplainer({
  apiKey, // GROQ_API_KEY — absent = deterministic fallback only
  model = "llama-3.3-70b-versatile", // fast + cheap, fine for one-sentence jobs
  fetchImpl = fetch, // injectable for tests
} = {}) {
  const liveMode = Boolean(apiKey);

  // The fallback: honest, useful, zero dependencies. This is what plain code
  // CAN say — reading it next to the LLM output is the whole lesson.
  function fallback(action) {
    if (action.type === "auto_cooldown") {
      return `${action.tenantId} was blocked repeatedly in under a minute and is in an automatic cooldown. Likely a retry loop or abuse.`;
    }
    if (action.type === "upgrade_nudge") {
      return `${action.tenantId} keeps hitting their monthly cap. They likely need the next plan up.`;
    }
    if (action.type === "quota_alert") {
      return `${action.tenantId} is at 80% of their monthly plan. At this pace they may run out before the reset.`;
    }
    return action.message ?? "A gateway automation fired.";
  }

  // Explain one automation action (or audit event) using recent audit
  // context. Returns a string, ALWAYS — LLM errors fall back silently,
  // because an explanation must never break the thing it explains.
  async function explain(action, recentEvents = []) {
    if (!liveMode) return fallback(action);

    // Only the facts, compactly. ~10 events is plenty of context.
    const context = recentEvents.slice(0, 10).map((e) => ({
      at: e.at,
      allowed: e.allowed,
      route: e.route,
      reason: e.reason ?? null,
      costClass: e.costClass,
      monthlyUsed: e.monthlyUsed,
    }));

    try {
      const res = await fetchImpl(GROQ_URL, {
        method: "POST",
        headers: {
          Authorization: `Bearer ${apiKey}`,
          "Content-Type": "application/json",
        },
        body: JSON.stringify({
          model,
          temperature: 0.3, // explanations should be boring and precise
          max_tokens: 120,
          messages: [
            {
              role: "system",
              content:
                "You are the ops assistant for an API rate-limiting gateway. Given an automation action and recent audit events (timestamps in ms), write ONE short plain-English explanation (max 2 sentences) of what happened and what the operator should do. Diagnose patterns: many burst blocks close together suggests a client retry-loop bug; steady monthly-cap hits suggest the customer outgrew their plan. No markdown, no preamble.",
            },
            {
              role: "user",
              content: JSON.stringify({ action, recentEvents: context }),
            },
          ],
        }),
      });
      const data = await res.json();
      const text = data.choices?.[0]?.message?.content?.trim();
      return text || fallback(action); // empty answer -> fallback, never blank
    } catch {
      return fallback(action); // network/LLM trouble must not surface
    }
  }

  return { liveMode, explain, fallback };
}
