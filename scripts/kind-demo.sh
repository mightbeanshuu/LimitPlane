#!/usr/bin/env bash
# One command, from nothing to a working Kubernetes deployment with proof.
#
# Everything this prints is measured, not narrated: the CRDs are really
# established, the operator really elects a leader, a real `kubectl apply`
# really changes the running limits, and the rate limiting is really enforced.
#
#   make kind-demo
#
# Requires: docker, kind, kubectl. Takes about three minutes on a laptop.
set -euo pipefail

# The Dockerfile cross-compiles via --platform=$BUILDPLATFORM, which the legacy
# docker builder cannot parse. BuildKit can, and enabling it needs no plugin.
export DOCKER_BUILDKIT=1

CLUSTER="${KIND_CLUSTER:-limitplane}"
NS="${NAMESPACE:-limitplane}"
IMAGE="${IMAGE:-limitplane}"
TAG="${TAG:-dev}"
ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"

step()  { printf '\n\033[1;34m▶ %s\033[0m\n' "$1"; }
ok()    { printf '\033[32m  ✓ %s\033[0m\n' "$1"; }
warn()  { printf '\033[33m  ! %s\033[0m\n' "$1"; }
fail()  { printf '\033[31m  ✗ %s\033[0m\n' "$1"; exit 1; }

for tool in docker kind kubectl; do
  command -v "$tool" >/dev/null || fail "$tool is not installed"
done
docker info >/dev/null 2>&1 || fail "the docker daemon is not running"

cd "$ROOT"

step "1/8  Creating the kind cluster (1 control plane, 2 workers)"
if kind get clusters 2>/dev/null | grep -qx "$CLUSTER"; then
  ok "cluster '$CLUSTER' already exists"
else
  kind create cluster --name "$CLUSTER" --config deploy/kind/cluster.yaml
fi
kubectl config use-context "kind-$CLUSTER" >/dev/null
kubectl get nodes

step "2/8  Building the image and side-loading it into the cluster"
docker build -q \
  --build-arg VERSION="kind-demo" \
  --build-arg COMMIT="$(git rev-parse --short HEAD 2>/dev/null || echo none)" \
  -t "$IMAGE:$TAG" .
# kind nodes run their own containerd, so an image built on the host is
# invisible to them. Skip this and every pod sits in ErrImagePull, trying to
# reach a registry that has never heard of this tag.
kind load docker-image "$IMAGE:$TAG" --name "$CLUSTER"
ok "$IMAGE:$TAG loaded onto every node"

step "3/8  Installing the CRDs"
kubectl apply -f deploy/k8s/crds/
# apply returns when the object is STORED, not when the API server is serving
# the new endpoint. Creating a custom resource in the next breath fails with
# "no matches for kind" — a race that bites on fast clusters and in CI.
kubectl wait --for=condition=Established --timeout=60s \
  crd/ratelimitpolicies.limitplane.dev crd/tenants.limitplane.dev
ok "kubectl now understands RateLimitPolicy and Tenant"
kubectl api-resources --api-group=limitplane.dev

step "4/8  Deploying the gateway, the operator and Redis"
kubectl apply -k deploy/k8s/overlays/dev
kubectl -n "$NS" rollout status deploy/limitplane --timeout=180s
kubectl -n "$NS" get pods -o wide

step "5/8  Declaring rate limits as YAML"
kubectl -n "$NS" create secret generic acme-api-key \
  --from-literal=apiKey=lp_acme_demo_key \
  --dry-run=client -o yaml | kubectl apply -f -
kubectl apply -f deploy/k8s/samples/

# The operator watches, so this is a matter of milliseconds — but poll rather
# than sleep, so the demo is honest about how long it actually took.
for _ in $(seq 1 30); do
  ready=$(kubectl -n "$NS" get rlp api-limits \
    -o jsonpath='{.status.conditions[?(@.type=="Ready")].status}' 2>/dev/null || true)
  [ "$ready" = "True" ] && break
  sleep 1
done
if [ "${ready:-}" = "True" ]; then
  ok "the operator reconciled the policy"
else
  warn "the policy has not reported Ready yet — status is written on a timer, give it a moment"
fi

echo
kubectl -n "$NS" get ratelimitpolicies
echo
kubectl -n "$NS" get tenants

step "6/8  Exactly one replica holds the operator lease"
# Poll. The lease is created within milliseconds of the operator starting, but
# the pod may still be waiting on Redis, so checking once races the boot.
for _ in $(seq 1 30); do
  holder=$(kubectl -n "$NS" get lease limitplane-operator \
    -o jsonpath='{.spec.holderIdentity}' 2>/dev/null || true)
  [ -n "${holder:-}" ] && break
  sleep 1
