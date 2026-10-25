#!/usr/bin/env bash
# One-time node setup: check hardware/BIOS/IOMMU, create and persist SR-IOV VFs.
#
# Usage:
#   sudo ./setup-vfs.sh --pf ens1f0 --numvfs 8          # create VFs + persist
#   sudo ./setup-vfs.sh --pf ens1f0 --numvfs 8 --persist  # same as above (default)
#   ./setup-vfs.sh --check                               # check only, no changes
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
Usage: setup-vfs.sh [options]
  --pf <netdev>       PF netdev name, e.g. ens1f0 (default: auto-detect first SR-IOV capable NIC)
  --numvfs <N>        Number of VFs to create (default 8)
  --no-persist        Do not write a systemd unit (this boot only)
  --check             Check only, make no changes
  -h, --help          Show help
EOF
}

while [[ $# -gt 0 ]]; do
    case "$1" in
        --pf)        PF="${2:-}"; shift 2 ;;
        --numvfs)    NUMVFS="${2:-8}"; shift 2 ;;
        --no-persist) PERSIST=0; shift ;;
        --check)     CHECK_ONLY=1; shift ;;
        -h|--help)   usage; exit 0 ;;
        *) error "Unknown argument: $1"; usage; exit 1 ;;
    esac
done

need_root() {
    if [[ $EUID -ne 0 ]]; then
        error "Run as root (needs to write /sys and /etc/systemd)"
        exit 1
    fi
}

# ---------- Auto-detect PF ----------
detect_pf() {
    local dev
    for dev in /sys/class/net/*; do
        local name; name=$(basename "$dev")
        [[ -e "$dev/device/sriov_totalvfs" ]] || continue
        # exclude VFs themselves (a VF has a physfn symlink)
        [[ -e "$dev/device/physfn" ]] && continue
        echo "$name"
        return 0
    done
    return 1
}

# ---------- Check ----------
do_check() {
    echo "================ 1. NICs and SR-IOV capability ================"
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
    [[ $found -eq 1 ]] || { error "No SR-IOV capable NIC found (check BIOS and NIC model)"; exit 1; }

    echo
    echo "================ 2. IOMMU status ================"
    if dmesg 2>/dev/null | grep -qi "DMAR: Intel-IOMMU enabled\|AMD-Vi:.*initialized"; then
        info "IOMMU enabled"
    else
        warn "IOMMU not enabled -- netdevice mode works without it; DPDK/vfio mode requires it"
        echo "        Add intel_iommu=on iommu=pt (AMD: amd_iommu=on) to /etc/default/grub, then run update-grub && reboot"
    fi

    echo
    echo "================ 3. Whether VF netdevs are claimed by a network manager ================"
    if command -v nmcli >/dev/null 2>&1; then
        local managed
        managed=$(nmcli -t -f DEVICE,STATE dev status 2>/dev/null | grep -E "v[0-9]+:" | grep -v "unmanaged" || true)
        if [[ -n "$managed" ]]; then
            warn "These VFs are managed by NetworkManager, which prevents the CNI from moving them:"
            echo "$managed" | sed 's/^/        /'
            echo "        Suggested: echo '[keyfile]' > /etc/NetworkManager/conf.d/10-sriov-vf-unmanaged.conf"
            echo "        Then append unmanaged-devices=interface-name:ens1f0v* and run systemctl reload NetworkManager"
        else
            info "No VFs are managed by NetworkManager"
        fi
    else
        info "NetworkManager not installed, skipping check"
    fi

    echo
    echo "================ 4. Current VF status ================"
    if [[ -n "$PF" ]]; then
        ip link show "$PF" | sed -n 's/^    \(vf .*\)/  \1/p'
    fi
}

# ---------- Create VFs ----------
do_create() {
    need_root
    [[ -n "$PF" ]] || PF=$(detect_pf) || { error "Cannot auto-detect PF, specify it with --pf"; exit 1; }

    local sysdev="/sys/class/net/$PF/device"
    [[ -d "$sysdev" ]] || { error "$PF has no corresponding PCI device"; exit 1; }
    [[ -f "$sysdev/sriov_totalvfs" ]] || { error "$PF does not support SR-IOV"; exit 1; }

    local total; total=$(cat "$sysdev/sriov_totalvfs")
    info "PF=$PF  hardware max VFs=$total  to create=$NUMVFS"

    if [[ "$NUMVFS" -gt "$total" ]]; then
        error "Requested $NUMVFS VFs, exceeds hardware limit $total"
        exit 1
    fi

    # must reset to 0 before changing numvfs, otherwise the driver errors
    echo 0 > "$sysdev/sriov_numvfs"
    echo "$NUMVFS" > "$sysdev/sriov_numvfs"

    info "Current VFs:"
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
        info "Persisted: ${unit}"
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
