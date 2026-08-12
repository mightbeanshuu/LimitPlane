package limiter

import (
	"math"
	"strconv"

	"github.com/mightbeanshuu/limitplane/internal/redisclient"
)

// The in-memory limiters above are correct for ONE process. Run three copies of
// the gateway behind a load balancer and each keeps its own jar, so a tenant
// gets 3x their limit. These two move the counter into Redis, where every
// gateway instance shares it.

// RedisFixedWindow — the naive distributed counter.
//
// INCR is atomic in Redis, so the count itself is never lost to a race. The
// weakness is the EXPIRE: it is a SECOND round trip, and if the process dies
// between the two, the key has no TTL and the tenant is locked out until
// somebody deletes it by hand. RedisLuaFixedWindow below closes that hole.
type RedisFixedWindow struct {
	client *redisclient.Client
}

func NewRedisFixedWindow(c *redisclient.Client) *RedisFixedWindow {
	return &RedisFixedWindow{client: c}
}

func (r *RedisFixedWindow) Check(a WindowArgs) (Decision, error) {
	count, err := r.client.Int("INCR", a.Key)
	if err != nil {
		return Decision{}, err
	}
	if count == 1 {
		ttl := int64(math.Ceil(float64(a.WindowMs) / 1000))
		if _, err := r.client.Int("EXPIRE", a.Key, strconv.FormatInt(ttl, 10)); err != nil {
			return Decision{}, err
		}
	}
	return Decision{
		Allowed:   float64(count) <= a.Limit,
		Remaining: int(math.Max(a.Limit-float64(count), 0)),
	}, nil
}

// RedisLuaFixedWindow — the same counter, made atomic.
//
// Redis runs a Lua script as a single indivisible operation, so INCR and EXPIRE
// happen together or not at all: no window can survive without its TTL, and the
// whole admission decision costs ONE round trip instead of two. This is the
// pattern the README's throughput number is built on, and the reason the
// gateway's counters live in Lua rather than in application code.
type RedisLuaFixedWindow struct {
	client *redisclient.Client
}

const luaFixedWindow = `
local count = redis.call("INCR", KEYS[1])
if count == 1 then
  redis.call("EXPIRE", KEYS[1], ARGV[1])
end
return count
`

func NewRedisLuaFixedWindow(c *redisclient.Client) *RedisLuaFixedWindow {
	return &RedisLuaFixedWindow{client: c}
}

func (r *RedisLuaFixedWindow) Check(a WindowArgs) (Decision, error) {
	ttl := int64(math.Ceil(float64(a.WindowMs) / 1000))
	count, err := r.client.Int("EVAL", luaFixedWindow, "1", a.Key, strconv.FormatInt(ttl, 10))
	if err != nil {
		return Decision{}, err
	}
	return Decision{
		Allowed:   float64(count) <= a.Limit,
		Remaining: int(math.Max(a.Limit-float64(count), 0)),
	}, nil
}

// CheckBurst adapts the Redis fixed-window limiter to the BurstLimiter
// interface. Capacity is read as the window's request limit, and WindowMs
// defaults to one minute when the caller has not set one.
func (r *RedisFixedWindow) CheckBurst(a BurstArgs) (Decision, error) {
	return r.Check(WindowArgs{Key: a.Key, Limit: a.Capacity, WindowMs: windowOr(a.WindowMs), Cost: a.Cost})
}

// CheckBurst adapts the atomic Lua variant to the BurstLimiter interface.
func (r *RedisLuaFixedWindow) CheckBurst(a BurstArgs) (Decision, error) {
	return r.Check(WindowArgs{Key: a.Key, Limit: a.Capacity, WindowMs: windowOr(a.WindowMs), Cost: a.Cost})
}

func windowOr(ms int64) int64 {
	if ms <= 0 {
		return 60_000
	}
	return ms
}

// Compile-time proof that every limiter the gateway may be handed actually
// satisfies the interface. Without these, a signature drift is only caught at
// the call site — or not at all, if nothing wires the Redis path up.
var (
	_ BurstLimiter = (*TokenBucket)(nil)
	_ BurstLimiter = (*RedisFixedWindow)(nil)
	_ BurstLimiter = (*RedisLuaFixedWindow)(nil)
)
