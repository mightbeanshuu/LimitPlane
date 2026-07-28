// Vercel serverless function: the hosted dashboard's AI voice.
// The Groq key lives in a Vercel env var, server-side only — the public
// page never sees it. Lightly rate limited per IP so the key can't be
// drained by a loop (best effort; instances are ephemeral).
const hits = new Map();

export default async function handler(req, res) {
  res.setHeader("Access-Control-Allow-Origin", "*");
  if (req.method !== "POST") return res.status(405).json({ error: "post_only" });

  const ip = (req.headers["x-forwarded-for"] ?? "?").split(",")[0];
  const now = Date.now();
  const recentHits = (hits.get(ip) ?? []).filter((t) => now - t < 60_000);
  if (recentHits.length >= 10) return res.status(429).json({ error: "rate_limited" });
  recentHits.push(now);
  hits.set(ip, recentHits);

  const { action, recent } = req.body ?? {};
  if (!process.env.GROQ_API_KEY || !action) return res.status(200).json({ note: null });

  try {
    const r = await fetch("https://api.groq.com/openai/v1/chat/completions", {
      method: "POST",
      headers: {
        Authorization: `Bearer ${process.env.GROQ_API_KEY}`,
        "Content-Type": "application/json",
      },
      body: JSON.stringify({
        model: "llama-3.3-70b-versatile",
        temperature: 0.3,
        max_tokens: 120,
        messages: [
          {
            role: "system",
            content:
              "You are the ops assistant for an API rate-limiting gateway. Given an automation action and recent decisions, write ONE short plain-English note (max 2 sentences): what happened and what the operator should do. No markdown, no preamble.",
          },
          { role: "user", content: JSON.stringify({ action, recent: (recent ?? []).slice(0, 8) }) },
        ],
      }),
    });
    const d = await r.json();
    return res.status(200).json({ note: d.choices?.[0]?.message?.content?.trim() ?? null });
  } catch {
    return res.status(200).json({ note: null }); // dashboard falls back to its template
  }
}
