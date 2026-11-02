#!/bin/sh
# Read-only inventory collector for the authorized router. Does not copy binaries/configs.
set -eu
umask 077
TARGET=${1:-root@192.0.2.1}
OUT=${2:-inventory/device-$(date -u +%Y%m%dT%H%M%SZ).txt}
mkdir -p "$(dirname "$OUT")"
# Uses SSH agent/key or prompts interactively. Never put a password on the command line.
ssh ${SSH_EXTRA_OPTS:-} -T -o BatchMode=no -o StrictHostKeyChecking=ask "$TARGET" 'sh -s' <<'REMOTE' > "$OUT"
set +e
section() { printf '\n===== %s =====\n' "$1"; }
section identity
uname -a
cat /etc/os-release 2>/dev/null
cat /etc/openwrt_release 2>/dev/null
cat /proc/cpuinfo 2>/dev/null | head -40
section memory_storage
cat /proc/meminfo 2>/dev/null | head -20
df -h 2>/dev/null
mount 2>/dev/null | head -60
section packages
if command -v opkg >/dev/null 2>&1; then opkg list-installed 2>/dev/null; fi
if command -v opkg >/dev/null 2>&1; then opkg status 2>/dev/null | grep -Ei "^(Package|Version|Depends|Provides|Status):|mesh|1905|map|qca|wireless|wifi|hostapd|wpa"; fi
if command -v apk >/dev/null 2>&1; then apk info 2>/dev/null; fi
if command -v rpm >/dev/null 2>&1; then rpm -qa 2>/dev/null; fi
section services
if command -v ubus >/dev/null 2>&1; then ubus call service list 2>/dev/null | grep -Ei -C 3 "mesh|1905|map|wireless|hostapd|wpa"; fi
find /etc/init.d /etc/rc.d /etc/rc.local /lib/systemd/system -maxdepth 2 -type f -o -type l 2>/dev/null | grep -Ei "mesh|1905|map|wireless|hostapd|wpa"
section processes
ps w 2>/dev/null

section kernel_modules
cat /proc/modules 2>/dev/null
find /lib/modules /lib/modules/* -type f \( -name '*.ko' -o -name '*.ko.*' \) 2>/dev/null
section interfaces
ip -details link 2>/dev/null
ip addr 2>/dev/null
if command -v iw >/dev/null 2>&1; then iw dev; iw phy 2>/dev/null; iw mld 2>/dev/null; fi
if command -v iwinfo >/dev/null 2>&1; then iwinfo 2>/dev/null; fi
section wireless_tools
for x in hostapd_cli wpa_cli wlanconfig iwpriv cfg80211tool; do command -v "$x" 2>/dev/null; done
section mesh_related_files
for d in /usr/bin /usr/sbin /bin /sbin /lib /usr/lib /lib/modules; do
  [ -d "$d" ] && find "$d" -maxdepth 3 -type f 2>/dev/null | grep -Ei '(easy.?mesh|multi.?ap|1905|map.?agent|map.?ctrl|mesh|qca|wlan|wifi|hostapd|wpa)' | head -400
done
section linker
cat /etc/ld.so.conf 2>/dev/null
cat /etc/ld-musl-*.path 2>/dev/null
section kernel_module_metadata_tool
command -v modinfo >/dev/null 2>&1 && modinfo -F filename 2>/dev/null
REMOTE
chmod 600 "$OUT"
printf 'Wrote read-only inventory to %s\n' "$OUT"
