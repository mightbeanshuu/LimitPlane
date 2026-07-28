export class TokenBucketLimiter {
  constructor({ now = () => Date.now() } = {}) {
    this.now = now; // function that gives current time
    this.buckets = new Map(); // one coin jar per key
  }

  check({ key, capacity, refillRatePerMs, cost = 1 }) {
    const currentTime = this.now(); // time right now
    let bucket = this.buckets.get(key); // find this key's jar

    if (!bucket) {
      bucket = { tokens: capacity, lastRefill: currentTime }; // new jar starts full
      this.buckets.set(key, bucket); // save it
    }

    const elapsed = currentTime - bucket.lastRefill; // time since last top-up
    const refillAmount = elapsed * refillRatePerMs; // coins earned during that time
    bucket.tokens = Math.min(bucket.tokens + refillAmount, capacity); // add coins, cap at capacity
    bucket.lastRefill = currentTime; // remember we just refilled

    const allowed = bucket.tokens >= cost; // enough coins to pay?

    if (allowed) {
      bucket.tokens -= cost; // spend the coins
    }

    return {
      allowed, // true = let it through
      remaining: Math.floor(bucket.tokens) // coins left
    };
  }
}
