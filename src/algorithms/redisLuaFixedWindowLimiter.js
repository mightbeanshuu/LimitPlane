const script = `
local count = redis.call("INCR", KEYS[1])
if count == 1 then
  redis.call("EXPIRE", KEYS[1], ARGV[1])
end
return count
`;

export class RedisLuaFixedWindowLimiter {
  constructor({ client }) {
    this.client = client;
  }

  async check({ key, limit, windowMs }) {
    const count = await this.client.eval(script, {
      keys: [key],
      arguments: [String(Math.ceil(windowMs / 1000))]
    });

    return {
      allowed: count <= limit,
      remaining: Math.max(limit - count, 0)
    };
  }
}
