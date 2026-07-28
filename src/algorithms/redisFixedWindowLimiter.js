export class RedisFixedWindowLimiter {
  constructor({ client }) {
    this.client = client; // connected redis client, passed in from outside
  }

  async check({ key, limit, windowMs }) {
    const count = await this.client.incr(key); // atomic: read + add 1 + write, in one step

    if (count === 1) {
      await this.client.expire(key, Math.ceil(windowMs / 1000)); // only set TTL on the first request of this window
    }

    const allowed = count <= limit; // still under the limit?

    return {
      allowed, // true = let it through
      remaining: Math.max(limit - count, 0) // quota left
    };
  }
}
