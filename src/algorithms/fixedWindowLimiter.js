export class FixedWindowLimiter {
  constructor({ now = () => Date.now() } = {}) {
    this.now = now; // function that gives current time
    this.windows = new Map(); // one window box per key
  }

  check({ key, limit, windowMs, cost = 1 }) {
    const currentTime = this.now(); // time right now
    let currentWindow = this.windows.get(key); // find this key's box

    const isExpired = currentWindow && currentTime - currentWindow.windowStart >= windowMs; // box too old?

    if (!currentWindow || isExpired) {
      currentWindow = { count: 0, windowStart: currentTime }; // start a fresh box
      this.windows.set(key, currentWindow); // save the fresh box
    }

    const allowed = currentWindow.count + cost <= limit; // still under the limit?

    if (allowed) {
      currentWindow.count += cost; // spend the quota
    }

    return {
      allowed, // true = let it through
      remaining: Math.max(limit - currentWindow.count, 0), // quota left
      resetAt: currentWindow.windowStart + windowMs // when box resets
    };
  }
}
