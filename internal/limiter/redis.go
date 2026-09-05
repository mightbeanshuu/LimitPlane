package limiter

import (
	"fmt"
	"math"
	"strconv"
	"time"

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

// RedisTokenBucket — the distributed limiter the Kubernetes deployment runs on.
//
// The two fixed-window limiters above are correct and shared, but they are the
// WRONG SHAPE for this gateway. The policy engine speaks in capacity and
// refillPerSecond — a jar that drains and trickles back — while a fixed window
// speaks in "N per minute". Running one in production and the other locally
// would mean the limits differ between `go run` and `kubectl apply`, which is
// the kind of gap nobody finds until a customer reports it. Fixed windows also
// carry the boundary burst: a client that spends its whole allowance at 11:59:59
// and again at 12:00:00 gets double the intended rate for one instant.
//
// This is the same lazy-refill token bucket as the in-memory one, evaluated
// inside Redis so that N replicas draw from ONE jar. Three pods with local
// buckets give a tenant 3x their plan; that is not a rounding error, it is the
// entire limit being wrong, and it is the first thing that breaks when a
// single-instance gateway is scaled up.
type RedisTokenBucket struct {
	client *redisclient.Client
	// idleTTL is how long an untouched jar survives in Redis. It is the
	// distributed twin of the in-memory janitor: keys include anon:<ip>, so
	// without an expiry every stranger who ever visited would occupy Redis
	// memory forever. The TTL is refreshed on every write, so an active jar
	// never expires under a client's feet.
	idleTTL time.Duration
}

func NewRedisTokenBucket(c *redisclient.Client) *RedisTokenBucket {
	return &RedisTokenBucket{client: c, idleTTL: 10 * time.Minute}
}

// luaTokenBucket is the whole admission decision, atomic, in one round trip.
//
// Two details carry the correctness:
//
//  1. The clock comes from redis.call("TIME"), NOT from the calling pod. Every
//     replica refilling against its own wall clock means clock skew turns
//     directly into free tokens: a pod running 200ms fast credits 200ms of
//     refill that never happened, on every single request. Redis is the one
//     clock all replicas already agree on, so it is the only defensible source.
//     (This makes the script non-deterministic, which is fine on Redis 5+ where
//     scripts replicate by effect rather than by re-execution.)
//
//  2. Read, refill, spend and write happen INSIDE the script. Doing it in Go
//     would be a read-modify-write across the network, and two replicas
//     interleaving there both see the same starting balance and both spend it —
//     the classic lost update, which under load hands out roughly double the
//     limit exactly when the limit matters most.
const luaTokenBucket = `
local capacity    = tonumber(ARGV[1])
local refillPerMs = tonumber(ARGV[2])
local cost        = tonumber(ARGV[3])
local ttlMs       = tonumber(ARGV[4])

local t   = redis.call("TIME")
local now = (tonumber(t[1]) * 1000) + math.floor(tonumber(t[2]) / 1000)

local jar    = redis.call("HMGET", KEYS[1], "tokens", "ts")
local tokens = tonumber(jar[1])
local ts     = tonumber(jar[2])

if tokens == nil or ts == nil then
  tokens = capacity
  ts     = now
end

local elapsed = now - ts
if elapsed > 0 then
  tokens = math.min(capacity, tokens + (elapsed * refillPerMs))
end

local allowed = 0
if tokens >= cost then
  tokens  = tokens - cost
  allowed = 1
end

redis.call("HSET", KEYS[1], "tokens", string.format("%.6f", tokens), "ts", now)
redis.call("PEXPIRE", KEYS[1], ttlMs)

return {allowed, math.floor(tokens)}
`

// CheckBurst asks Redis for one admission decision.
func (r *RedisTokenBucket) CheckBurst(a BurstArgs) (Decision, error) {
	ttl := r.idleTTL
	if ttl <= 0 {
		ttl = 10 * time.Minute
	}
	// A jar must live at least long enough to refill completely, or a slow
	// client would find their bucket expired and silently reset to full —
	// which is a refund, not a limit.
	if a.RefillRatePerMs > 0 {
		if full := time.Duration(a.Capacity/a.RefillRatePerMs) * time.Millisecond; full > ttl {
			ttl = full
		}
	}

	reply, err := r.client.Do("EVAL", luaTokenBucket, "1", a.Key,
		strconv.FormatFloat(a.Capacity, 'f', -1, 64),
		strconv.FormatFloat(a.RefillRatePerMs, 'f', -1, 64),
		strconv.FormatFloat(a.Cost, 'f', -1, 64),
		strconv.FormatInt(ttl.Milliseconds(), 10),
	)
	if err != nil {
		return Decision{}, err
	}
	parts, ok := reply.([]any)
	if !ok || len(parts) != 2 {
		return Decision{}, fmt.Errorf("redis: token bucket returned %T, want a 2-element array", reply)
	}
	allowed, ok1 := parts[0].(int64)
	remaining, ok2 := parts[1].(int64)
	if !ok1 || !ok2 {
		return Decision{}, fmt.Errorf("redis: token bucket returned non-integer elements")
	}
	return Decision{Allowed: allowed == 1, Remaining: int(math.Max(float64(remaining), 0))}, nil
}

// Compile-time proof that every limiter the gateway may be handed actually
// satisfies the interface. Without these, a signature drift is only caught at
// the call site — or not at all, if nothing wires the Redis path up.
var (
	_ BurstLimiter = (*TokenBucket)(nil)
	_ BurstLimiter = (*RedisFixedWindow)(nil)
	_ BurstLimiter = (*RedisLuaFixedWindow)(nil)
	_ BurstLimiter = (*RedisTokenBucket)(nil)
)
