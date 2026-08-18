# Benchmarks

Every number on this page was measured on one machine on **19 August 2026** and
is reproducible with the commands printed beside it. Nothing here is carried
over from an older run, an older language, or somebody's memory. If a figure in
`README.md` or `RESUME.md` disagrees with this file, this file is right and the
other one is stale — say so rather than quoting the flattering one.

The whole point of the page is the separation in the two headings below. The
**limiter** does ~11M checks/sec. The **service** does ~47k requests/sec. Both
are true, they differ by 240×, and the difference is almost entirely net/http
and the kernel. Quote the second one when somebody asks what the gateway does.

---

## The machine

| | |
|---|---|
| CPU | Apple M2, 8 cores (4 performance + 4 efficiency) |
| RAM | 8 GB |
| OS | macOS 26.2 (darwin/arm64) |
| Go | go1.26.3 |
| Node (baseline only) | v24.14.0 |
| `GOMAXPROCS` | 8 (unset; Go's default) |
| Load average before the run | **2.27** (1 min), 3.69 (5 min), 4.28 (15 min) |
| Load average after the run | **8.07** (1 min), 5.98 (5 min), 4.15 (15 min) |

The after-figure is the benchmark itself: a `RunParallel` benchmark saturates
all 8 cores by design, so a high load average at the end is the measurement
working, not interference. The before-figure is what matters, and 2.27 on an
8-core laptop is idle-ish but not a clean room — a desktop session, a browser
and a terminal were resident. **Expect ±10% run to run**, more on the
socket-bound rows. This was measured on a laptop with thermal limits and
efficiency cores, not a dedicated benchmark host; treat every figure as an
order of magnitude with a bit of precision, not a specification.

## The command

One command produces everything except the Node baseline:

```bash
go test -bench=. -benchtime=2s -count=5 -benchmem -run=XXX \
  ./internal/limiter/ ./internal/gateway/
```

`-run=XXX` matches no test, so only benchmarks run. `-count=5` gives five
independent measurements per benchmark; **every ns/op below is the best of the
five**, which is the convention for a shared machine — the fastest run is the
one least polluted by something else on the box. Ranges are noted where the
spread was wide enough to matter.

---

## In-process: the limiter alone

`internal/limiter/bench_test.go`. No HTTP, no sockets, no handler — one
`TokenBucket.Check()` call per operation, arithmetic under a sharded mutex.

```bash
go test -bench=. -benchtime=2s -count=5 -benchmem -run=XXX ./internal/limiter/
```

| Benchmark | Concurrency | ns/op | ops/sec | B/op | allocs/op |
|---|---|---:|---:|---:|---:|
| `TokenBucket_Serial` | 1 goroutine, 1 key | 88.9 | **11.25M** | 0 | **0** |
| `TokenBucket_SerialManyTenants` | 1 goroutine, 512 keys | 128.6 | 7.78M | 0 | **0** |
| `TokenBucket_ManyTenants` | 8 goroutines, 512 keys | 53.1 | **18.83M** | 0 | **0** |
| `TokenBucket_Contended` | 8 goroutines, 1 hot key | 230.7 | 4.33M | 0 | **0** |
| `MonthlyQuota_Parallel` | 8 goroutines, 1 key | 291.5 | 3.43M | 8 | 1 |

Zero allocations per check on every token-bucket row, which is the claim worth
defending: sustained admission traffic creates no garbage, so there is no GC
pressure to show up as tail latency later.

Two rows deserve reading together. Serial with one key is 88.9 ns; serial with
512 keys is 128.6 ns. That 45% is cache misses on the map, not lock cost —
there is no contention in a single goroutine. Real traffic looks like the
second row, so that is the fair single-thread number.

`TokenBucket_Contended` (4.33M/sec) is the honest floor: eight goroutines on one
tenant all hash to the same shard, so striping cannot help and the mutex
serialises them. Quote it when asked about worst case.

### What lock striping actually bought

The naive port — one map behind one mutex — serialises every admission decision
in the process. `BenchmarkTokenBucket_ShardCount` varies only the shard count,
8 goroutines over 512 keys:

```bash
go test -bench=TokenBucket_ShardCount -benchtime=2s -count=5 -run=XXX ./internal/limiter/
```

| Shards | ns/op | ops/sec | |
|---:|---:|---:|---|
| 1 | 257.5 | 3.88M | one global lock — the naive port |
| 2 | 230.1 | 4.35M | |
| 8 | 116.1 | 8.61M | 1× GOMAXPROCS, the obvious default |
| 32 | 70.5 | 14.19M | |
| 128 | 57.4 | **17.43M** | flattens out around here |

**4.5× from sharding alone** (3.88M → 17.43M). The shards=1 row was the noisiest
in the set (257–315 ns across the five runs), so treat the multiplier as
"between 4× and 5×" rather than a precise 4.5.

The default is ~8× GOMAXPROCS rather than 1×, and the 8-shard row is why:
goroutines are not pinned to cores and hash collisions cluster, so one shard per
core leaves them hot at less than half the achievable throughput.

### The Node baseline

`README.md` claims Go was chosen for reasons other than single-thread speed.
That claim rests on a comparison, so the comparison is now runnable:

```bash
node bench/node_baseline.mjs
```

`bench/node_baseline.mjs` is the deleted Node token bucket verbatim (recovered
with `git show 3a2895e:src/algorithms/tokenBucketLimiter.js`; the Node backend
was removed in commit `79c74d4`) plus a harness shaped like Go's — same huge
capacity, same key sets, JIT warm-up discarded, best of five.

| Single-threaded | ns/op | checks/sec |
|---|---:|---:|
| Node, 1 hot key | **78.9** | 12.67M |
| Go, 1 hot key | 88.9 | 11.25M |
| Node, 512 tenants | **108.3** | 9.23M |
| Go, 512 tenants | 128.6 | 7.78M |

**Node is faster single-threaded — by about 10–16%.** Say it plainly; V8's JIT is
very good at a monomorphic hot loop like this one, and the follow-up question
that exposes a fudge here is an easy one to ask.

> **Correction.** Earlier versions of this repo claimed Node did 22M checks/sec
> at 45 ns/op against Go's 18.1M at 55 ns/op. Neither figure reproduces. The
> measured numbers are 78.9 ns for Node and 88.9 ns for Go on the same machine
> on the same day. The *direction* of the old claim was right — Node really is
> ahead single-threaded — but the magnitudes were not, and both sides were
> roughly twice as fast on paper as they are in fact.

What Go actually buys, stated against these numbers rather than against a
better-sounding set:

1. **9.23M/sec is the ceiling for the entire Node process**, because JavaScript
   runs one callback at a time. Go's 18.83M/sec on the same 512-key workload is
   8 cores aggregating, i.e. **~2× the whole Node process**, and it scales with
   the box. Not 2× per core — 2× total, and that is the honest framing.
2. **Zero allocations per check.** The Node version allocates a result object on
   every call; the Go version allocates nothing at all.
3. **It has to be correct under real parallelism**, which Node never had to be.
   Six real bugs came out of making it so (see `RESUME.md`).

---

## Over HTTP: the service

`internal/gateway/bench_http_test.go`. The same `gateway.Middleware` wrapping a
handler that does nothing but `WriteHeader(200)`, so what is measured is the
layer plus its transport and nothing else.

```bash
go test -bench=. -benchtime=2s -count=5 -benchmem -run=XXX ./internal/gateway/
```

Two configurations appear in both tables:

- **lean** — middleware mode as `README.md` documents it: a policy, the default
  sharded in-memory token bucket, the default audit ring.
- **full stack** — what `cmd/gateway` actually deploys: the above plus the
  monthly plan meter, behavioural fingerprinting and the autopilot, all on the
  request path.

### Direct `ServeHTTP` — no socket

Real net/http plumbing (headers, context, handler chain) into an in-memory
`ResponseWriter`. No kernel, no TCP, no request parsing.

| Benchmark | What | ns/op | req/sec | B/op | allocs/op |
|---|---|---:|---:|---:|---:|
| `GatewayDirect_Baseline` | bare handler, **no LimitPlane** | 3.4 | 298M | 0 | 0 |
| `GatewayDirect_Serial` | lean, 1 goroutine | 1456 | 687k | 768 | 14 |
| `GatewayDirect_ManyTenants` | lean, 8 goroutines, 512 keys | 853 | **1.17M** | 768 | 14 |
| `GatewayDirect_HotKey` | lean, 8 goroutines, 1 key | 781 | 1.28M | 768 | 14 |
| `GatewayDirect_Blocked` | lean, every request 429s | 1197 | 835k | 897 | 22 |
| `GatewayDirect_FullStack` | deployed config, 8 goroutines | 3290 | 304k | 1413 | 34 |
| `GatewayDirect_Recorder` | lean, via `httptest.NewRecorder` | 2033 | 492k | 1728 | 21 |

The `_Recorder` row is there to be subtracted, not quoted: it is the same work
as `_Serial` with a fresh `httptest.NewRecorder` per call, and the 577 ns and
960 B of difference are the recorder, not the gateway. Most people write this
benchmark the `_Recorder` way and then report the gateway as 40% slower than it
is. The other rows reuse a minimal `ResponseWriter` for exactly that reason.

`_Blocked` costs about 350 ns and 8 allocations more than the admit path,
because refusing a request means encoding a JSON body that explains itself.
Refusing is more expensive than allowing — worth knowing before designing a
system whose steady state is refusal.

The full stack costs **~2.4 µs more per request** than the lean one (3290 vs
853 ns) and doubles the allocations. Fingerprinting, the monthly meter and the
autopilot are not free; they are affordable, which is a different claim.

### Real socket — `httptest.NewServer` + `http.Client`

The same middleware behind a real listener, driven by a real client over
loopback with keep-alive on. This includes request serialisation, syscalls,
net/http's reader and writer goroutines, and response parsing on the client
side.

| Benchmark | What | ns/op | req/sec | B/op | allocs/op |
|---|---|---:|---:|---:|---:|
| `GatewayHTTP_Baseline` | bare handler, **no LimitPlane** | 14250 | **70.2k** | 6085 | 67 |
| `GatewayHTTP_ManyTenants` | lean, 512 keys | 21466 | **46.6k** | 7842 | 91 |
| `GatewayHTTP_HotKey` | lean, 1 hot key | 21071 | 47.5k | 7851 | 91 |
| `GatewayHTTP_FullStack` | deployed config | 24624 | 40.6k | 9240 | 123 |

**47k req/sec is the number to quote for the service.** Not 11M, not 18M.

### Explaining the gap, which is the actual interview question

> *"Your limiter does 11M checks a second but your gateway does 47k requests a
> second. Where did the other 99.6% go?"*

Into net/http and the kernel, and the baseline row proves it. A bare handler
with no LimitPlane in front of it manages 70.2k req/sec on this machine — so
**33% of the theoretical maximum is gone before the gateway exists**, and the
gateway is not what is expensive. A rate limiter is never the bottleneck of an
HTTP service; it is a sub-microsecond decision bolted onto a request that costs
14 µs just to receive and reply to.

Three specific things to be careful about when reading these rows:

1. **The apparent per-request cost of the layer is inflated here.** 21466 −
   14250 = 7.2 µs of apparent overhead, but the same layer measured without a
   socket costs 0.85 µs. The difference is that `httptest`'s load generator runs
   on the *same 8 cores as the server*: work the middleware does is work the
   client cannot do, so server cost shows up twice. The honest statement is
   "the layer costs under a microsecond of CPU and about a third of achievable
   throughput on a machine where the client and server share a CPU."
2. **HotKey and ManyTenants are indistinguishable over HTTP** (21.1 vs 21.5 µs,
   within noise). Lock contention that is a 4× effect in the in-process
   benchmark is invisible once a socket is involved. The sharding work is real
   and it is measurable — but you have to say *where* it is measurable, and it
   is not here.
3. **Loopback is not a network.** No NIC, no TLS, no real RTT. A deployed
   gateway behind a load balancer will be slower for reasons this benchmark
   cannot see.

---

## What the HTTP benchmark found

The HTTP benchmark did not exist before 19 August 2026, and the first time it
ran it immediately paid for itself.

`internal/audit` described itself as a ring buffer, but `Record` appended and
then did `copy(events, events[1:])` to drop the oldest entry. That is O(n) per
write, and once the diary saturates at its default 1000 pages it memmoves about
130 KB **on every admitted request**. The limiter benchmarks could never see it
because they never touch the audit log. Over HTTP it was the single largest cost
in the gateway — the diary cost 50× more than the decision it was recording.

The fix is a real ring: a fixed slice plus a moving head index, O(1) per write,
same external behaviour, same tests.

| Benchmark | before | after | |
|---|---:|---:|---|
| `GatewayDirect_Serial` | 7588 ns | 1456 ns | **5.2×** |
| `GatewayDirect_ManyTenants` | 8647 ns | 853 ns | **10.1×** |
| `GatewayDirect_HotKey` | 10053 ns | 781 ns | **12.9×** |
| `GatewayHTTP_ManyTenants` | 29146 ns (34.3k/s) | 21466 ns (46.6k/s) | **1.36×** |

The lesson is the one worth telling: a benchmark that measures your favourite
function will keep telling you your favourite function is fast. The bottleneck
was in a package nobody thought to benchmark, and it took a benchmark at the
layer a user actually touches to find it.

---

## Reproducing all of it

```bash
# in-process limiter + HTTP gateway, the numbers in both tables above
go test -bench=. -benchtime=2s -count=5 -benchmem -run=XXX \
  ./internal/limiter/ ./internal/gateway/

# the Node comparison
node bench/node_baseline.mjs

# check the machine was idle enough for any of it to mean anything
uptime
```
