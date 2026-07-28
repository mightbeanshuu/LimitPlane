# LimitPlane — Innovation Research (2026-07-29)

Honest landscape check first: **cost-class rate limiting is no longer unique.**
Kong AI Gateway, Envoy AI Gateway, APISIX, and TrueFoundry all ship token-aware
limits; Cloudflare fronts LLMs at the edge. What none of them ship in open
source, and what fits LimitPlane's learnable zero-dep DNA, are the three
features below. Scores are /100 for (uniqueness × usefulness × demo-ability ×
learning value).

---

## 1. RAG Incident Memory — "ask your gateway about its whole history" · 93/100

**The gap.** Our chatbot answers from the last 10 events. Every gateway's
observability story (Kong, Grafana dashboards) shows you *charts*; none can
answer "when did visualise last hit its cap, and what did we do about it?"
grounded in weeks of history. That's a RAG problem, and nobody applies RAG to
rate-limit audit trails.

**The build (all learnable, zero external services):**
1. **Chunk**: every audit event, autopilot action, policy change, and billing
   event becomes a small text document ("2026-07-12 14:02 visualise.vercel.app
   blocked, monthly_quota, 50k/50k used, tier pro").
2. **Embed**: two-stage, deterministic-first like everything here — start with
   a hand-rolled TF-IDF / hashed bag-of-words vector (~60 lines, teaches what
   an embedding IS), then optionally swap in a real embedding API behind the
   same interface.
3. **Store + retrieve**: cosine similarity over an in-memory matrix, persisted
   to a JSONL file. A real vector DB is a config swap later.
4. **Generate**: retrieval results go into the existing `/v1/ai/chat` system
   prompt as citations. The bot answers with dates and event ids it can point
   to — no hallucinated incidents.

**Why 93:** unique in the OSS rate-limiter space; makes the chatbot genuinely
useful for ops; and it is the single best project for LEARNING RAG because you
build every stage by hand (chunk → embed → retrieve → ground → cite) instead
of calling a framework.

---

## 2. Behavioral Fingerprinting + Adaptive Lanes — "is this a human, a bot, or an AI agent?" · 92/100

**The gap.** Industry consensus (Nordic APIs, FluxNinja) is that static limits
fail against AI-agent traffic: agents burst unpredictably but legitimately.
Adaptive Rate Limiting exists in closed enterprise products; there is no
readable OSS implementation.

**The build:**
1. **Fingerprint** each client from data we ALREADY record: inter-request
   timing variance (humans are irregular, retry loops are metronomes), path
   entropy (crawlers sweep, humans cluster), burst shape, 429-reaction time
   (does it back off after a block? bots don't).
2. **Classify** with plain scored rules first (deterministic-first): metronome
   timing + zero backoff = retry-bug; high path entropy + steady rate = crawler;
   irregular + backoff-aware = human. An ML upgrade slots behind the same
   interface later.
3. **Adaptive lanes**: instead of one bucket per tier, the classification picks
   a lane — humans get burst-friendly limits, agents get steady-throughput
   limits, retry-bugs get the existing cooldown, crawlers get a crawl-delay
   lane. Dashboard shows the live label on each visitor card.

**Why 92:** turns the autopilot from reactive (ban after 10 blocks) into
predictive; visibly cool on the dashboard ("this IP is behaving like an AI
agent"); teaches feature engineering + classification on real traffic you
generated yourself.

---

## 3. True LLM Token Metering Proxy — make "AI-aware" literal · 90/100

**The gap.** Our "heavy = 5 units" is a static price tag. The real cost driver
of AI endpoints is tokens (this is the whole pitch of Kong/Envoy AI gateways —
but theirs need Kubernetes and enterprise plumbing).

**The build:** a new proxy mode where LimitPlane sits in front of an actual LLM
API (Groq works, we have the key): it forwards the request, reads
`usage.prompt_tokens` / `completion_tokens` from the response, and charges the
tenant's monthly meter with REAL token counts (and real dollars, from a price
table). Budgets become "$5/month of actual inference" instead of "N requests".
Streaming responses teach the hard part: counting tokens as chunks fly by.

**Why 90:** it upgrades LimitPlane's core claim from framing to fact, in ~150
readable lines where the equivalent enterprise feature needs a service mesh.
Resume line: "metered real LLM token spend per tenant with hard monthly
dollar caps."

---

## Rejected on purpose
- **Semantic caching** (cache similar LLM prompts): big win but it's a cache,
  not a limiter — scope creep, and Cloudflare/Kong already own the story.
- **Predictive quota forecasting** (~85): useful but it's a chart feature;
  weaker demo and weaker learning arc than the three above.

## Suggested order
RAG memory (1) first — it builds directly on the chat + audit systems that
already exist and is the one explicitly requested. Then fingerprinting (2),
which reuses the same event stream. Token metering (3) last since it adds a
new proxy path.
