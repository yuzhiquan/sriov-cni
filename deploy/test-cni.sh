#!/usr/bin/env bash
# 不依赖 k8s 集群，直接手动调用 sriov-cni 做一次完整的 ADD / 验证 / DEL 冒烟测试。
#
# 前提：
#   1. 节点上已经创建好 SR-IOV VF（见 setup-vfs.sh）
#   2. VF 绑定了内核驱动（iavf/ixgbevf/mlx5_core），即有 netdev
#   3. 有 static 或 host-local IPAM 插件（可选，没有则跳过 IPAM）
#
# 用法：
#   sudo ./test-cni.sh                      # 自动挑第一个可用 VF
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
        *) error "未知参数: $1"; exit 1 ;;
    esac
done

cleanup() {
    ip netns del "$NSNAME" 2>/dev/null || true
    rm -f /etc/netns/"$NSNAME" 2>/dev/null || true
}
trap cleanup EXIT

# ---------- 0. 环境检查 ----------
step "检查插件二进制"
[[ -x "$PLUGIN" ]] || { error "找不到可执行插件: $PLUGIN（先执行 ./build.sh）"; exit 1; }
info "插件: $PLUGIN"

"$PLUGIN" <<< '{}' 2>/dev/null >/dev/null || true
info "VERSION=$(CNI_COMMAND=VERSION "$PLUGIN")"

# ---------- 1. 找一个空闲 VF ----------
step "查找可用的 SR-IOV VF"
if [[ -z "$DEVICE" ]]; then
    for pfdev in /sys/class/net/*/device; do
        pf=$(basename "$(dirname "$(dirname "$pfdev")")")
        [[ -d "$pfdev" ]] || continue
        [[ -e "$pfdev/physfn" ]] && continue   # 跳过 VF 自己
        for vf in "$pfdev"/virtfn*; do
            [[ -e "$vf" ]] || continue
            bdf=$(basename "$(readlink -f "$vf")")
            # 有 netdev 说明绑了内核驱动
            if [[ -d "/sys/bus/pci/devices/$bdf/net" ]]; then
                DEVICE="$bdf"
                info "选中 VF: $DEVICE (PF=$pf)"
                break 2
            fi
        done
    done
fi

if [[ -z "$DEVICE" ]]; then
    error "未找到可用的 SR-IOV VF"
    cat <<'EOF'

        可能原因：
          1. BIOS 未开启 SR-IOV / Above 4G Decoding
          2. 尚未创建 VF：
               echo 8 > /sys/class/net/<PF>/device/sriov_numvfs
          3. VF 被绑到了 vfio-pci（DPDK 模式），没有 netdev：
               driverctl list-devices
          4. 这台机器/云主机压根没有 SR-IOV 网卡

        用 './setup-vfs.sh --check' 做一次完整检查。
EOF
    exit 1
fi

# ---------- 2. 准备 netns 与 IPAM ----------
step "准备环境"
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
    warn "未找到 IPAM 插件 /opt/cni/bin/$IPAM_TYPE，本次跳过 IPAM 验证"
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
step "执行 CNI ADD"
RESULT=$(echo "$CONF" | CNI_COMMAND=ADD \
    CNI_CONTAINERID=smoke-test-$$ \
    CNI_NETNS="$NETNS" \
    CNI_IFNAME="$IFNAME" \
    CNI_ARGS="K8S_POD_NAME=smoke;K8S_POD_NAMESPACE=default;IgnoreUnknown=true" \
    CNI_PATH=/opt/cni/bin \
    "$PLUGIN")
echo "$RESULT" | jq . 2>/dev/null || echo "$RESULT"

step "验证 Pod netns 内的网卡"
echo "--- ip -br link ---"
ip netns exec "$NSNAME" ip -br link
echo "--- ip -br addr ---"
ip netns exec "$NSNAME" ip -br addr
echo "--- bus-info ---"
ip netns exec "$NSNAME" sh -c "readlink -f /sys/class/net/$IFNAME/device"

step "验证宿主侧 VF 属性已被改写"
ip link show "$(ls -d /sys/class/net/*/device | head -1 | xargs dirname | xargs dirname | xargs basename)" 2>/dev/null \
  | sed -n 's/^    \(vf .*\)/  \1/p' || true

step "执行 CNI DEL"
CONTAINER_ID="smoke-test-$$"
echo "$CONF" | CNI_COMMAND=DEL \
    CNI_CONTAINERID="$CONTAINER_ID" \
    CNI_NETNS="$NETNS" \
    CNI_IFNAME="$IFNAME" \
    CNI_PATH=/opt/cni/bin \
    "$PLUGIN" && info "DEL 成功"

step "确认 VF 已回到宿主"
ip -br link | grep -E "v[0-9]+" || true

info "冒烟测试完成"
