export class SlidingWindowLogLimiter {
  constructor({ now = () => Date.now() } = {}) {
    this.now = now; // function that gives current time
    this.logs = new Map(); // list of timestamps per key
  }

  check({ key, limit, windowMs }) {
    const currentTime = this.now(); // time right now
    const timestamps = this.logs.get(key) ?? []; // this key's saved times, or empty list

    const windowStart = currentTime - windowMs; // oldest time still "in range"
    const recent = timestamps.filter((timestamp) => timestamp > windowStart); // drop old ones

    const allowed = recent.length < limit; // still under the limit?

    if (allowed) {
      recent.push(currentTime); // remember this new request time
    }

    this.logs.set(key, recent); // save the cleaned-up list

    return {
      allowed, // true = let it through
      remaining: Math.max(limit - recent.length, 0) // quota left
    };
  }
}
