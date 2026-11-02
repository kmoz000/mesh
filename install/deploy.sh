#!/bin/sh
# Push wmeshd to one or more T2 devices over SSH and run the additive installer.
#
#   ./deploy.sh root@HOST [root@HOST ...]
#
# Uses SSH keys by default. For old T2 firmware that needs legacy algorithms,
# export SSH/SCP, e.g.:
#   export SSH='sshpass -e ssh -o KexAlgorithms=+diffie-hellman-group14-sha1 \
#     -o HostKeyAlgorithms=+ssh-rsa -o PubkeyAcceptedAlgorithms=+ssh-rsa \
#     -o Ciphers=+aes128-cbc -o MACs=+hmac-sha1 -o StrictHostKeyChecking=no'
#   export SCP='sshpass -e scp -O <same -o flags>'   # SSHPASS=<password>
set -e
ROOT="$(cd "$(dirname "$0")/.." && pwd)"
BIN="$ROOT/install/bin/wmeshd-aarch64"
[ -f "$BIN" ] || { echo "missing $BIN — build with: tools/build-cgo.sh install/bin/wmeshd-aarch64"; exit 1; }
SSH="${SSH:-ssh -o StrictHostKeyChecking=accept-new}"
SCP="${SCP:-scp -O}"
[ "$#" -ge 1 ] || { echo "usage: $0 root@HOST [root@HOST ...]"; exit 2; }

for H in "$@"; do
	echo "== deploying to $H =="
	$SSH "$H" 'mkdir -p /tmp/wmi/bin /tmp/wmi/files/etc/init.d /tmp/wmi/files/etc/config'
	$SCP "$BIN" "$H:/tmp/wmi/bin/wmeshd-aarch64"
	$SCP "$ROOT/install/files/etc/init.d/wmeshd" "$H:/tmp/wmi/files/etc/init.d/wmeshd"
	$SCP "$ROOT/install/files/etc/config/wmesh" "$H:/tmp/wmi/files/etc/config/wmesh"
	$SCP "$ROOT/install/install.sh" "$H:/tmp/wmi/install.sh"
	$SSH "$H" 'sh /tmp/wmi/install.sh'
	echo
done
echo "done. Configure each node per its role, then: uci set wmesh.global.enabled=1; uci commit wmesh; /etc/init.d/wmeshd start"
