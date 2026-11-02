#!/bin/sh
# wmeshd installer — ADDITIVE ONLY.
#
# Installs the daemon, a procd service and a default (disabled) config. It NEVER
# edits /etc/config/wireless, never runs `wifi`, never reboots. The existing
# (PMTP) AP config is left byte-for-byte unchanged — proven by a sha256 of
# /etc/config/wireless taken before and after. Safe to re-run (upgrade).
set -e
SRC="$(cd "$(dirname "$0")" && pwd)"
BIN_SRC="$SRC/bin/wmeshd-aarch64"
BIN_DST=/usr/sbin/wmeshd

echo "== wmeshd installer (additive) =="
[ -f "$BIN_SRC" ] || { echo "missing $BIN_SRC"; exit 1; }

WPRE=$(sha256sum /etc/config/wireless 2>/dev/null | awk '{print $1}')
echo "wireless sha256 (before): ${WPRE:-<none>}"

cp "$BIN_SRC" "$BIN_DST"; chmod 0755 "$BIN_DST"
cp "$SRC/files/etc/init.d/wmeshd" /etc/init.d/wmeshd; chmod 0755 /etc/init.d/wmeshd
mkdir -p /etc/wmeshd; chmod 0700 /etc/wmeshd

if uci -q get wmesh.global >/dev/null 2>&1; then
	echo "config: /etc/config/wmesh exists — preserving operator settings"
else
	cp "$SRC/files/etc/config/wmesh" /etc/config/wmesh
	chmod 0644 /etc/config/wmesh
	echo "config: installed default /etc/config/wmesh (enabled=0)"
fi

/etc/init.d/wmeshd enable 2>/dev/null || true

WPOST=$(sha256sum /etc/config/wireless 2>/dev/null | awk '{print $1}')
echo "wireless sha256 (after):  ${WPOST:-<none>}"
if [ "$WPRE" != "$WPOST" ]; then
	echo "!! ERROR: /etc/config/wireless changed — this must never happen." >&2
	exit 2
fi
echo "OK: AP/wireless config unchanged (installer is additive)."

cat <<'NEXT'

Next steps:
  1) Put the 32-byte network key (64 hex chars) at /etc/wmeshd/net.key:
       printf '%s' '<64-hex>' > /etc/wmeshd/net.key && chmod 600 /etc/wmeshd/net.key
  2) Configure the role:
       # leaf (relay):
       uci set wmesh.global.net='<16-hex>'
       uci set wmesh.global.controller_id='<32-hex>'
       uci set wmesh.global.backhaul_ssid='wmesh-bh'   # to form the wireless backhaul
       uci set wmesh.global.backhaul_radio='wifi2'     # the 6 GHz radio
       # root (controller): uci set wmesh.global.role='root'
  3) Enable + start:
       uci set wmesh.global.enabled='1' && uci commit wmesh
       /etc/init.d/wmeshd start
  4) Verify (on the root): cat /tmp/wmesh-state.txt

The root AP 'wmesh-bh' is stood up once per network (see README, "Root AP").
NEXT
