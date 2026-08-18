// The Node baseline, kept so the Go-vs-Node comparison in README.md and
// BENCHMARKS.md is something a reader can actually re-run instead of taking on
// trust.
//
// The Node backend was deleted in commit 79c74d4 ("chore: remove the Node
// backend"). This file is a verbatim copy of the token bucket as it stood at
// its parent commit — `git show 3a2895e:src/algorithms/tokenBucketLimiter.js`
// — plus a timing harness shaped like Go's, so the two numbers measure the same
// work: one in-memory admission decision on a bucket that never runs dry.
//
//   node bench/node_baseline.mjs
//
// This is the ONLY JavaScript in the gateway's build, and it is not in it: no
// package.json, no dependencies, and nothing in cmd/ or internal/ imports it.
//
// A warning about the number it prints. V8 needs a warm-up before the JIT has
// settled, so the harness discards the first pass. It is still a single-threaded
// number, and that is the whole point of the comparison: it is the ceiling for
// an entire Node process, while the Go figure it is compared against is one
// core of eight.

// ---- verbatim from the deleted Node implementation --------------------------

class TokenBucketLimiter {
  constructor({ now = () => Date.now() } = {}) {
    this.now = now;
    this.buckets = new Map();
  }

  check({ key, capacity, refillRatePerMs, cost = 1 }) {
    const currentTime = this.now();
    let bucket = this.buckets.get(key);

    if (!bucket) {
      bucket = { tokens: capacity, lastRefill: currentTime };
      this.buckets.set(key, bucket);
    }

    const elapsed = currentTime - bucket.lastRefill;
    const refillAmount = elapsed * refillRatePerMs;
    bucket.tokens = Math.min(bucket.tokens + refillAmount, capacity);
    bucket.lastRefill = currentTime;

    const allowed = bucket.tokens >= cost;

    if (allowed) {
      bucket.tokens -= cost;
    }

    return {
      allowed,
      remaining: Math.floor(bucket.tokens),
    };
  }
}

// ---- harness ----------------------------------------------------------------

// Matches internal/limiter/bench_test.go: a capacity nothing can exhaust, so we
// time the admit path rather than the rejection path.
const HUGE_CAPACITY = 1e18;

function run(label, keys, iterations) {
  const limiter = new TokenBucketLimiter();
  let sink = 0;

  const once = () => {
    const start = process.hrtime.bigint();
    for (let i = 0; i < iterations; i++) {
      const d = limiter.check({
        key: keys[i % keys.length],
        capacity: HUGE_CAPACITY,
        refillRatePerMs: 1,
        cost: 1,
      });
      if (d.allowed) sink++; // keep the call from being optimised away
    }
    return Number(process.hrtime.bigint() - start);
  };

  once(); // warm-up: let the JIT settle before anything is recorded

  // Report the best of five, matching `-count=5` and the floor-of-runs rule
  // README.md uses for the Go side.
  let bestNs = Infinity;
  for (let r = 0; r < 5; r++) bestNs = Math.min(bestNs, once());

  const nsPerOp = bestNs / iterations;
  console.log(
    `${label.padEnd(28)} ${nsPerOp.toFixed(2).padStart(8)} ns/op   ` +
      `${(1e9 / nsPerOp / 1e6).toFixed(2).padStart(6)}M checks/sec   (sink=${sink})`,
  );
}

console.log(`node ${process.version}  ${process.platform}/${process.arch}`);
console.log("single-threaded — this is the ceiling for the whole process\n");

run("one hot key", ["tenant:pro:/v1/demo/ping"], 20_000_000);

const many = Array.from({ length: 512 }, (_, i) => `tenant${i}:pro:/v1/demo/ping`);
run("512 tenants", many, 20_000_000);
