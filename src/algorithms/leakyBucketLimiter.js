export class LeakyBucketLimiter {
  constructor({ now = () => Date.now() } = {}) {
    this.now = now; // function that gives current time
    this.buckets = new Map(); // one leaking jar per key
  }

  check({ key, capacity, leakRatePerMs, cost = 1 }) {
    const currentTime = this.now(); // time right now
    let bucket = this.buckets.get(key); // find this key's jar

    if (!bucket) {
      bucket = { level: 0, lastLeak: currentTime }; // new jar starts empty
      this.buckets.set(key, bucket); // save it
    }

    const elapsed = currentTime - bucket.lastLeak; // time since last drain
    const leaked = elapsed * leakRatePerMs; // how much drained out
    bucket.level = Math.max(bucket.level - leaked, 0); // drain, never below 0
    bucket.lastLeak = currentTime; // remember we just drained

    const allowed = bucket.level + cost <= capacity; // would this overflow the jar?

    if (allowed) {
      bucket.level += cost; // pour water in
    }

    return {
      allowed, // true = let it through
      remaining: Math.max(Math.floor(capacity - bucket.level), 0) // room left before overflow
    };
  }
}
