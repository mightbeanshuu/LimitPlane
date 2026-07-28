// nsfwStub — a STAND-IN for a real NSFW classification model.
//
// LimitPlane's job is protecting expensive AI endpoints, not being one. So
// the demo needs something that LOOKS like an AI inference endpoint (JSON
// label + confidence output, like the NSFW Website Detection API problem
// statement asks for) without shipping a real model. This is a deterministic
// keyword scorer: same input, same output, every time — perfect for tests.
//
// Swapping in a real model later = replace this one function. The gateway
// never knows the difference; it only knows the route is "heavy".

const FLAGGED_TERMS = ["nsfw", "explicit", "adult-only", "xxx"]; // demo trigger words

export function classifyText(text = "") {
  const lower = String(text).toLowerCase();

  // Count how many flagged terms appear — each hit raises confidence.
  const hits = FLAGGED_TERMS.filter((term) => lower.includes(term)).length;

  // 0 hits -> 0.02 (basically clean), each hit adds 0.4, capped at 0.98
  // (a real model is never 100% sure; the stub mimics that honesty).
  const confidence = Math.min(0.02 + hits * 0.4, 0.98);

  return {
    label: confidence >= 0.5 ? "nsfw" : "safe", // the verdict
    confidence: Number(confidence.toFixed(2)), // the certainty
    model: "stub-keyword-v0", // honest about what produced this
  };
}
