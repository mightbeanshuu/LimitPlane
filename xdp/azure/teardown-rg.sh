#!/usr/bin/env bash
# CI teardown: empty the resource group without deleting it (the CI identity
# has Contributor on this one group and nothing else). VMs go first so their
# NICs and disks free up, then everything left, retried until the group is
# empty, because Azure refuses to delete a network that a NIC still uses.
set -uo pipefail
RG=${RG:?set RG}
ids() { az resource list -g "$RG" --query "$1" -o tsv; }
vms=$(ids "[?type=='Microsoft.Compute/virtualMachines'].id")
[ -n "$vms" ] && az vm delete --ids $vms --yes -o none
for attempt in 1 2 3 4 5 6; do
  left=$(ids "[].id")
  [ -z "$left" ] && { echo "resource group $RG is empty"; exit 0; }
  echo "pass $attempt: $(echo "$left" | wc -l | tr -d ' ') resources left"
  for id in $left; do az resource delete --ids "$id" -o none 2>/dev/null; done
  sleep 10
done
echo "still left in $RG:"; ids "[].name"; exit 1
