# Live test on Azure: a real site, a real attacker

`xdp/test.sh` proves the fast path inside network namespaces on one machine.
This directory is the next step: two Azure VMs on the public internet, a demo
shop behind the reverse proxy, and a second VM attacking it. Everything that
ran is in [`session.log`](session.log), one command at a time with its output
(the laptop's address and the admin key are redacted).

```
laptop (normal visitor) ─┐
                         ├──▶ lp-site  104.208.118.64   Ubuntu 24.04, kernel 7.0.0-1017-azure, hv_netvsc
attacker VM ─────────────┘      eth0: limitplane-xdp (XDP)
  20.239.65.212                 :80   limitplane --config lp.json   (reverse proxy, 20-token jar, 1/s)
                                :8080 python http.server            ("XYZ Shop", the tenant site)
```

Run on 22 Oct 2026, East Asia, `Standard_B2ats_v2`. Both VMs were deleted afterwards.

## Results

| Step | What happened | Evidence |
|---|---|---|
| Normal visitor | 200 from the shop, with `X-Ratelimit-Limit: 20` | `## 2` |
| Attacker floods HTTP, forging `X-Forwarded-For` with the laptop's address | 20 × 200, 29 × 429, then `000` (no answer) for the rest | `## 3` |
| Who got banned | `anon:20.239.65.212`, the attacker's socket address. The forged header was ignored | `activeBans` in `## 3` |
| Ban reaches the kernel | `kernel ban ip=20.239.65.212 for=5m0s` 4 s after the flood began | `xdp.log`, `## 4` |
| Attacker while banned | HTTP `000`, ping 100% loss, `dropped_banned=98` | `## 4` |
| Laptop while attacker banned | 200, 200, 200 | `## 5` |
| Ban ends | `kernel unban` at +5 min, attacker gets 200 again | `## 6`, `## 8` |
| UDP flood, 20,000 packets in 0.14 s | `dropped_rate=15719`, so 4,281 passed: the 4,000 burst plus 0.14 s × 2,000/s of refill | `## 7d` |
| After the flood | attacker and laptop both 200 | `## 7e` |

## What the test found (and what was fixed before it ran)

Writing the plan for a real deployment exposed two gaps that the namespace
test could not, because it ran against the demo gateway:

1. **The proxy could not feed the agent.** The agent read bans from the demo
   gateway's `/v1/admin/autopilot` (JWT login). A real site runs the reverse
   proxy, which had no such endpoint. Fixed: the proxy serves
   `/_limitplane/autopilot` behind its `adminKey`, and the agent uses it when
   `LIMITPLANE_ADMIN_KEY` is set.
2. **A forged header could get an innocent visitor dropped at the NIC.** The
   proxy took the visitor's identity from `X-Forwarded-For`, which on an
   internet-facing host comes from the client. An attacker could name a
   customer's address, run up blocks, and the agent would ban that customer in
   the kernel. Fixed: the proxy now ignores the header unless started with
   `--trust-proxy`. Step 3 above is the check: the attacker forged the
   laptop's address and still banned only itself.

And two things the run itself taught:

- **Generic mode on Azure.** The agent logged `mode=generic`: native XDP did
  not attach on this VM's `hv_netvsc` interface, so the program runs after the
  kernel builds the packet buffer. Drops still never reach TCP, the proxy or
  the shop, but each one costs more than in native mode.
- **The cloud firewall sits in front of XDP.** The first UDP flood was
  invisible to the counters because Azure's network security group dropped it
  before it reached the VM. On a cloud VM, XDP only sees what the provider's
  firewall lets through. The NSG rule for UDP/9 is now in `provision.sh`.
  The failed attempt and a quoting mistake in the retry are left in the log as
  they happened.

## Reproduce

```bash
cd xdp/azure
mkdir -p bin
GOOS=linux GOARCH=amd64 CGO_ENABLED=0 go build -o bin/limitplane ../../cmd/limitplane
(cd .. && GOOS=linux GOARCH=amd64 CGO_ENABLED=0 go build -o azure/bin/limitplane-xdp ./cmd/limitplane-xdp)
./provision.sh                                  # prints both public IPs
SITE=<site ip> ATTACKER=<attacker ip> ./run-test.sh
./teardown.sh
```

Only flood machines you own. Azure allows load against your own resources,
but large-scale DDoS simulation needs an approved testing partner, so the
floods here are a few thousand packets.
