#!/usr/bin/env bash
# Two throwaway VMs for the live XDP test: "site" runs a demo shop behind
# LimitPlane + the XDP agent, "attacker" floods it. Delete with teardown.sh.
set -euo pipefail
RG=${RG:-limitplane-xdp-live}
LOC=${LOC:-eastasia}  # Azure for Students only allows a few regions
SIZE=${SIZE:-Standard_B2s}
KEY=${KEY:-$HOME/.ssh/limitplane_azure}
SITE_VM=${SITE_VM:-lp-site}
ATK_VM=${ATK_VM:-lp-attacker}

[ -f "$KEY" ] || ssh-keygen -t ed25519 -N "" -f "$KEY" -C limitplane-xdp-test
# CI's identity may only touch an existing group, so it skips the create.
[ -n "${SKIP_GROUP:-}" ] || az group create -n "$RG" -l "$LOC" -o none
pids=()
for vm in "$SITE_VM" "$ATK_VM"; do
  az vm create -g "$RG" -n "$vm" -l "$LOC" --image Ubuntu2404 --size "$SIZE" \
    --admin-username azureuser --ssh-key-values "$KEY.pub" \
    --public-ip-sku Standard --nsg-rule SSH -o none &
  pids+=($!)
done
for p in "${pids[@]}"; do wait "$p"; done # a failed create fails the script
az vm open-port -g "$RG" -n "$SITE_VM" --port 80 --priority 900 -o none
# UDP/9 for the packet-flood step. Without it Azure's NSG drops the flood
# before it reaches the VM, and XDP never sees it.
az network nsg rule create -g "$RG" --nsg-name "${SITE_VM}NSG" -n allow-udp-9 --priority 910 \
  --protocol Udp --destination-port-ranges 9 --access Allow --direction Inbound -o none
echo "SITE=$(az vm show -d -g "$RG" -n "$SITE_VM" --query publicIps -o tsv)"
echo "ATTACKER=$(az vm show -d -g "$RG" -n "$ATK_VM" --query publicIps -o tsv)"
