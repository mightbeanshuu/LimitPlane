#!/usr/bin/env bash
# Prove that three replicas enforce ONE budget, not three.
#
# This is the single most important claim in the Kubernetes layer, and the one
# most often asserted without evidence. The measurement is simple: send more
# requests than one tenant's plan allows, through a load balancer that spreads
# them across all three replicas, and count the outcomes.
#
#   shared buckets  -> allowed == the tier capacity, no matter how many replicas
#   local buckets   -> allowed == capacity x replicas, and the limit is a lie
set -euo pipefail

BASE="${BASE:-http://localhost:8080}"
ROUTE="${ROUTE:-/v1/demo/ping}"
REQUESTS="${REQUESTS:-40}"
CAPACITY="${CAPACITY:-10}"   # the free tier's burst, from cmd/gateway

blue()  { printf '\033[34m%s\033[0m\n' "$1"; }
green() { printf '\033[32m%s\033[0m\n' "$1"; }
red()   { printf '\033[31m%s\033[0m\n' "$1"; }

blue "── waiting for the load balancer ──"
for _ in $(seq 1 60); do
  curl -sf "$BASE/healthz" >/dev/null 2>&1 && break
  sleep 1
done
curl -sf "$BASE/healthz" >/dev/null || { red "gateway never became healthy at $BASE"; exit 1; }

blue "── which replicas are answering ──"
# nginx echoes the upstream it chose in X-Served-By, so this shows the requests
# genuinely spreading rather than all landing on one container.
for _ in $(seq 1 6); do
  curl -s -o /dev/null -D - "$BASE/healthz" 2>/dev/null | grep -i '^x-served-by' || true
done | sort | uniq -c

blue ""
blue "── firing $REQUESTS requests at $ROUTE as one anonymous tenant ──"
allowed=0
blocked=0
for _ in $(seq 1 "$REQUESTS"); do
  code=$(curl -s -o /dev/null -w '%{http_code}' "$BASE$ROUTE")
  case "$code" in
    200) allowed=$((allowed + 1)) ;;
    429) blocked=$((blocked + 1)) ;;
    *)   red "unexpected status $code"; exit 1 ;;
  esac
done

echo
echo "  allowed: $allowed"
echo "  blocked: $blocked"
echo "  tier capacity: $CAPACITY"
echo

# Some slack upward: the bucket refills at 1 token/sec while the loop runs, so
# a slow run legitimately allows a few extra. What must NOT happen is allowed
# landing anywhere near capacity x 3.
upper=$((CAPACITY + 10))
triple=$((CAPACITY * 2))

if [ "$allowed" -ge "$triple" ]; then
  red "FAIL: $allowed requests allowed against a capacity of $CAPACITY."
  red "      The replicas are NOT sharing a bucket — each is keeping its own,"
  red "      so every tenant is getting roughly one limit per replica."
  red "      Check that REDIS_URL is set on the gateway service."
  exit 1
fi

if [ "$allowed" -le "$upper" ] && [ "$blocked" -gt 0 ]; then
  green "PASS: the limit held at ~$CAPACITY across all three replicas."
  green "      One tenant, one bucket, however many pods are serving."
else
  red "INCONCLUSIVE: allowed=$allowed blocked=$blocked (expected ~$CAPACITY allowed)."
  exit 1
fi
