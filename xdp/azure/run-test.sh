#!/usr/bin/env bash
# The recorded live test. Runs from a laptop, drives both VMs over SSH, and
# writes every command and its output to session.log.
#
#   SITE=<site public IP> ATTACKER=<attacker public IP> ./run-test.sh
#
# Expects bin/limitplane and bin/limitplane-xdp (linux/amd64) next to it.
set -uo pipefail
cd "$(dirname "$0")"
KEY=${KEY:-$HOME/.ssh/limitplane_azure}
LOG=session.log
SSH="ssh -i $KEY -o StrictHostKeyChecking=accept-new -o ConnectTimeout=10 -o LogLevel=ERROR"
ADMIN=$(openssl rand -hex 12)
: > "$LOG"

say()  { printf '\n## %s\n' "$*" | tee -a "$LOG"; }
run()  { printf '\n[%s] laptop$ %s\n' "$(date -u +%H:%M:%S)" "$*" | tee -a "$LOG"; eval "$@" 2>&1 | tee -a "$LOG"; }
site() { printf '\n[%s] site$ %s\n' "$(date -u +%H:%M:%S)" "$*" | tee -a "$LOG"; $SSH azureuser@"$SITE" "$@" 2>&1 | tee -a "$LOG"; }
atk()  { printf '\n[%s] attacker$ %s\n' "$(date -u +%H:%M:%S)" "$*" | tee -a "$LOG"; $SSH azureuser@"$ATTACKER" "$@" 2>&1 | tee -a "$LOG"; }

say "0. Who is who"
run 'echo "site=$SITE attacker=$ATTACKER laptop=$(curl -s https://api.ipify.org)"'
site 'uname -sr; ip -br link show eth0; ethtool -i eth0 | head -2'

say "1. Install: two static binaries and a demo shop (the tenant site)"
run "scp -i $KEY -o LogLevel=ERROR bin/limitplane bin/limitplane-xdp azureuser@$SITE:~/"
site "mkdir -p shop && printf '<h1>XYZ Shop</h1><p>Real tenant site, port 8080.</p>\n' > shop/index.html"
site "cat > lp.json <<J
{\"upstream\":\"http://127.0.0.1:8080\",\"port\":80,\"adminKey\":\"$ADMIN\",
 \"policy\":{\"tiers\":{\"free\":{\"capacity\":20,\"refillPerSecond\":1}},\"routes\":{\"*\":{\"costClass\":\"light\"}}}}
J"
site 'nohup python3 -m http.server 8080 --bind 127.0.0.1 -d shop >shop.log 2>&1 & sleep 1; curl -s -o /dev/null -w "shop direct: %{http_code}\n" http://127.0.0.1:8080/'
site 'sudo nohup ./limitplane --config lp.json >proxy.log 2>&1 & sleep 1; tail -3 proxy.log'
site "sudo LIMITPLANE_ADMIN_KEY=$ADMIN nohup ./limitplane-xdp -iface eth0 -gateway http://127.0.0.1:80 -rate 2000 -burst 4000 >xdp.log 2>&1 & sleep 2; cat xdp.log"
site 'sudo ./limitplane-xdp stats'

say "2. A normal visitor (the laptop) gets the shop through LimitPlane"
run "curl -s -m 5 -D - http://$SITE/ | grep -E 'HTTP/|X-Ratelimit|XYZ'"

say "3. Attacker floods HTTP with a FORGED X-Forwarded-For naming the laptop"
run 'LAPTOP=$(curl -s https://api.ipify.org); echo "forging X-Forwarded-For: $LAPTOP"'
atk "for i in \$(seq 1 80); do curl -s -m 3 -o /dev/null -w '%{http_code} ' -H 'X-Forwarded-For: $(curl -s https://api.ipify.org)' http://$SITE/; done; echo"
site "curl -s -H 'X-LimitPlane-Admin: $ADMIN' http://127.0.0.1/_limitplane/autopilot | python3 -m json.tool | grep -A3 activeBans"

say "4. Within a second the ban is in the kernel; attacker packets now die at the NIC"
run 'sleep 2'
site 'sudo ./limitplane-xdp stats; grep -i ban xdp.log | tail -3'
atk "curl -s -m 5 -o /dev/null -w 'attacker HTTP: %{http_code} (000 = no answer at all)\n' http://$SITE/; ping -c 3 -W 1 $SITE | tail -2"
site 'sudo ./limitplane-xdp stats'

say "5. Meanwhile the laptop (whose address was forged) is still served"
run "for i in 1 2 3; do curl -s -m 5 -o /dev/null -w 'laptop HTTP: %{http_code}\n' http://$SITE/; done"

say "6. The ban expires in the kernel after the 5 minute cooldown"
run 'sleep 305'
site 'sudo ./limitplane-xdp stats'
atk "curl -s -m 5 -o /dev/null -w 'attacker HTTP after expiry: %{http_code}\n' http://$SITE/"

say "7. Per-IP packet flood (UDP, no HTTP at all): the kernel token bucket"
site 'sudo ./limitplane-xdp stats'
run "scp -i $KEY -o LogLevel=ERROR udpflood.py azureuser@$ATTACKER:~/"
atk "python3 udpflood.py $SITE"
run 'sleep 1'
site 'sudo ./limitplane-xdp stats; curl -s localhost:9091/metrics | grep -v ^#'

say "8. Logs"
site 'tail -5 proxy.log; tail -8 xdp.log'
say "done"
