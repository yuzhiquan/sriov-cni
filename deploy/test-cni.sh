#!/usr/bin/env bash
# Without a k8s cluster, invoke sriov-cni by hand for a full ADD / verify / DEL smoke test.
#
# Prerequisites:
#   1. SR-IOV VFs already created on the node (see setup-vfs.sh)
#   2. VF bound to a kernel driver (iavf/ixgbevf/mlx5_core), i.e. has a netdev
#   3. A static or host-local IPAM plugin is available (optional; IPAM is skipped if absent)
#
# Usage:
#   sudo ./test-cni.sh                      # auto-pick the first usable VF
#   sudo ./test-cni.sh --device 0000:03:02.1
#   sudo ./test-cni.sh --plugin ./build/sriov --device 0000:03:02.1 --vlan 1000
#
set -euo pipefail

PLUGIN="${PLUGIN:-$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)/build/sriov}"
DEVICE=""
VLAN=0
NSNAME="sriov-cni-test"
IFNAME="net1"
IPAM_TYPE="static"
IP="192.168.100.99/24"

RED=$'\033[0;31m'; GRN=$'\033[0;32m'; YEL=$'\033[0;33m'; BLU=$'\033[0;34m'; NC=$'\033[0m'
info()  { printf '%s[INFO]%s  %s\n'  "$GRN" "$NC" "$*"; }
warn()  { printf '%s[WARN]%s  %s\n'  "$YEL" "$NC" "$*"; }
error() { printf '%s[ERROR]%s %s\n' "$RED" "$NC" "$*"; }
step()  { printf '\n%s===> %s%s\n' "$BLU" "$*" "$NC"; }

while [[ $# -gt 0 ]]; do
    case "$1" in
        --plugin)  PLUGIN="$2"; shift 2 ;;
        --device)  DEVICE="$2"; shift 2 ;;
        --vlan)    VLAN="$2"; shift 2 ;;
        -h|--help) sed -n '2,12p' "$0"; exit 0 ;;
        *) error "Unknown argument: $1"; exit 1 ;;
    esac
done

cleanup() {
    ip netns del "$NSNAME" 2>/dev/null || true
    rm -f /etc/netns/"$NSNAME" 2>/dev/null || true
}
trap cleanup EXIT

# ---------- 0. Environment check ----------
step "Check plugin binary"
[[ -x "$PLUGIN" ]] || { error "Executable plugin not found: $PLUGIN (run ./build.sh first)"; exit 1; }
info "Plugin: $PLUGIN"

"$PLUGIN" <<< '{}' 2>/dev/null >/dev/null || true
info "VERSION=$(CNI_COMMAND=VERSION "$PLUGIN")"

# ---------- 1. Find a free VF ----------
step "Find an available SR-IOV VF"
if [[ -z "$DEVICE" ]]; then
    for pfdev in /sys/class/net/*/device; do
        pf=$(basename "$(dirname "$(dirname "$pfdev")")")
        [[ -d "$pfdev" ]] || continue
        [[ -e "$pfdev/physfn" ]] && continue   # skip the VF itself
        for vf in "$pfdev"/virtfn*; do
            [[ -e "$vf" ]] || continue
            bdf=$(basename "$(readlink -f "$vf")")
            # has a netdev, so a kernel driver is bound
            if [[ -d "/sys/bus/pci/devices/$bdf/net" ]]; then
                DEVICE="$bdf"
                info "Selected VF: $DEVICE (PF=$pf)"
                break 2
            fi
        done
    done
fi

if [[ -z "$DEVICE" ]]; then
    error "No available SR-IOV VF found"
    cat <<'EOF'

        Possible causes:
          1. SR-IOV / Above 4G Decoding not enabled in BIOS
          2. VFs not created yet:
               echo 8 > /sys/class/net/<PF>/device/sriov_numvfs
          3. VF is bound to vfio-pci (DPDK mode), so there is no netdev:
               driverctl list-devices
          4. This machine/VM has no SR-IOV NIC at all

        Run './setup-vfs.sh --check' for a full check.
EOF
    exit 1
fi

# ---------- 2. Prepare netns and IPAM ----------
step "Prepare environment"
ip netns add "$NSNAME"
NETNS="/var/run/netns/$NSNAME"
info "netns: $NETNS"

IPAM_BLOCK=""
if [[ -f "/opt/cni/bin/$IPAM_TYPE" ]]; then
    IPAM_BLOCK=$(cat <<EOF
,
      "ipam": {
        "type": "$IPAM_TYPE",
        "addresses": [ { "address": "$IP" } ],
        "routes": [ { "dst": "10.99.0.0/16" } ]
      }
EOF
)
else
    warn "IPAM plugin /opt/cni/bin/$IPAM_TYPE not found, skipping IPAM verification"
fi

CONF=$(cat <<EOF
{
  "cniVersion": "1.0.0",
  "name": "sriov-test",
  "type": "sriov",
  "deviceID": "$DEVICE",
  "vlan": $VLAN,
  "spoofchk": "off",
  "trust": "on",
  "logLevel": "debug"
  $IPAM_BLOCK
}
EOF
)
echo "$CONF" | jq . 2>/dev/null || echo "$CONF"

# ---------- 3. ADD ----------
step "Run CNI ADD"
RESULT=$(echo "$CONF" | CNI_COMMAND=ADD \
    CNI_CONTAINERID=smoke-test-$$ \
    CNI_NETNS="$NETNS" \
    CNI_IFNAME="$IFNAME" \
    CNI_ARGS="K8S_POD_NAME=smoke;K8S_POD_NAMESPACE=default;IgnoreUnknown=true" \
    CNI_PATH=/opt/cni/bin \
    "$PLUGIN")
echo "$RESULT" | jq . 2>/dev/null || echo "$RESULT"

step "Verify NIC inside Pod netns"
echo "--- ip -br link ---"
ip netns exec "$NSNAME" ip -br link
echo "--- ip -br addr ---"
ip netns exec "$NSNAME" ip -br addr
echo "--- bus-info ---"
ip netns exec "$NSNAME" sh -c "readlink -f /sys/class/net/$IFNAME/device"

step "Verify host-side VF attributes were rewritten"
ip link show "$(ls -d /sys/class/net/*/device | head -1 | xargs dirname | xargs dirname | xargs basename)" 2>/dev/null \
  | sed -n 's/^    \(vf .*\)/  \1/p' || true

step "Run CNI DEL"
CONTAINER_ID="smoke-test-$$"
echo "$CONF" | CNI_COMMAND=DEL \
    CNI_CONTAINERID="$CONTAINER_ID" \
    CNI_NETNS="$NETNS" \
    CNI_IFNAME="$IFNAME" \
    CNI_PATH=/opt/cni/bin \
    "$PLUGIN" && info "DEL succeeded"

step "Confirm VF returned to host"
ip -br link | grep -E "v[0-9]+" || true

info "Smoke test complete"
