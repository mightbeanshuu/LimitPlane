#!/usr/bin/env bash
# test.sh — prove the XDP fast path on a real kernel. Run as root on Linux.
#
#   lp-cli netns                         lp-srv netns
#   10.77.0.2 / 10.77.0.3 / fd77::2  <── veth ──>  10.77.0.1 / fd77::1
#                                       XDP attached here, on lp-s ingress
#                                       HTTP :8080, LimitPlane gateway :3000
#
# Checks: normal traffic passes; a banned address is dropped (v4 and v6) and
# the counter moves; a ban expires in the kernel with no agent help; a flood
# trips the per-IP bucket; unban restores traffic; the agent copies a real
# gateway autopilot cooldown into the kernel; SIGTERM detaches the program.
set -uo pipefail

cd "$(dirname "$0")"
REPO=$(cd .. && pwd)
BIN=/tmp/limitplane-xdp
GW=/tmp/limitplane-gateway
PIN=/run/lp-bpf/limitplane
FAILS=0
PIDS=()

pass() { echo "PASS  $*"; }
fail() { echo "FAIL  $*"; FAILS=$((FAILS + 1)); }
cli() { ip netns exec lp-cli "$@"; }
srv() { ip netns exec lp-srv "$@"; }
stat() { "$BIN" stats -pin "$PIN" | tr ' ' '\n' | sed -n "s/^$1=//p"; }
# http_ok SRC URL — exit 0 iff an HTTP request from SRC gets an answer.
http_ok() { cli curl -s -o /dev/null --max-time 2 --interface "$1" "$2"; }

cleanup() {
	for p in "${PIDS[@]}"; do kill "$p" 2>/dev/null; done
	wait 2>/dev/null
	ip netns del lp-cli 2>/dev/null
	ip netns del lp-srv 2>/dev/null
	rm -rf "$PIN"
	umount /run/lp-bpf 2>/dev/null
}
trap cleanup EXIT

[ "$(id -u)" = 0 ] || { echo "run as root"; exit 2; }
echo "kernel $(uname -r) ($(uname -m))"

echo "== build"
(cd "$REPO/xdp" && go build -o "$BIN" ./cmd/limitplane-xdp) || exit 2
(cd "$REPO" && go build -o "$GW" ./cmd/gateway) || exit 2

echo "== topology"
cleanup 2>/dev/null; trap cleanup EXIT
# `ip netns exec` remounts /sys, which hides /sys/fs/bpf inside the netns, so
# the maps are pinned on a bpffs mounted under /run instead.
mkdir -p /run/lp-bpf && mount -t bpf bpf /run/lp-bpf
ip netns add lp-srv
ip netns add lp-cli
ip link add lp-s netns lp-srv type veth peer name lp-c netns lp-cli
srv ip addr add 10.77.0.1/24 dev lp-s
srv ip addr add fd77::1/64 dev lp-s nodad
cli ip addr add 10.77.0.2/24 dev lp-c
cli ip addr add 10.77.0.3/24 dev lp-c
cli ip addr add fd77::2/64 dev lp-c nodad
srv ip link set lp-s up; srv ip link set lo up
cli ip link set lp-c up; cli ip link set lo up

srv python3 -m http.server 8080 --bind :: >/dev/null 2>&1 &
PIDS+=($!)
srv env JWT_SECRET=xdp-test-secret PORT=3000 "$GW" >/tmp/lp-gw.log 2>&1 &
PIDS+=($!)
sleep 1.5

start_agent() { # args passed to the agent
	# Not via srv(): a backgrounded function is a subshell, and $! would be its
	# PID, not the agent's. ip and env both exec, so $! IS the agent.
	ip netns exec lp-srv env LIMITPLANE_TOKEN="${LIMITPLANE_TOKEN:-}" \
		"$BIN" -iface lp-s -pin "$PIN" -metrics-addr 127.0.0.1:9091 "$@" >>/tmp/lp-xdp.log 2>&1 &
	AGENT=$!
	PIDS+=("$AGENT")
	for _ in $(seq 50); do [ -e "$PIN/counters" ] && grep -q attached /tmp/lp-xdp.log && break; sleep 0.1; done
}
: >/tmp/lp-xdp.log

echo "== phase 1: ban list and flood bucket (no gateway sync)"
start_agent -gateway "" -rate 100 -burst 200
echo "     $(grep -o 'mode=[a-z]*' /tmp/lp-xdp.log | head -1) on veth"

# (a) normal traffic passes
if http_ok 10.77.0.2 http://10.77.0.1:8080/ && http_ok 10.77.0.3 http://10.77.0.1:8080/ && http_ok fd77::2 "http://[fd77::1]:8080/"; then
	pass "(a) normal v4 + v6 traffic reaches the server"
else
	fail "(a) normal traffic did not get through"
fi
[ "$(stat passed)" -gt 0 ] && pass "(a) passed counter = $(stat passed)" || fail "(a) passed counter did not move"

# (b) a banned v4 address is dropped, its neighbour is not
before=$(stat dropped_banned)
"$BIN" ban 10.77.0.3 -pin "$PIN"
if ! http_ok 10.77.0.3 http://10.77.0.1:8080/ && http_ok 10.77.0.2 http://10.77.0.1:8080/; then
	pass "(b) 10.77.0.3 banned → dropped; 10.77.0.2 unaffected"
