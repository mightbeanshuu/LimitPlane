# Learning Kubernetes from LimitPlane

This document has one job: turn this repository into the only Kubernetes course
you need, and leave you able to defend every line of it in an interview.

It is not a tour of features. Each lesson names a concept, points at the exact
file where this project uses it, gives you a command to run that makes the
concept visible, and ends with the question an interviewer actually asks about
it — plus the answer, grounded in code you wrote rather than a definition you
memorised.

**Read it with the repo open.** Every claim here is checkable, and checking it
is the point.

---

## Table of contents

- [Why this project is a good way to learn](#why-this-project-is-a-good-way-to-learn)
- [The 3-minute version](#the-3-minute-version)
- [What actually got built](#what-actually-got-built)
- [The learning path](#the-learning-path) — 12 lessons
- [Interview defense](#interview-defense) — 24 questions
- [What this deliberately does not do](#what-this-deliberately-does-not-do)
- [Glossary](#glossary)

---

## Why this project is a good way to learn

Most people learn Kubernetes by deploying somebody else's nginx. You apply a
Deployment, you see a pod, you have learned approximately nothing, because
nothing about your workload made any of those decisions necessary.

LimitPlane is a better teacher because it is genuinely hostile to naive
deployment. It holds mutable state (token buckets). It holds long-lived
connections (WebSocket, SSE). It has a control plane that writes to the API
server. It sits in the request path of other services. Every one of those
properties breaks something obvious:

| The naive thing | What breaks | The lesson you are forced to learn |
|---|---|---|
| Scale to 3 replicas | Each pod keeps its own buckets, so every tenant gets 3× their limit | Stateful workloads need externalised state |
| 3 replicas of the operator | All three write status, waking each other's watches forever | Leader election, and why |
| Reconcile on each watch event | Your own status write triggers another reconcile | `metadata.generation` vs `resourceVersion` |
| Export a metric per tenant | `anon:<ip>` labels explode Prometheus | Cardinality is a production concern |
| One health endpoint | A Redis blip restarts every replica at once | Liveness ≠ readiness |
| `kubectl apply -f crd.yaml && kubectl apply -f cr.yaml` | "no matches for kind" | Established conditions and admission ordering |

You cannot learn those from a tutorial that deploys a stateless hello-world.
They only show up when the workload is real, and here they are all real.

---

## The 3-minute version

```bash
make kind-demo
```

That creates a three-node kind cluster, builds the image, side-loads it,
installs the CRDs, deploys the gateway with its operator and Redis, applies a
rate-limit policy as YAML, proves the limiting is enforced, then changes a
production limit with `kubectl patch` and shows the gateway obeying it — with
zero pod restarts.

Then look around:

```bash
kubectl -n limitplane get rlp,lpt          # your limits, as objects
kubectl -n limitplane describe rlp api-limits
kubectl -n limitplane get lease limitplane-operator -o yaml
kubectl -n limitplane get events --sort-by=.lastTimestamp | tail
make kind-down
```

---

## What actually got built

```
                          ┌──────────────────────────────────────┐
   kubectl apply ────────▶│         Kubernetes API server        │
   (a PR, ArgoCD, a human)│  RateLimitPolicy   Tenant   Lease    │
                          └──────────────┬───────────────────────┘
                                         │  watch (LIST → WATCH → BOOKMARK)
                                         │  status patch (leader only)
              ┌──────────────────────────┼──────────────────────────┐
              ▼                          ▼                          ▼
      ┌───────────────┐          ┌───────────────┐          ┌───────────────┐
      │  gateway pod  │          │  gateway pod  │          │  gateway pod  │
      │  ┌─────────┐  │          │  ┌─────────┐  │          │  ┌─────────┐  │
      │  │operator │  │          │  │operator │  │          │  │operator │  │
      │  │ watches │  │          │  │ watches │  │          │  │ watches │  │
      │  │ ★LEADER │  │          │  │ follower│  │          │  │ follower│  │
      │  └─────────┘  │          │  └─────────┘  │          │  └─────────┘  │
      │   limiter     │          │   limiter     │          │   limiter     │
      └───────┬───────┘          └───────┬───────┘          └───────┬───────┘
              └──────────────────────────┼──────────────────────────┘
                                         ▼
                              ┌─────────────────────┐
                              │  Redis: ONE token   │
                              │  bucket per tenant  │
                              └─────────────────────┘

   Every replica WATCHES (each pod serves traffic, so each needs the config).
   Only the leader WRITES (three writers would live-lock the API server).
```

And separately, the reason other teams would adopt it:

```
   client ──▶ Envoy Gateway ──(1) "may this proceed?"──▶ LimitPlane /v1/authz
                    │                                          │
                    │◀────────── 200 allow / 429 deny ─────────┘
                    │
                    └──(2)──▶ your service, unmodified
```

`deploy/k8s/gateway-api/envoy-gateway.yaml` is twelve lines of `SecurityPolicy`
and it rate-limits every service behind the gateway. None of them are
recompiled, redeployed, or even aware.

### New files, and what each one teaches

| Path | What it is | Concepts it teaches |
|---|---|---|
| `internal/k8s/config.go` | In-cluster auth | ServiceAccount tokens, projected token rotation, CA verification |
| `internal/k8s/client.go` | REST client | API paths, subresources, patch types, server-side apply, finalizers |
| `internal/k8s/watch.go` | The reflector | LIST→WATCH, `resourceVersion`, BOOKMARK, 410 Gone, jittered backoff |
| `internal/k8s/lease.go` | Leader election | Coordination Leases, compare-and-swap, self-fencing |
| `internal/k8s/events.go` | Event recorder | Why `kubectl describe` shows anything useful |
| `internal/operator/types.go` | CRD Go types | Conditions, `observedGeneration`, spec/status split |
| `internal/operator/operator.go` | The reconciler | Level-triggered reconciliation, idempotency, write amplification |
| `internal/metrics/prometheus.go` | Exposition format | Metric types, label escaping, cardinality |
| `internal/server/handlers_k8s.go` | ext_authz + probes | Forward auth, liveness vs readiness |
| `deploy/k8s/crds/` | The CRDs | OpenAPI schemas, CEL validation, printer columns |
| `deploy/k8s/base/` | The manifests | Every workload object, and why each field is set |
| `deploy/helm/limitplane/` | The chart | Templating, immutable selectors, capability checks |
| `.github/workflows/release.yml` | Image publishing | Multi-arch buildx, GHCR auth, SBOM, provenance attestation |

---

## The learning path

Twelve lessons, ordered so each builds on the last. Budget 30–60 minutes each.
Do the **Try it** in every one — reading about a 410 Gone teaches you nothing;
watching your controller recover from one teaches you permanently.

---

### Lesson 1 — Pods, Deployments, and why the manifest looks like that

**Concept.** A Pod is the unit of scheduling. You almost never create one: a
Deployment creates a ReplicaSet, which creates Pods, so that a rollout is a
matter of creating a new ReplicaSet and draining the old one.

**In this repo.** `deploy/k8s/base/deployment.yaml`. Read it top to bottom.
Every field has a comment saying why it is set, and several exist to prevent a
specific failure.

**Try it.**
```bash
kubectl -n limitplane get deploy,rs,pod
kubectl -n limitplane rollout history deploy/limitplane
kubectl -n limitplane rollout restart deploy/limitplane
kubectl -n limitplane get pods -w      # watch the surge-then-drain
```

**The bit people miss.** `maxUnavailable: 0` with `maxSurge: 1`. The default is
25%/25%, which means a rollout deliberately runs below full capacity. For a
gateway — the front door — that means shedding real traffic during every deploy.

**Interviewer asks:** *"Walk me through what happens when you change the image
tag."*

**Your answer:** The Deployment controller sees a new pod template hash and
creates a second ReplicaSet. With `maxSurge: 1` it scales the new one to 1,
waits for that pod to pass its readiness probe — for us `/readyz`, which does
not report ready until the operator's initial LIST has completed, so a pod
never receives traffic before it knows the current tiers and API keys — then
scales the old ReplicaSet down by one. It repeats until the new set is at full
size. `maxUnavailable: 0` means capacity never dips below the target, which
matters because we are in the request path of everything behind the gateway.
The old ReplicaSet is retained (three of them, `revisionHistoryLimit: 3`) so
`kubectl rollout undo` is instant.

---

### Lesson 2 — Probes: three questions, not one

**Concept.** Kubernetes asks three different questions and people wire them to
one endpoint.

| Probe | Question | On failure |
|---|---|---|
| `startupProbe` | Has it finished booting? | Hold the other probes off |
| `livenessProbe` | Is it broken beyond recovery? | **Kill and restart the container** |
| `readinessProbe` | Should traffic go here *now*? | Remove from Service endpoints |

**In this repo.** `internal/server/handlers_k8s.go`, `handleReady`. `/healthz`
is deliberately trivial and depends on nothing. `/readyz` reports not-ready
until `PoliciesSynced && TenantsSynced`.

**Try it.**
```bash
kubectl -n limitplane exec deploy/limitplane -- true 2>/dev/null || \
  kubectl -n limitplane port-forward svc/limitplane 3000:80 &
curl -s localhost:3000/healthz | jq
curl -s localhost:3000/readyz  | jq
```

**The bit people miss.** Putting a dependency check in `livenessProbe` is a
recognised way to turn a degradation into an outage: Redis blips, every replica
fails liveness, every replica restarts *simultaneously*, and now you have no
gateway at all. Liveness must only test the process itself.

**Interviewer asks:** *"Your service depends on Redis. Should the health check
test the Redis connection?"*

**Your answer:** Readiness, maybe. Liveness, never. If Redis goes down and it is
in the liveness probe, every replica gets killed at once and you have converted
a partial failure into a total one — restarting the pod cannot fix a dependency
outage anyway. In LimitPlane it is not even in readiness, because the limiter
fails *open* when Redis is unreachable: the gateway keeps serving traffic
unlimited, on the reasoning that an outage in the rate limiter should cost you
rate limiting, not availability. Readiness here gates on something a restart
*would* fix and that genuinely makes the pod unfit to serve: whether the
operator's initial LIST has completed, because until it has, this pod does not
know which API keys exist and would reject paying customers as unknown.

---

### Lesson 3 — Custom Resource Definitions

**Concept.** A CRD teaches the API server a new noun. After applying one,
`kubectl get ratelimitpolicies` works, RBAC applies to it, admission webhooks
can validate it, and GitOps can sync it — because the API server treats a
custom resource exactly like a built-in one.

**In this repo.** `deploy/k8s/crds/ratelimitpolicy.yaml` and `tenant.yaml`.

**Try it.**
```bash
kubectl explain ratelimitpolicy.spec.tiers
kubectl explain tenant.spec.apiKeySecretRef
kubectl get crd ratelimitpolicies.limitplane.dev -o yaml | head -40
kubectl api-resources --api-group=limitplane.dev
```
Note that `kubectl explain` prints the `description` fields from the schema.
Documentation that ships *inside* the cluster is a real advantage of a CRD over
a ConfigMap, and it costs nothing.

Now try to break it:
```bash
kubectl apply -f - <<'EOF'
apiVersion: limitplane.dev/v1alpha1
kind: RateLimitPolicy
metadata: {name: bad, namespace: limitplane}
spec:
  tiers:
    broken: {capacity: 10, refillPerSecond: 0}
EOF
# rejected by the API server, before it is ever stored
```

**The bit people miss.** Validation belongs as early as it can live. A bad value
caught by the API server is a clear error at `kubectl apply` time. The same bad
value caught by your controller is an object sitting in etcd, silently doing
nothing, with an explanation in a status field nobody is looking at.

**Interviewer asks:** *"Why a CRD and not a ConfigMap?"*

**Your answer:** Three things a ConfigMap cannot do. First, schema validation:
`refillPerSecond: 0` would create a bucket that never refills and block that
tier's tenants permanently — the CRD's OpenAPI schema rejects it at admission,
so it cannot reach the cluster. A ConfigMap holds an opaque string; the mistake
is only found at parse time, inside the app, after it is deployed. Second,
status: a custom resource has a `/status` subresource with its own RBAC, so the
controller reports observed state without being able to touch the spec a user
wrote, and `kubectl get rlp` shows live allowed/blocked counts. Third,
everything Kubernetes already knows how to do to an object applies for free —
per-resource RBAC, admission webhooks, `kubectl wait --for=condition=Ready`,
ArgoCD health assessment, audit logging.

---

### Lesson 4 — The watch loop, which is what a controller actually is

**Concept.** The core protocol of every Kubernetes controller:

1. **LIST** everything. Note the collection's `resourceVersion`.
2. **WATCH** from *exactly* that `resourceVersion`.
3. Handle `ADDED` / `MODIFIED` / `DELETED`.
4. On `BOOKMARK`, advance your cursor and do nothing else.
5. On **410 Gone**, throw the cursor away and go back to step 1.

**In this repo.** `internal/k8s/watch.go`, `Reflector.Run`. About 200 lines, and
it is the whole thing — this is what `client-go`'s reflector does underneath.

**Try it.**
```bash
go test ./internal/k8s/ -run TestReflector -v
```
Three tests: the LIST→WATCH handoff, the 410 recovery, and bookmark handling.
Read `watch_test.go`; the fake API server in it is a good model of how the real
one behaves.

**The bit people miss.** Two things, both subtle.

*Watching from "now" instead of from the LIST's `resourceVersion`.* Anything
that changes between your LIST and your WATCH is lost forever, and you will
never notice, because nothing errors.

*Treating 410 as a retryable error.* It is not. 410 means the API server has
already discarded the history you are asking to resume from, so retrying the
same watch loops forever. The only correct response is a full re-LIST.

**Interviewer asks:** *"What is `resourceVersion` and why can't you compare
two of them?"*

**Your answer:** It is an opaque cursor into the cluster's change stream, backed
by an etcd revision. Opaque is the operative word: it is a string, the API
contract explicitly says not to parse it, and comparing two numerically is
wrong because the value is only meaningful to the API server that issued it —
across an etcd restore or a different resource, the ordering does not hold. You
only ever hand it back verbatim. In `watch.go` I store it as a string and pass
it straight to the next watch. The one place it needs care is 410 Gone: the
server keeps a limited history window (a few minutes of changes), so a
controller that reconnects after a pause may hold a cursor the server has
forgotten. That arrives *in-band* as an `ERROR` event on an otherwise healthy
stream, not as an HTTP status — code that only checks the response code misses
it entirely and wedges.

---

### Lesson 5 — Level-triggered reconciliation

**Concept.** A controller should ask *"what should be true?"*, never *"what just
happened?"*. Recompute the desired state from your full cache every time, and
make the world match.

**In this repo.** `internal/operator/operator.go`, `reconcile()`. It ignores the
event that woke it and rebuilds desired state from every cached object.

**Try it.**
```bash
go test ./internal/operator/ -run TestTenantBeforeItsPolicy -v
```
A `Tenant` arrives before the `RateLimitPolicy` defining its tier. Edge-triggered
code would drop it. Level-triggered code just converges on the next pass, with
no retry queue and no ordering requirement.

**The bit people miss.** This is what makes at-least-once event delivery safe.
Watches can deliver twice, and drop events entirely during a 410 recovery. If
your handler applies deltas, both are corruption. If it recomputes state, both
are free.

**Interviewer asks:** *"Your controller missed an event. What breaks?"*

**Your answer:** Nothing, and that is by construction rather than by luck. The
handler does not apply the delta it received — it updates a local cache and then
recomputes the entire desired state from every cached object. So a duplicated
event does the same work twice with the same result, and a dropped event is
repaired by the next reconcile or by the re-LIST that a 410 forces. That is the
level-triggered property, and it is why the reflector can advance its cursor
*before* calling the handler: worst case one event replays, and every handler is
idempotent.

---

### Lesson 6 — `generation`, `observedGeneration`, and the write loop

**Concept.** `metadata.generation` increments **only when spec changes**.
`metadata.resourceVersion` changes on **any** write, including status.

**In this repo.** `onPolicyEvent` in `operator.go`:

```go
if prev, ok := o.policies[key]; ok &&
    prev.Metadata.Generation == p.Metadata.Generation &&
    p.Metadata.Generation != 0 {
    o.policies[key] = &p   // refresh resourceVersion, do no work
    o.mu.Unlock()
    return nil
}
```

**Try it.**
```bash
go test ./internal/operator/ -run TestStatusEchoDoesNotTriggerAReconcile -v
```

**The bit people miss.** Your own status write comes back to you as a `MODIFIED`
event. Reconcile on it and you write status again, which produces another event,
forever. With three replicas all writing, that is a self-inflicted denial of
service on your own control plane — and it is one of the most common ways a
first operator misbehaves in production.

**Interviewer asks:** *"How does a user know your controller has caught up?"*

**Your answer:** `status.observedGeneration` compared against
`metadata.generation`. Equal means the status reflects the current spec; behind
means the controller has not processed the latest edit. It is what makes
`kubectl wait --for=condition=Ready` trustworthy — without it you cannot
distinguish "reconciled and healthy" from "not looked at yet", and a wait that
returns immediately on a stale condition is worse than no wait. The same field
is what lets me skip reconciling on my own status echo.

---

### Lesson 7 — Leader election

**Concept.** Multiple replicas, one writer. Kubernetes does it with a
`coordination.k8s.io/v1` Lease: the holder rewrites `renewTime` on a timer, and
if it goes stale past `leaseDurationSeconds` a challenger takes over, with the
API server's `resourceVersion` check making sure only one challenger wins.

**In this repo.** `internal/k8s/lease.go`. Note the design decision in `Run`:
**every replica watches, only the leader writes.**

**Try it.**
```bash
kubectl -n limitplane get lease limitplane-operator -o yaml
kubectl -n limitplane delete pod <the-leader-pod>     # watch failover
kubectl -n limitplane get lease limitplane-operator \
  -o jsonpath='{.spec.holderIdentity}{"  transitions="}{.spec.leaseTransitions}{"\n"}'
go test ./internal/k8s/ -run TestLease -v
go test ./internal/k8s/ -run TestConcurrentTakeover -v
```

**The bit people miss.** Two things.

*It is not a distributed lock, and cannot be.* If the leader's process freezes
past its deadline — a long GC pause, a suspended VM — a new leader is elected
while the old one still believes it leads. Safety comes from the leader
**standing down on its own** the moment it cannot renew, so the windows never
overlap. That is why `renew()` returns (and cancels the leader context) on an
API timeout, even though nothing crashed.

*Staleness is measured against your own clock, not the holder's.* `lease.go`
records when *it first saw* a given `resourceVersion` and measures elapsed time
locally. Comparing your wall clock to a `renewTime` written by another machine
turns clock skew directly into premature takeovers.

**Interviewer asks:** *"Two replicas both think they are the leader. What
happens to your data?"*

**Your answer:** In this design, nothing, and that is deliberate — the work is
idempotent. Both would recompute the same desired state from the same objects
and write the same status, so the damage is wasted API calls, not corruption.
But I would rather it not happen, so there are two guards. The API server's
compare-and-swap on `resourceVersion` means only one PUT can win a contested
lease — there is tested coverage for that in `TestConcurrentTakeoverLosesOnConflict`.
And the leader self-fences: if it cannot renew within `RenewDeadline` it cancels
its own leader context and stops writing, before the lease it advertised has
actually expired. I also export `limitplane_operator_is_leader` and alert on
`sum(...) != 1`, which catches both a split brain and a leaderless cluster.

---

### Lesson 8 — RBAC, and the one verb that matters

**Concept.** Role-based access control: a Role or ClusterRole lists
(apiGroups, resources, verbs); a binding attaches it to a ServiceAccount.

**In this repo.** `deploy/k8s/base/rbac.yaml`. Read the comments — each rule
exists because the code genuinely calls it.

**Try it.**
```bash
kubectl auth can-i --as=system:serviceaccount:limitplane:limitplane \
  list secrets -n limitplane        # no
kubectl auth can-i --as=system:serviceaccount:limitplane:limitplane \
  get secrets -n limitplane         # yes
kubectl auth can-i --as=system:serviceaccount:limitplane:limitplane \
  update ratelimitpolicies -n limitplane   # no — status only
```

**The bit people miss.** `get` versus `list` on Secrets. With `list`, a
compromised pod enumerates every credential it can reach. With `get` it must
already know the exact name — which, here, only a `Tenant` object supplies. One
verb, enormous difference in blast radius. This pod terminates untrusted
internet traffic, so its token is exactly what an attacker would want.

Also: the operator has **no `update` on the parent resource**, only on
`/status` and `/finalizers`. A bug in the reconcile loop physically cannot
rewrite everyone's limits.

**Interviewer asks:** *"Your operator needs to read Secrets. How do you scope
that safely?"*

**Your answer:** `get` only, never `list` or `watch`. `get` requires knowing the
exact name, which the Tenant object supplies, so a compromised pod cannot
enumerate the namespace's credentials. Then I split the permissions by
subresource: `get/list/watch` on the custom resources, but `update/patch` only
on `/status` and `/finalizers` — the operator can report what it observes and
release an object it is holding, and cannot touch the spec a human wrote.
Leader election is a separate namespaced Role rather than part of the
ClusterRole, because the Lease only ever lives in the operator's own namespace.
And `WATCH_NAMESPACE` lets the whole thing run with a Role instead of a
ClusterRole when it only serves one namespace.

---

### Lesson 9 — Finalizers

**Concept.** An object with finalizers does not disappear on `kubectl delete`.
The API server sets `deletionTimestamp` and waits for every finalizer to be
removed. That pause is your only chance to clean up external state.

**In this repo.** `FinalizerTenant` in `operator/types.go`; the terminating
branch in `onTenantEvent`; `RemoveFinalizer` in `k8s/client.go`.

**Try it.**
```bash
kubectl -n limitplane get tenant acme -o jsonpath='{.metadata.finalizers}'
kubectl -n limitplane delete tenant acme     # blocks until the key is revoked
go test ./internal/operator/ -run TestDeletingATenantRevokesItsKey -v
```

**The bit people miss.** Look at `RemoveFinalizer`. It uses a **JSON patch with
a `test` op**, not a merge patch:

```go
{"op": "test",   "path": "/metadata/finalizers/1", "value": name},
{"op": "remove", "path": "/metadata/finalizers/1"},
```

A merge patch on a list replaces the *whole list*. If another controller added
its finalizer between your read and your write, you delete theirs — and the
object vanishes before they have cleaned up. The `test` op makes the API server
verify the list is unchanged and reject the write with 409 if it is not.

The other classic: a finalizer whose controller is gone means the object can
**never** be deleted. `kubectl delete` hangs forever, and the escape hatch is
`kubectl patch ... -p '{"metadata":{"finalizers":[]}}' --type=merge`.

**Interviewer asks:** *"Why do you need a finalizer here?"*

**Your answer:** Because the API key lives in two places: the Tenant object, and
the running gateway's in-memory policy. Without a finalizer, `kubectl delete
tenant acme` removes the object while the gateway keeps honouring `acme`'s key
— the customer is deleted on paper and still being served in production, which
is a real security hole with no visible symptom. The finalizer makes the API
server hold the deletion open until I have revoked the key from the live policy,
then I remove the finalizer and the object goes. There is a second revocation
path too, for the case a finalizer cannot catch: if someone *edits* a Tenant to
use a different key, nothing was deleted, so `reconcileTenants` diffs the keys
it issued against the keys still declared and revokes the orphan.

---

### Lesson 10 — Stateful workloads and horizontal scaling

**Concept.** The bug that makes this project a good teacher.

**In this repo.** `internal/limiter/redis.go`, `RedisTokenBucket`, and the
`REDIS_URL` branch in `cmd/gateway/main.go`.

**Try it — this is the most convincing demo in the repo:**
```bash
make compose-up      # 3 replicas, one Redis, round-robin nginx in front
make compose-proof   # PASS: the limit holds at ~10 across all three

# now break it on purpose
# edit docker-compose.yml, comment out REDIS_URL, then:
make compose-down && make compose-up && make compose-proof
# FAIL: ~30 allowed against a capacity of 10
```

**The bit people miss.** Read the Lua script. Two details carry correctness:

*The clock comes from `redis.call("TIME")`, not from the calling pod.* If each
replica refilled against its own wall clock, skew would translate directly into
free tokens — a pod running 200ms fast credits 200ms of refill that never
happened, on every request.

*Read-refill-spend-write happens inside the script.* Doing it in Go would be a
read-modify-write across the network; two replicas interleaving there both see
the same balance and both spend it. Classic lost update, and it hands out
roughly double the limit exactly when load is highest.

**Interviewer asks:** *"You scaled from one replica to three and your rate
limiter got 3× more permissive. Why, and how do you fix it?"*

**Your answer:** Because the token buckets were process-local — each pod kept
its own jar, so a tenant on a 50-token plan could spend 150. That is not
approximation error, it is the limit being wrong by the replica count, and it
gets worse every time you scale. The fix is to move the bucket into shared
storage, which I did with a Lua script in Redis that does the whole
read-refill-spend-write atomically in one round trip. Two things I would flag:
it uses Redis's own `TIME` rather than the pod's clock, because clock skew
between replicas otherwise becomes free tokens; and it fails open, so if Redis
is unreachable the gateway serves unlimited rather than refusing everything —
an outage in the rate limiter should cost you rate limiting, not availability.
The alternative I considered was sticky routing by tenant so each tenant always
lands on one pod, which avoids Redis entirely, but it makes rebalancing and pod
loss lose state and it does not survive an autoscaling event.

---

### Lesson 11 — Prometheus, and cardinality as a production concern

**Concept.** Prometheus scrapes a text endpoint. Every distinct combination of
label values is a separate time series, held in memory and on disk.

**In this repo.** `internal/metrics/prometheus.go` and `handleMetrics` in
`handlers_k8s.go`.

**Try it.**
```bash
curl -s localhost:3000/metrics | grep '^limitplane_'
go test ./internal/metrics/ -v
```

**The bit people miss.** `maxTenantSeries = 100` and the `anon:` filter. Tenant
IDs include `anon:<ip>` — one per unique visitor. Exporting those as label
values is unbounded cardinality: a traffic spike from a botnet mints a hundred
thousand series and takes your Prometheus down. The thing being monitored
survives; the monitoring dies.

And label escaping. Tenant IDs are user-supplied. A quote in one produces a
malformed line, and a scraper drops the **entire scrape** on a parse error — not
just that series. `TestMetricsEscapesHostileTenantNames` covers it.

**Interviewer asks:** *"You want a per-customer metric. What could go wrong?"*

**Your answer:** Unbounded cardinality. Every distinct label combination is its
own time series, so a label whose values are user-controlled or effectively
infinite will kill a Prometheus server — and the irony is that the monitoring
dies while the monitored system is fine. In LimitPlane the tenant ID includes
`anon:<ip>` for unidentified visitors, so I export per-tenant series only for
*named* tenants, rank them by traffic, cap at 100, and export
`limitplane_tenant_series_dropped` so the truncation is visible rather than
silent. Anonymous traffic is still fully counted in the aggregates, so nothing
is actually lost. The related trap is escaping: tenant IDs are user-supplied, so
a quote character in one would emit a malformed line and make the scraper drop
every metric in the scrape.

---

### Lesson 12 — Gateway API and external authorization

**Concept.** Forward auth: the gateway pauses a request, asks a service you
nominate whether it may proceed, and honours the answer.

**In this repo.** `handleAuthz` in `handlers_k8s.go`;
`deploy/k8s/gateway-api/` for Envoy, NGINX and Traefik.

**Try it.**
```bash
curl -i localhost:3000/v1/authz/api/orders          # 200, with budget headers
for i in $(seq 1 12); do
  curl -s -o /dev/null -w '%{http_code} ' localhost:3000/v1/authz/api/orders
done; echo                                          # ...429
go test ./internal/server/ -run TestAuthz -v
```

**The bit people miss.** `TestAuthzKeysOnTheOriginalPathNotItsOwn`. Envoy calls
`<prefix><original path>`, so a request for `/orders` arrives as
`/v1/authz/orders`. Forget to strip the prefix and every request in the cluster
shares one bucket keyed on the literal string `/v1/authz` — the limiter appears
to work and is meaningless.

And `failOpen: true` in the SecurityPolicy. It decides what happens when
LimitPlane is unreachable: `true` means traffic flows unlimited, `false` means
every request 503s. There is no safe default, only a deliberate choice.

**Interviewer asks:** *"How would you add rate limiting to twelve services owned
by five teams?"*

**Your answer:** Not by asking five teams to add a library. I would put it at the
gateway they already share, using Envoy's external authorization filter: the
gateway asks one HTTP question per request — "may this proceed?" — and honours
the answer. That is twelve lines of `SecurityPolicy` and none of the twelve
services change, redeploy, or even know. The trade-offs I would raise: it puts
one service in the hot path of everything, so it needs a tight timeout (250ms
here) and a circuit breaker, or a slow limiter becomes a slow everything; and
`failOpen` has to be decided deliberately, because unmetered traffic and total
downtime are both real costs and which one is worse depends on what the traffic
does. For per-token LLM billing I might well choose fail-closed.

---

## Interview defense

The lessons above each end with one question. Here are the rest — the ones that
come after "so you put it on Kubernetes?"

<details>
<summary><b>1. Why did you write your own Kubernetes client instead of using client-go?</b></summary>

Because the project has a hard zero-dependency constraint enforced in CI, and
client-go is the largest dependency tree in the Go ecosystem — roughly 40
modules for what is, underneath, a REST API. LIST is a GET, WATCH is a GET that
streams newline-delimited JSON, an update is a PATCH. All of it is reachable
from `net/http` and `encoding/json`.

I would not make the same call at work. What client-go gives you that I gave up
is real: shared informer caches, typed clientsets, workqueues with rate limiting
and per-key deduplication, and years of hardening. If I were shipping an
operator on a team, kubebuilder and controller-runtime, without hesitation.

What I got instead was understanding. I now know what a reflector *is*, because
I had to handle 410 Gone myself and discover it arrives in-band as an ERROR
event rather than an HTTP status.
</details>

<details>
<summary><b>2. What is the hardest bug you hit?</b></summary>

Leader election that was silently, completely dead — and the reason it was
*silent* mattered more than the bug itself.

The symptom was that `kubectl get rlp` showed empty columns. Status was never
written back. Every unit test passed, including tests specifically covering
leader election against a fake API server, and the operator's logs said nothing
at all.

The cause: `Lease.spec.renewTime` is `metav1.MicroTime`, not the ordinary
`metav1.Time` used almost everywhere else in Kubernetes. MicroTime parses with
a layout of `2006-01-02T15:04:05.000000Z07:00`, and `.000000` in a Go layout
means **exactly six** fractional digits. I used `time.RFC3339Nano`, which emits
up to nine and strips trailing zeros — so every write was rejected:

```
400 BadRequest: Lease in version "v1" cannot be handled as a Lease:
parsing time "2026-09-05T07:45:43.938803641Z" as
"2006-01-02T15:04:05.000000Z07:00": cannot parse "641Z" as "Z07:00"
```

Two lessons, and I would lead with the second.

**My fake API server was more permissive than the real one.** It accepted any
string as a timestamp, so it certified broken code. A fake that is more
forgiving than production is worse than no fake, because it converts "untested"
into "tested and wrong". The fake now parses MicroTime and returns a 400
exactly as Kubernetes does, and the fix is verified by reverting it and
watching the suite go red.

**The error was logged at `Debug`.** A total failure of the operator's entire
write path produced no output at the default level. I changed it to `Warn`
first, on general principle, and the very next cluster run printed the exact
400 in one line — which is how the bug was found at all. Making the failure
loud was the fix that found the fix.

</details>

<details>
<summary><b>2b. What else only showed up on a real cluster?</b></summary>

Three more, all of which had passed a green unit suite:

**A 30-second watch timeout.** `http.Client.Timeout` bounds the entire request
*including reading the body*, and a watch is a body held open for minutes. Every
watch died at exactly 30s and reconnected forever, never receiving a bookmark
and re-LISTing on every cycle. It looked healthy from outside because
reconciliation still ran on each reconnect. Fixed with a separate client that
has no timeout; the regression test asserts the field structurally, because a
behavioural test would add 30 seconds to every CI run.

**A data race on the probe path.** `o.elector` was assigned inside `Run()`,
which is its own goroutine, while `/readyz` and `/metrics` read it on theirs —
a write racing every kubelet probe and every Prometheus scrape. Production does
not run under `-race`, so it would have surfaced as a nil dereference under
load. Fixed by constructing the elector in `New()`, before anything can read it.

**A crash loop caused by my own dependency ordering.** The gateway exited if
Redis was not reachable at boot. In Kubernetes the Redis StatefulSet and the
Deployment are created in the same instant, so on a cold namespace Redis is
reliably not ready — the pod crash-looped twice on every single deploy and
recovered only because the kubelet kept restarting it. It also contradicted the
design: the limiter fails *open* at runtime, so failing *closed* at boot was
incoherent. It now waits with backoff and stays NotReady, which means it
receives no traffic and comes up cleanly — and it still gives up eventually,
because booting on a typo'd `REDIS_URL` would mean enforcing nothing while
reporting itself healthy.

The through-line: every one of these was a gap between my test doubles and a
real API server. Unit tests prove the parts; only a cluster proves the contract.

</details>

<details>
<summary><b>2c. The status write loop</b></summary>

The status write loop. The operator patches `status` on the objects it manages,
and that patch comes back as a `MODIFIED` watch event. My first version
reconciled on it, which wrote status again, which produced another event. With
three replicas all doing it, that is a self-inflicted DoS on the API server.

The fix is `metadata.generation`, which increments *only* on a spec change, so
an event whose generation matches what I already hold is my own echo and can be
skipped. There is a second layer too: `patchStatus` hashes the status and skips
the write when nothing a reader would care about changed — with `appliedAt` and
`lastSeen` excluded from the hash, since they move every tick and would make
every write look like a change. `TestStatusEchoDoesNotTriggerAReconcile` and
`TestStatusIsWrittenOnceUntilSomethingChanges` lock both in.
</details>

<details>
<summary><b>3. Why do all replicas watch, but only the leader writes?</b></summary>

Because they are two different needs and most tutorials conflate them. Each pod
serves live traffic, so each pod needs current tiers and API keys in its own
memory — gating the watches behind leader election would leave the followers
enforcing stale limits, which is worse than being down because it is silent.
But status patches and Events go to the API server, and three replicas writing
the same field is the write-amplification loop above. So: read widely, write
once.
</details>

<details>
<summary><b>4. How do you handle a Tenant that references a tier that doesn't exist?</b></summary>

I treat it as a normal transient state, not an error. Two independent objects
have no ordering guarantee, so a Tenant genuinely can arrive before the
RateLimitPolicy defining its tier — especially under GitOps, which applies a
directory in whatever order it likes. The Tenant simply is not applied, a
`Ready=False` condition with reason `UnknownTier` is written so the state is
visible, and the next reconcile picks it up. No retry queue and no backoff,
because reconciliation is level-triggered: it recomputes from the full cache
every time, so the fix is automatic. `TestTenantBeforeItsPolicyConvergesLater`
covers exactly this.
</details>

<details>
<summary><b>5. What happens when the API server is unreachable for ten minutes?</b></summary>

Traffic keeps flowing. The gateway serves from whatever configuration it last
had, so limits stay enforced at their last-known values.

Concretely: the reflector's watch fails and it retries with exponential backoff
and full jitter, capped at 30 seconds. The leader cannot renew its lease within
`RenewDeadline` (10s), so it cancels its own leader context and *stops writing* —
standing down on an API outage is correct, because otherwise it would still
believe it leads while another replica takes over. Status goes stale, which is
the right thing to sacrifice. When the API server returns, the cursor is almost
certainly outside the history window, so the watch gets a 410, drops the cursor,
re-LISTs, and converges.

The one thing that would *not* survive is a pod restart during the outage, since
readiness gates on the initial sync — but a pod that cannot reach the API server
also cannot learn its configuration, so refusing traffic is the honest answer.
</details>

<details>
<summary><b>6. Why is there no CPU limit on the container?</b></summary>

Because CPU limits are enforced by CFS throttling, and throttling adds tail
latency to a component whose entire job is answering in microseconds. A pod at
its CPU limit is stalled until the next scheduling period even when the node is
idle. The CPU *request* already guarantees a floor under contention, which is
the property I actually want. Memory is different — it is incompressible, so
there is a memory limit, and exceeding it is an OOMKill rather than a slowdown.
</details>

<details>
<summary><b>7. Your Deployment has a PodDisruptionBudget and a topology spread constraint. Aren't those the same thing?</b></summary>

No, and assuming so is a common gap. A PDB constrains *voluntary* disruption:
node drains, cluster upgrades, autoscaler scale-downs. Without it, `kubectl
drain` happily evicts all three replicas at once during routine maintenance. It
does nothing about a node dying — that is involuntary, and the API server cannot
negotiate with a dead node.

Involuntary loss is what the topology spread constraint addresses, by keeping
the replicas on different nodes so one node's failure does not take all of them.
Complementary, not redundant. The related footgun is `minAvailable >=
replicaCount`, which makes the budget unsatisfiable and hangs every drain
forever — the Helm chart fails the install with an explanation rather than
letting you find that at 2am.
</details>

<details>
<summary><b>8. Walk me through what happens on `kubectl apply -f ratelimitpolicy.yaml`.</b></summary>

kubectl POSTs to `/apis/limitplane.dev/v1alpha1/namespaces/limitplane/ratelimitpolicies`.
The API server authenticates, runs authorization (RBAC), then admission:
mutating webhooks, then schema validation against the CRD's OpenAPI schema plus
its CEL rules, then validating webhooks. If `refillPerSecond: 0` it is rejected
here and never stored. Otherwise it is written to etcd, which bumps
`resourceVersion` and — because `spec` changed — `generation`.

Every watcher on that resource receives a `MODIFIED` event within milliseconds.
My reflector decodes it, sees the generation is new, updates its cache, and
calls `reconcile()`, which recomputes desired tiers and routes from *all* cached
policies and writes them into the live `policy.Policy` under its lock. The next
request through the gateway resolves against the new numbers. No restart.

Separately, on the leader's next status tick (15s), `status` is patched with the
observed generation, conditions, and live counters — so `kubectl get rlp` shows
allowed/blocked traffic.
</details>

<details>
<summary><b>9. How would you upgrade the CRD from v1alpha1 to v1beta1?</b></summary>

Add `v1beta1` to `versions` with `served: true`, and move `storage: true` onto
it — exactly one version may be the storage version. If the schemas are
structurally compatible, that plus `kubectl explain` is most of the work.

If they are not, you need a conversion webhook: the API server calls it to
convert between versions on read and write, and every object is stored in the
storage version. The subtlety is that existing objects in etcd are still stored
in the *old* version until something rewrites them, so you run a storage
migration (read and re-write every object) before you can drop the old version
from the CRD, and `status.storedVersions` tells you when it is safe.

For this project I would not do any of that yet — v1alpha1 is an honest label
and I would rather break the schema than carry a conversion webhook for two
users.
</details>

<details>
<summary><b>10. Why `emptyDir` and not a PersistentVolume?</b></summary>

Because everything on disk is a cache that is rebuilt from the custom resources
on boot: the tenant table, the org directory, the RAG memory. Surviving a
reschedule would actively be wrong — a pod that came back with a stale tenant
file could serve a key that has since been revoked.

Redis is the same reasoning: every key is a token bucket with a short TTL, so
losing them means every tenant refills to full. That is a moment of generosity,
not a correctness failure, and it self-heals within one refill period. Paying
for a PVC plus its backup story to protect data with a ten-minute lifetime is
the wrong trade.

The thing that *would* need durable storage is the monthly quota meter, since
that is billing state — and being honest, it is in memory today. That is a real
gap I would fix by moving it into Redis with persistence enabled, or into
Postgres, before charging anyone money on it.
</details>

<details>
<summary><b>11. What does `restricted` Pod Security Admission actually enforce?</b></summary>

It is a built-in admission controller with three profiles — privileged,
baseline, restricted — set per namespace by label. `restricted` requires
`runAsNonRoot`, `allowPrivilegeEscalation: false`, dropping ALL capabilities, a
`RuntimeDefault` or `Localhost` seccomp profile, and no host namespaces, host
paths, or host ports.

It matters that the namespace label is `enforce` and not just `warn`: a pod that
violates it is rejected by the API server, so the `securityContext` in the
Deployment is verified by the cluster rather than asserted in YAML nobody reads.
One detail that trips people: the kubelet can only enforce `runAsNonRoot` if it
knows the image's user is non-root, which is why the Dockerfile sets a *numeric*
`USER 65534:65534` — a username would require resolving `/etc/passwd`, and a
scratch image does not have one.
</details>

<details>
<summary><b>12. Your NetworkPolicy — how do you know it works?</b></summary>

By testing it, because the object being accepted proves nothing. NetworkPolicy
is enforced by the CNI plugin, not by Kubernetes: on Calico or Cilium these
rules are real, and on kind's default kindnet or plain flannel they are silently
accepted and ignored. Applying them and assuming protection is worse than not
applying them, so the dev overlay deletes them rather than shipping a false
sense of security, and the Helm chart defaults `networkPolicy.enabled` to false.

To verify, I would exec into a pod in another namespace and try to reach
port 6379 — it should hang and time out. The classic bug I did guard against: the
moment a policy has an `Egress` section, everything not listed is denied,
*including cluster DNS*. Forget the kube-dns rule and every outbound lookup
fails, and the symptom looks like total application failure rather than a
firewall rule.
</details>

<details>
<summary><b>13. Three replicas share one Redis. Isn't that just moving the single point of failure?</b></summary>

Yes, and I would say so rather than pretend otherwise. The bundled Redis is one
replica with no persistence — fine for a demo, not for production.

What makes it survivable is that the limiter fails *open*: if Redis is
unreachable, `CheckBurst` returns an error and the gateway allows the request
with the error surfaced. So a Redis outage costs you rate limiting, not
availability. For production the answer is a managed Redis or Sentinel, which is
why `redis.externalUrl` exists and the prod overlay points at one.

The deeper answer is that a rate limiter is inherently a coordination problem,
and you are choosing where to pay: shared store (a network hop and a SPOF),
sticky routing (no coordination but state lost on rebalance), or per-replica
budgets (limit divided by replica count, which breaks under autoscaling). For
this workload the shared store is the right trade, and fail-open is what makes
its failure mode acceptable.
</details>

<details>
<summary><b>14. How does the operator handle two RateLimitPolicy objects defining the same tier?</b></summary>

Last one wins by namespace/name, and the sort in `reconcile()` is what makes
that a *decision* rather than an accident. Go randomises map iteration, so
without the sort, which policy won would differ between replicas — and two
replicas enforcing different limits for the same tier is a genuinely miserable
bug to diagnose.

If I were hardening this I would go further: Gateway API's approach is to
resolve conflicts by creation timestamp with name as a tiebreak, and to set an
`Accepted=False` condition on the loser so the conflict is visible on the object
rather than only in someone's head.
</details>

<details>
<summary><b>15. Why is your reconcile loop synchronous instead of using a workqueue?</b></summary>

Because reconciliation here is cheap — it recomputes maps in memory and writes
them under a lock — and the object count is small. A workqueue's real value is
deduplicating bursts of events for the same key and giving you per-item rate
limiting and retry backoff.

I get deduplication a different way: the generation check drops status echoes
before any work, and level-triggered reconciliation means a burst of ten events
produces the same end state as one. What I genuinely lack is backoff on a
*failed* reconcile — a Secret that is missing is retried only on the next event
or status tick, not on an increasing delay. If reconciliation got expensive or
started calling external APIs, a workqueue would be the first thing I added, and
it is the honest limitation of doing this without controller-runtime.
</details>

<details>
<summary><b>16. What is a BOOKMARK event and why should you care?</b></summary>

A watch event carrying no object worth reconciling — only a fresher
`resourceVersion`. The API server sends them so an idle watcher's cursor does
not age out of the history window.

You care because without `allowWatchBookmarks=true`, a controller watching a
resource nobody touches holds a cursor that gets steadily older, and every
reconnect costs a full re-LIST of every object. On a large cluster with many
controllers that is real API server load caused entirely by not setting a query
parameter. The corresponding bug is passing bookmarks to your handler: they
carry no meaningful object, so a controller that reconciles on them is
reconciling on a heartbeat. `TestReflectorBookmarkAdvancesCursorSilently`
asserts both halves.
</details>

<details>
<summary><b>17. Why does Helm install CRDs from `crds/` instead of `templates/`?</b></summary>

Because Helm treats `crds/` specially: it installs those files once, before
templates, and never upgrades or deletes them. That asymmetry is a deliberate
safety property — deleting a CRD deletes *every object of that kind* in the
cluster, so `helm uninstall` must not be able to wipe every rate limit policy
you have.

The cost is that a CRD schema change needs `kubectl apply -f` by hand, which is
a real operational wart. The alternative is putting them in `templates/` with
`helm.sh/resource-policy: keep`, which gets you upgrades and keeps the deletion
guard, at the price of the CRD no longer being guaranteed to exist before the
custom resources that reference it. I went with `crds/` and used post-install
hooks to order the sample resources after it.
</details>

<details>
<summary><b>18. What is server-side apply and did you use it?</b></summary>

It is a patch mode where the API server tracks *which field manager owns which
field*, stored in `metadata.managedFields`. Two controllers editing different
fields of one object no longer clobber each other, and a conflicting write is
rejected unless you pass `force`.

The client supports it — `Client.Apply`, content type
`application/apply-patch+yaml`. A detail I liked: the content type says YAML,
but every JSON document is valid YAML, so server-side apply works with no YAML
encoder at all, which matters given the zero-dependency rule.

The operator does not currently use it, because it only writes `status`, and the
status subresource already gives the isolation I need. Where it would earn its
place is if LimitPlane started managing objects a human also edits — a generated
ConfigMap, say — since that is exactly the shared-ownership problem SSA exists
to solve.
</details>

<details>
<summary><b>19. Explain the difference between `kubectl apply`, `create`, `replace`, and `patch`.</b></summary>

`create` fails if the object exists. `replace` overwrites it wholesale and needs
the current `resourceVersion`, so it fails on a concurrent edit — that
compare-and-swap is exactly what leader election relies on. `patch` sends a
partial update in one of three flavours: merge patch (replaces the fields you
name; a list is replaced entirely), JSON patch (a list of ops including `test`,
which is how you remove a finalizer safely), and strategic merge (knows how to
merge Kubernetes lists by key, so it can merge `env` entries by name).

`apply` is declarative: it computes a diff against the last-applied state, which
client-side lives in an annotation and server-side lives in `managedFields`.
That is why `apply` can remove a field you deleted from your file while `patch`
cannot.

All four are in this repo: `Create` for the Lease, `Update` (replace) for the
lease CAS, merge patch for status, JSON patch for finalizers, and SSA in
`Apply`.
</details>

<details>
<summary><b>20. How would you debug a pod stuck in CrashLoopBackOff?</b></summary>

`kubectl describe pod` first — it shows the last state, the exit code, and the
events. Exit 137 is a SIGKILL, usually OOMKilled, which `describe` confirms
under Last State. Then `kubectl logs --previous`, since the current container
may not have logged anything yet.

The specific one worth naming: a crash loop caused by the *probes themselves*.
If a process takes 20 seconds to start and the liveness probe begins at 10, the
kubelet kills it before it is ever ready, forever, and the logs show a healthy
startup every time. That is what `startupProbe` exists for — it holds liveness
off until the process is up — and it is why this Deployment has one.

For this image there is an extra wrinkle: it is `scratch`, so there is no shell
and `kubectl exec` cannot help. That is why the version is stamped into
`limitplane_build_info` on `/metrics` — it is the only way to find out what a
pod is actually running.
</details>

<details>
<summary><b>21. Your image is `FROM scratch`. What do you lose?</b></summary>

Debuggability, mostly. No shell, so no `kubectl exec -it -- sh`. No package
manager, no `curl`, no `ps`. If something is wrong inside a running pod, the
usual toolkit is gone.

What I gain is a ~7.5MB image with no OS packages, and therefore no CVEs from
any of them — a meaningful thing for something that terminates untrusted
traffic. The workarounds: the binary is its own health probe (`limitplane
healthcheck`) so Docker's HEALTHCHECK works without a shell; the version is on
`/metrics`; and for real debugging, `kubectl debug --image=busybox
--target=gateway` attaches an ephemeral container sharing the pod's namespaces,
which gets you a shell without putting one in the image.

The one thing scratch genuinely requires is copying `ca-certificates.crt` in —
without it, every outbound HTTPS call fails with an x509 error that reads like a
code bug rather than a missing file.
</details>

<details>
<summary><b>22. How do you know any of this works?</b></summary>

Three layers, and the CI job names them.

Unit tests run the reflector against a fake API server that scripts LIST and
WATCH responses, including an in-band 410 so the recovery path is exercised
rather than assumed. Leader election is tested against a fake that implements
`resourceVersion` compare-and-swap the way the real API server does, so
`TestConcurrentTakeoverLosesOnConflict` is testing the actual mechanism.

Integration tests drive the real object graph over real HTTP — that is where the
ext_authz path-stripping and the metrics cardinality cap are covered.

Then the `e2e` CI job creates a real kind cluster and asserts the things only a
real API server can show: that the CRD schema rejects `refillPerSecond: 0`, that
the CEL rule rejects a Tenant with no key, that a lease is actually acquired,
that a Secret-backed key is really read from the Secret, that deleting a Tenant
completes (so the finalizer really released), and that `kubectl patch` changes a
live rate limit with the pod's restart count unchanged.

That last assertion is the one I care about most, because "no restart" is the
whole claim.
</details>

<details>
<summary><b>23. What would you do differently if you started again?</b></summary>

Use kubebuilder. Not because what I wrote is wrong, but because on a team the
right question is not "can I write a reflector?" — it is "what will the next
person maintain?" controller-runtime gives you informer caches, workqueues with
proper backoff, and a testing harness, and those are not things worth
reimplementing on company time.

Technically, three things. I would add a workqueue so failed reconciles retry
with backoff instead of waiting for the next event. I would move the monthly
quota meter out of memory before anyone bills on it. And I would write a
validating admission webhook for the cross-object rules the CRD schema cannot
express — a Tenant referencing a nonexistent tier is currently only reported in
status, and it could be rejected at apply time.
</details>

<details>
<summary><b>24. Sell me on this in thirty seconds.</b></summary>

LimitPlane is an AI-aware rate limiter: it prices an inference call five times a
page load, so one heavy user cannot quietly run up your GPU bill. On Kubernetes
it is two things at once — a gateway, and an operator that makes rate limits
first-class API objects, so a limit change is a reviewed pull request rather
than an SSH session, and it takes effect in under a second with no restart.
It exposes Envoy's ext_authz contract, so it protects every service behind your
gateway without any of them changing. The operator is about a thousand lines
against the raw Kubernetes API, no client-go, because the project has a
zero-dependency rule — which means I can explain every part of it, including
the parts I got wrong first.
</details>

---

## What this deliberately does not do

Being able to name the limits is what separates someone who built a thing from
someone who followed a tutorial. Say these before you are asked.

- **No admission webhook.** Cross-object validation — a Tenant naming a tier no
  policy defines — is reported in `status`, not rejected at apply time. A
  webhook would need a TLS certificate, a CA bundle injected into the
  configuration, and a failure policy, and a webhook that is down blocks writes
  to the resources it guards. Real work, deliberately deferred.

- **No workqueue.** Reconciles are synchronous and a failed one waits for the
  next event or status tick rather than retrying with backoff. Fine at this
  object count and this cost; the first thing to fix if either grows.

- **Monthly quota is in memory.** Burst buckets are shared through Redis; the
  monthly meter is not. It is per-pod, so with three replicas a tenant can
  exceed a monthly plan by up to 3×, and it resets on restart. Nobody should
  bill on it as it stands. Fixing it means Redis with persistence, or Postgres.

- **Bundled Redis is a single point of failure**, with no persistence and no
  auth. Its blast radius is bounded by fail-open, but production wants Sentinel
  or a managed instance.

- **NetworkPolicy is untested here.** kind's default CNI ignores it, so the dev
  overlay removes it rather than shipping the appearance of protection.

- **No mTLS between the gateway and Redis**, and no Redis password. Inside a
  cluster with a working NetworkPolicy that is defensible; on a shared cluster
  it is not.

- **`v1alpha1` means what it says.** No conversion webhook, no stability
  promise. Breaking the schema is currently cheaper than maintaining a
  conversion path.

- **Not tested at scale.** The limiter benchmarks at 18.7M checks/sec in memory,
  but the Redis path adds a network round trip and I have not load-tested the
  operator against thousands of custom resources.

---

## Glossary

| Term | What it actually means |
|---|---|
| **Reconcile** | Make the world match the declared spec. Not "handle this event". |
| **Level-triggered** | Act on current state, not on the transition that woke you. |
| **`resourceVersion`** | Opaque cursor into the change stream. Never parse it. |
| **`generation`** | Increments only on a spec change. `resourceVersion` changes on any write. |
| **410 Gone** | Your cursor is older than the server's history. Re-LIST; do not retry. |
| **BOOKMARK** | A watch event carrying only a fresher cursor. Advance it, reconcile nothing. |
| **Finalizer** | A string that blocks deletion until your controller removes it. |
| **Subresource** | A separate endpoint (`/status`, `/scale`) with its own RBAC. |
| **Lease** | The `coordination.k8s.io` object leader election is built on. |
| **CEL** | Expression language for cross-field validation in a CRD schema. |
| **ext_authz** | Envoy's filter that asks an external service to allow or deny each request. |
| **Cardinality** | Number of distinct label combinations. Unbounded cardinality kills Prometheus. |
| **PDB** | Constrains *voluntary* disruption only. Node death is not voluntary. |
| **PSA** | Pod Security Admission. Namespace-labelled enforcement of pod security. |
| **Fail open / closed** | On dependency failure: allow everything, or refuse everything. |

---

## Where to go next

Once this is comfortable, the natural progressions, in order of value:

1. **Rebuild the operator with kubebuilder.** Same CRDs, same behaviour,
   controller-runtime underneath. You will recognise every piece, and being able
   to compare them is worth more than either alone.
2. **Write the validating admission webhook** for the cross-object rules.
3. **Install kube-prometheus-stack** and wire up the ServiceMonitor and the
   alerts in `deploy/k8s/observability/`. Make the split-brain alert fire on
   purpose by disabling leader election.
4. **Add prometheus-adapter** and turn on the commented-out custom-metric HPA,
   so it scales on requests per second rather than CPU.
5. **Deploy Envoy Gateway** and put a real service behind the SecurityPolicy.
6. **CKA / CKAD**, if you want the certificate. You will have met most of the
   syllabus here, in a context that makes it stick.
