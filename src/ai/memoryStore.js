// memoryStore — RAG over the gateway's own history, from scratch.
//
// RAG = Retrieval Augmented Generation: before asking the LLM a question,
// RETRIEVE the documents that matter and paste them into the prompt, so the
// model answers from facts instead of vibes. Frameworks hide the pipeline;
// this file builds every stage by hand so you can see there's no magic:
//
//   1. CHUNK    each notable gateway event becomes one small text document
//   2. EMBED    each document becomes a bag of weighted tokens (TF-IDF)
//   3. RETRIEVE cosine similarity between the question and every document
//   4. GROUND   top matches go into the chat prompt, with ids to cite
//
// About the "embedding": real systems use neural embeddings (vectors that
// capture MEANING, so "cap exceeded" matches "quota exhausted"). TF-IDF only
// matches shared words — but it is deterministic, dependency-free, debuggable
// by reading, and honestly good on log-like text full of exact names and ids.
// House rule applies: deterministic first, neural swap-in later behind the
// same remember()/search() interface.
//
// TF-IDF in one breath: a token matters in a doc if it appears often there
// (term frequency) AND rarely elsewhere (inverse document frequency). "the"
// is everywhere -> weight ~0. "visualise.vercel.app" is rare -> heavy.

import { appendFileSync, readFileSync } from "node:fs";

export function createMemory({ file, now = () => Date.now(), maxDocs = 5000 } = {}) {
  const docs = []; // { id, at, text, meta, tf: Map(token -> count) }
  const df = new Map(); // token -> how many docs contain it (for IDF)
  let nextId = 1;

  // Tokens: words, plus the dots/slashes/colons that make hostnames, paths
  // and ips stay whole — "visualise.vercel.app" must be ONE token.
  function tokenize(text) {
    return String(text).toLowerCase().match(/[a-z0-9][a-z0-9.\/:_-]*/g) ?? [];
  }

  function termCounts(text) {
    const tf = new Map();
    const add = (t) => tf.set(t, (tf.get(t) ?? 0) + 1);
    for (const t of tokenize(text)) {
      add(t); // the exact token ("visualise.vercel.app") for precise hits...
      if (/[.\/:_-]/.test(t)) {
        // ...AND its word parts, so the question "visualise quota?" still
        // finds documents that only contain the compound forms.
        for (const part of t.split(/[.\/:_-]+/)) if (part.length > 1) add(part);
      }
    }
    return tf;
  }

  function indexDoc(doc) {
    docs.push(doc);
    for (const token of doc.tf.keys()) df.set(token, (df.get(token) ?? 0) + 1);
    if (docs.length > maxDocs) {
      // forget the oldest, and un-count its tokens so IDF stays honest
      const old = docs.shift();
      for (const token of old.tf.keys()) {
        const n = df.get(token) - 1;
        n > 0 ? df.set(token, n) : df.delete(token);
      }
    }
  }

  // ---- 1+2. CHUNK + EMBED: one event in, one indexed document out -----------
  function remember(text, meta = {}) {
    const doc = { id: nextId++, at: now(), text: String(text), meta, tf: termCounts(text) };
    indexDoc(doc);
    if (file) {
      // JSONL: one JSON per line, append-only — the humblest possible database
      appendFileSync(file, JSON.stringify({ id: doc.id, at: doc.at, text: doc.text, meta }) + "\n");
    }
    return doc.id;
  }

  // ---- 3. RETRIEVE: cosine similarity over TF-IDF weights --------------------
  function idf(token) {
    // ln(1 + N/(1+df)): unseen tokens score high, ubiquitous ones near zero
    return Math.log(1 + docs.length / (1 + (df.get(token) ?? 0)));
  }

  function search(query, k = 5) {
    const qtf = termCounts(query);
    if (qtf.size === 0 || docs.length === 0) return [];

    // query vector: tf * idf per token, plus its norm for cosine
    let qNorm = 0;
    const qw = new Map();
    for (const [t, c] of qtf) {
      const w = c * idf(t);
      qw.set(t, w);
      qNorm += w * w;
    }
    qNorm = Math.sqrt(qNorm);

    const scored = [];
    for (const doc of docs) {
      let dot = 0, dNorm = 0;
      for (const [t, c] of doc.tf) {
        const w = c * idf(t);
        dNorm += w * w;
        const q = qw.get(t);
        if (q) dot += q * w; // only shared tokens contribute
      }
      if (dot > 0) scored.push({ doc, score: dot / (Math.sqrt(dNorm) * qNorm) });
    }

    return scored
      .sort((a, b) => b.score - a.score)
      .slice(0, k)
      .map(({ doc, score }) => ({ id: doc.id, at: doc.at, text: doc.text, meta: doc.meta, score: Number(score.toFixed(3)) }));
  }

  // Reload yesterday's memory on boot (ephemeral hosts simply start fresh).
  if (file) {
    try {
      for (const line of readFileSync(file, "utf8").split("\n")) {
        if (!line.trim()) continue;
        const { id, at, text, meta } = JSON.parse(line);
        indexDoc({ id, at, text, meta, tf: termCounts(text) });
        nextId = Math.max(nextId, id + 1);
      }
    } catch {
      /* no memory file yet — first boot */
    }
  }

  return { remember, search, size: () => docs.length };
}
