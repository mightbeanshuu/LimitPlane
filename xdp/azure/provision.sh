#!/usr/bin/env bash
# Two throwaway VMs for the live XDP test: "site" runs a demo shop behind
# LimitPlane + the XDP agent, "attacker" floods it. Delete with teardown.sh.
set -euo pipefail
RG=${RG:-limitplane-xdp-live}
LOC=${LOC:-eastasia}  # Azure for Students only allows a few regions
SIZE=${SIZE:-Standard_B2s}
KEY=${KEY:-$HOME/.ssh/limitplane_azure}

[ -f "$KEY" ] || ssh-keygen -t ed25519 -N "" -f "$KEY" -C limitplane-xdp-test
az group create -n "$RG" -l "$LOC" -o none
for vm in site attacker; do
  az vm create -g "$RG" -n "lp-$vm" --image Ubuntu2404 --size "$SIZE" \
    --admin-username azureuser --ssh-key-values "$KEY.pub" \
    --public-ip-sku Standard --nsg-rule SSH -o none
done
az vm open-port -g "$RG" -n lp-site --port 80 --priority 900 -o none
# UDP/9 for the packet-flood step. Without it Azure's NSG drops the flood
# before it reaches the VM, and XDP never sees it.
az network nsg rule create -g "$RG" --nsg-name lp-siteNSG -n allow-udp-9 --priority 910 \
  --protocol Udp --destination-port-ranges 9 --access Allow --direction Inbound -o none
az vm list-ip-addresses -g "$RG" -o table