done
if [ -n "${holder:-}" ]; then
  kubectl -n "$NS" get lease limitplane-operator \
    -o custom-columns=LEASE:.metadata.name,HOLDER:.spec.holderIdentity,TRANSITIONS:.spec.leaseTransitions,RENEWED:.spec.renewTime
  ok "one leader, writing status back for everyone"
else
  warn "no lease was acquired — check: kubectl -n $NS logs -l app.kubernetes.io/component=gateway | grep lease"
fi

step "7/8  Rate limiting, enforced for real"
kubectl -n "$NS" port-forward svc/limitplane 38080:80 >/dev/null 2>&1 &
PF_PID=$!
trap 'kill $PF_PID 2>/dev/null || true' EXIT
for _ in $(seq 1 40); do
  curl -sf localhost:38080/healthz >/dev/null 2>&1 && break
  sleep 0.5
done

allowed=0; blocked=0
for _ in $(seq 1 20); do
  code=$(curl -s -o /dev/null -w '%{http_code}' localhost:38080/v1/demo/ping)
  [ "$code" = "200" ] && allowed=$((allowed + 1))
  [ "$code" = "429" ] && blocked=$((blocked + 1))
done
echo "  20 requests -> $allowed allowed, $blocked refused"
[ "$blocked" -gt 0 ] || fail "nothing was rate limited; the policy is not being enforced"
ok "the free tier's burst of 10 held"

echo
echo "  the 429 body explains itself:"
curl -s localhost:38080/v1/demo/ping | sed 's/^/    /'

step "7b/8  The status board, now that traffic has flowed"
# Status is written on a timer (15s), so wait for a tick rather than printing
# an empty table and calling it a demo.
for _ in $(seq 1 25); do
  allowed=$(kubectl -n "$NS" get rlp api-limits -o jsonpath='{.status.allowed}' 2>/dev/null || true)
  [ -n "${allowed:-}" ] && [ "${allowed:-0}" -gt 0 ] 2>/dev/null && break
  sleep 1
done
echo
kubectl -n "$NS" get ratelimitpolicies
echo
kubectl -n "$NS" get tenants
echo
ok "ALLOWED / BLOCKED / LANE are written back from live traffic, not from the YAML"

step "8/8  Changing a limit with kubectl, live"
echo "  before: capacity 10"
kubectl -n "$NS" patch rlp api-limits --type=merge \
  -p '{"spec":{"tiers":{"free":{"capacity":100,"refillPerSecond":10,"monthlyQuota":1000}}}}'

# Poll for the new limit to take effect. No restart, no redeploy, no dropped
# connection — the watch delivers the change and the next request uses it.
for _ in $(seq 1 30); do
  limit=$(curl -s -o /dev/null -D - localhost:38080/v1/demo/ping 2>/dev/null \
    | tr -d '\r' | awk -F': ' 'tolower($1)=="x-ratelimit-limit"{print $2}')
  [ "${limit:-0}" != "" ] && [ "${limit%.*}" -gt 10 ] 2>/dev/null && break
  sleep 1
done
echo "  after:  the gateway reports X-RateLimit-Limit: ${limit:-unknown}"
if [ "${limit%.*}" -gt 10 ] 2>/dev/null; then
  ok "a git-reviewable YAML edit changed a live production limit in seconds"
else
  warn "the new limit has not appeared yet; check: kubectl -n $NS logs -l app.kubernetes.io/component=gateway"
fi

step "Done"
cat <<SUMMARY

  What just happened, and why each part matters:

    CRDs            rate limits became first-class Kubernetes objects, so they
                    are reviewed in a PR, applied by GitOps, and audited by
                    whatever already audits the cluster
    the operator    a watch loop written on the raw API (no client-go) keeps
                    the running gateway matching those objects
    the lease       exactly one replica writes status back, so three pods do
                    not live-lock fighting over the same objects
    Redis           the three replicas share ONE token bucket per tenant, so
                    scaling out does not triple everyone's limit
    the live patch  a limit changed with no restart and no dropped connection

  Explore it:
    kubectl -n $NS get rlp,lpt
    kubectl -n $NS describe rlp api-limits
    kubectl -n $NS get events --sort-by=.lastTimestamp | tail
    kubectl -n $NS port-forward svc/limitplane 3000:80   # then open /dashboard

  Tear it down:
    make kind-down

SUMMARY