else
	fail "(b) ban did not isolate 10.77.0.3"
fi
after=$(stat dropped_banned)
[ "$after" -gt "$before" ] && pass "(b) dropped_banned $before → $after" || fail "(b) dropped_banned did not move"

# (b6) the same for IPv6
"$BIN" ban fd77::2 -pin "$PIN"
if ! http_ok fd77::2 "http://[fd77::1]:8080/"; then pass "(b) fd77::2 banned → dropped (IPv6)"; else fail "(b) IPv6 ban not enforced"; fi
"$BIN" unban fd77::2 -pin "$PIN"
sleep 1 # neighbour discovery re-resolves after the drops
if http_ok fd77::2 "http://[fd77::1]:8080/"; then pass "(d) fd77::2 unbanned → reachable"; else fail "(d) IPv6 unban did not restore traffic"; fi

# (d) unban restores traffic
"$BIN" unban 10.77.0.3 -pin "$PIN"
if http_ok 10.77.0.3 http://10.77.0.1:8080/; then pass "(d) 10.77.0.3 unbanned → reachable"; else fail "(d) unban did not restore traffic"; fi

# (e) a timed ban lapses inside the kernel, no agent involved
"$BIN" ban 10.77.0.3 -for 2s -pin "$PIN"
http_ok 10.77.0.3 http://10.77.0.1:8080/ && fail "(e) timed ban not enforced" || pass "(e) 2s ban enforced"
sleep 2.5
if http_ok 10.77.0.3 http://10.77.0.1:8080/; then pass "(e) ban expired in the kernel by itself"; else fail "(e) timed ban did not expire"; fi

# (c) a flood from one source trips its bucket; other sources keep working
before=$(stat dropped_rate)
cli python3 - <<'EOF'
import socket
s = socket.socket(socket.AF_INET, socket.SOCK_DGRAM)
s.bind(("10.77.0.2", 0))
for _ in range(5000):
    s.sendto(b"x" * 64, ("10.77.0.1", 9))
EOF
after=$(stat dropped_rate)
dropped=$((after - before))
if [ "$dropped" -ge 4000 ]; then pass "(c) 5000-packet flood from 10.77.0.2: $dropped dropped by the bucket (burst 200)"; else fail "(c) flood dropped only $dropped"; fi
if http_ok 10.77.0.3 http://10.77.0.1:8080/; then pass "(c) a different source is not affected by the flood"; else fail "(c) flood spilled onto 10.77.0.3"; fi
sleep 2.5 # bucket refills at 100 pps
if http_ok 10.77.0.2 http://10.77.0.1:8080/; then pass "(c) flooder's bucket refilled → reachable again"; else fail "(c) bucket did not refill"; fi

srv curl -s 127.0.0.1:9091/metrics | grep -q 'limitplane_xdp_packets_total{verdict="dropped_rate"}' \
	&& pass "metrics endpoint exports the counters" || fail "metrics endpoint"

# (g) SIGTERM detaches the program
kill -TERM "$AGENT"; wait "$AGENT" 2>/dev/null
if srv ip link show lp-s | grep -q xdp; then fail "(g) XDP still attached after SIGTERM"; else pass "(g) SIGTERM detached the program"; fi

echo "== phase 2: gateway autopilot cooldown → kernel ban"
# Storm the gateway from 10.77.0.3: free tier holds 10 tokens, and 10
# burst-blocks within 60s trips the autopilot's storm cooldown.
for _ in $(seq 30); do cli curl -s -o /dev/null --max-time 2 --interface 10.77.0.3 http://10.77.0.1:3000/v1/demo/ping; done
TOKEN=$(cli curl -s --max-time 2 -X POST http://10.77.0.1:3000/v1/auth/login -H 'content-type: application/json' \
	-d '{"email":"demo@limitplane.dev","password":"demo123"}' | python3 -c 'import sys,json;print(json.load(sys.stdin)["token"])')
bans=$(cli curl -s --max-time 2 http://10.77.0.1:3000/v1/admin/autopilot -H "authorization: Bearer $TOKEN")
if echo "$bans" | grep -q 'anon:10.77.0.3'; then pass "gateway autopilot put anon:10.77.0.3 in cooldown"; else fail "gateway did not ban 10.77.0.3: $bans"; fi

LIMITPLANE_TOKEN=$TOKEN start_agent -gateway http://10.77.0.1:3000 -sync 500ms -rate 0
sleep 1.5
if grep -q 'kernel ban.*10.77.0.3' /tmp/lp-xdp.log && ! http_ok 10.77.0.3 http://10.77.0.1:8080/; then
	pass "agent copied the cooldown into the kernel: 10.77.0.3 now dropped at the NIC"
else
	fail "agent did not enforce the gateway cooldown"
fi
# The ban must not leak onto anyone else — including the agent's own path to
# the gateway, which comes from inside lp-srv.
if http_ok 10.77.0.2 http://10.77.0.1:8080/; then pass "10.77.0.2 still reachable during the kernel ban"; else fail "kernel ban leaked onto 10.77.0.2"; fi

kill -TERM "$AGENT"; wait "$AGENT" 2>/dev/null

echo
if [ "$FAILS" -eq 0 ]; then echo "ALL CHECKS PASSED"; else echo "$FAILS CHECK(S) FAILED"; fi
exit "$FAILS"
