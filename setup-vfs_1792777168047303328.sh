#!/usr/bin/env bash
# 节点侧一次性准备：检查硬件/BIOS/IOMMU，创建并持久化 SR-IOV VF。
#
# 用法：
#   sudo ./setup-vfs.sh --pf ens1f0 --numvfs 8          # 创建 VF + 持久化
#   sudo ./setup-vfs.sh --pf ens1f0 --numvfs 8 --persist  # 同上（默认）
#   ./setup-vfs.sh --check                               # 只做检查，不改动
#
set -euo pipefail

PF=""
NUMVFS=0
PERSIST=1
CHECK_ONLY=0

RED=$'\033[0;31m'; GRN=$'\033[0;32m'; YEL=$'\033[0;33m'; NC=$'\033[0m'
info()  { printf '%s[INFO]%s  %s\n'  "$GRN" "$NC" "$*"; }
warn()  { printf '%s[WARN]%s  %s\n'  "$YEL" "$NC" "$*"; }
error() { printf '%s[ERROR]%s %s\n' "$RED" "$NC" "$*"; }

usage() {
    cat <<'EOF'
用法: setup-vfs.sh [选项]
  --pf <netdev>       PF 的 netdev 名，如 ens1f0（默认自动探测第一个支持 SR-IOV 的网卡）
  --numvfs <N>        要创建的 VF 数量（默认 8）
  --no-persist        不写 systemd unit（仅本次生效）
  --check             只检查不修改
  -h, --help          显示帮助
EOF
}

while [[ $# -gt 0 ]]; do
    case "$1" in
        --pf)        PF="${2:-}"; shift 2 ;;
        --numvfs)    NUMVFS="${2:-8}"; shift 2 ;;
        --no-persist) PERSIST=0; shift ;;
        --check)     CHECK_ONLY=1; shift ;;
        -h|--help)   usage; exit 0 ;;
        *) error "未知参数: $1"; usage; exit 1 ;;
    esac
done

need_root() {
    if [[ $EUID -ne 0 ]]; then
        error "请以 root 运行（需要写 /sys 和 /etc/systemd）"
        exit 1
    fi
}

# ---------- 自动探测 PF ----------
detect_pf() {
    local dev
    for dev in /sys/class/net/*; do
        local name; name=$(basename "$dev")
        [[ -e "$dev/device/sriov_totalvfs" ]] || continue
        # 排除 VF 自己（VF 有 physfn 软链接）
        [[ -e "$dev/device/physfn" ]] && continue
        echo "$name"
        return 0
    done
    return 1
}

# ---------- 检查 ----------
do_check() {
    echo "================ 1. 网卡与 SR-IOV 能力 ================"
    local found=0
    for dev in /sys/class/net/*; do
        local name; name=$(basename "$dev")
        [[ -e "$dev/device/sriov_totalvfs" ]] || continue
        [[ -e "$dev/device/physfn" ]] && continue
        found=1
        local bdf total cur
        bdf=$(basename "$(readlink -f "$dev/device")")
        total=$(cat "$dev/device/sriov_totalvfs")
        cur=$(cat "$dev/device/sriov_numvfs" 2>/dev/null || echo 0)
        echo "  PF: $name  $bdf   totalvfs=$total  numvfs=$cur"
    done
    [[ $found -eq 1 ]] || { error "未发现支持 SR-IOV 的网卡（检查 BIOS 与网卡型号）"; exit 1; }

    echo
    echo "================ 2. IOMMU 状态 ================"
    if dmesg 2>/dev/null | grep -qi "DMAR: Intel-IOMMU enabled\|AMD-Vi:.*initialized"; then
        info "IOMMU 已启用"
    else
        warn "IOMMU 未启用 —— netdevice 模式可以正常工作；DPDK/vfio 模式需要它"
        echo "        在 /etc/default/grub 里加 intel_iommu=on iommu=pt（AMD: amd_iommu=on），然后 update-grub && reboot"
    fi

    echo
    echo "================ 3. VF netdev 是否被网络管理工具接管 ================"
    if command -v nmcli >/dev/null 2>&1; then
        local managed
        managed=$(nmcli -t -f DEVICE,STATE dev status 2>/dev/null | grep -E "v[0-9]+:" | grep -v "unmanaged" || true)
        if [[ -n "$managed" ]]; then
            warn "以下 VF 被 NetworkManager 管理，会导致 CNI 搬不动："
            echo "$managed" | sed 's/^/        /'
            echo "        建议：echo '[keyfile]' > /etc/NetworkManager/conf.d/10-sriov-vf-unmanaged.conf"
            echo "        再追加 unmanaged-devices=interface-name:ens1f0v* 后 systemctl reload NetworkManager"
        else
            info "VF 均未被 NetworkManager 接管"
        fi
    else
        info "未安装 NetworkManager，跳过检查"
    fi

    echo
    echo "================ 4. 当前 VF 状态 ================"
    if [[ -n "$PF" ]]; then
        ip link show "$PF" | sed -n 's/^    \(vf .*\)/  \1/p'
    fi
}

# ---------- 创建 VF ----------
do_create() {
    need_root
    [[ -n "$PF" ]] || PF=$(detect_pf) || { error "无法自动探测 PF，请用 --pf 指定"; exit 1; }

    local sysdev="/sys/class/net/$PF/device"
    [[ -d "$sysdev" ]] || { error "$PF 没有对应的 PCI 设备"; exit 1; }
    [[ -f "$sysdev/sriov_totalvfs" ]] || { error "$PF 不支持 SR-IOV"; exit 1; }

    local total; total=$(cat "$sysdev/sriov_totalvfs")
    info "PF=$PF  硬件支持 VF 上限=$total  本次创建=$NUMVFS"

    if [[ "$NUMVFS" -gt "$total" ]]; then
        error "请求 $NUMVFS 个 VF，超过硬件上限 $total"
        exit 1
    fi

    # 改 numvfs 前必须先归零，否则驱动会报错
    echo 0 > "$sysdev/sriov_numvfs"
    echo "$NUMVFS" > "$sysdev/sriov_numvfs"

    info "当前 VF："
    ip link show "$PF" | sed -n 's/^    \(vf .*\)/  \1/p'

    if [[ "$PERSIST" -eq 1 ]]; then
        local unit="/etc/systemd/system/sriov-vfs-${PF}.service"
        cat > "$unit" <<EOF
[Unit]
Description=Create SR-IOV VFs for ${PF}
After=network-pre.target
Before=network.target

[Service]
Type=oneshot
ExecStart=/bin/sh -c 'echo 0 > ${sysdev}/sriov_numvfs; echo ${NUMVFS} > ${sysdev}/sriov_numvfs'
RemainAfterExit=yes

[Install]
WantedBy=multi-user.target
EOF
        systemctl daemon-reload
        systemctl enable --now "sriov-vfs-${PF}.service"
        info "已持久化：${unit}"
    fi
}

main() {
    if [[ "$CHECK_ONLY" -eq 1 ]]; then
        do_check
        return
    fi
    do_create
}

main "$@"
