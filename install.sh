#!/bin/sh
# wmeshd one-line installer (bootstrap) — ADDITIVE ONLY.
#
#   curl -fsSL https://raw.githubusercontent.com/kmoz000/mesh/main/install.sh | sh
#   # or, on firmware without curl:
#   wget -qO- https://raw.githubusercontent.com/kmoz000/mesh/main/install.sh | sh
#
# Downloads the daemon, a procd service and a default (disabled) config from the
# repo and installs them. It NEVER edits /etc/config/wireless, never runs `wifi`,
# never reboots — proven by a sha256 of /etc/config/wireless before and after.
# Re-runnable (upgrade). Offline alternative: clone the repo and run
# install/install.sh.
set -e

REPO="${WMESH_REPO:-kmoz000/mesh}"
REF="${WMESH_REF:-main}"
RAW="https://raw.githubusercontent.com/$REPO/$REF"
BIN_DST=/usr/sbin/wmeshd

say() { echo "wmeshd-install: $*"; }

dl() { # dl <url> <outfile>
	if command -v curl >/dev/null 2>&1; then curl -fsSL "$1" -o "$2"
	elif command -v uclient-fetch >/dev/null 2>&1; then uclient-fetch -q -O "$2" "$1"
	elif command -v wget >/dev/null 2>&1; then wget -q -O "$2" "$1"
	else say "need curl, wget or uclient-fetch"; return 1; fi
}

say "additive install from $RAW"
[ "$(uname -m)" = "aarch64" ] || say "warning: this host is $(uname -m), not aarch64 — the T2 binary may not run"

WPRE=$(sha256sum /etc/config/wireless 2>/dev/null | awk '{print $1}')
say "wireless sha256 (before): ${WPRE:-<none>}"

TMP=$(mktemp -d /tmp/wmeshd-install.XXXXXX) || TMP=/tmp/wmeshd-install.$$
mkdir -p "$TMP"
trap 'rm -rf "$TMP"' EXIT INT TERM

say "downloading daemon + service..."
dl "$RAW/install/bin/wmeshd-aarch64"        "$TMP/wmeshd" || { say "download failed"; exit 1; }
dl "$RAW/install/files/etc/init.d/wmeshd"   "$TMP/init"   || { say "download failed"; exit 1; }
dl "$RAW/install/files/etc/config/wmesh"    "$TMP/wmesh"  || { say "download failed"; exit 1; }

# A 404 from raw.githubusercontent is a small HTML page; the real binary is ~2 MB.
if [ "$(wc -c < "$TMP/wmeshd")" -lt 1000000 ]; then
	say "downloaded binary looks wrong (check WMESH_REPO/WMESH_REF)"; exit 1
fi

cp "$TMP/wmeshd" "$BIN_DST"; chmod 0755 "$BIN_DST"
cp "$TMP/init" /etc/init.d/wmeshd; chmod 0755 /etc/init.d/wmeshd
mkdir -p /etc/wmeshd; chmod 0700 /etc/wmeshd

if uci -q get wmesh.global >/dev/null 2>&1; then
	say "config: /etc/config/wmesh exists — preserving operator settings"
else
	cp "$TMP/wmesh" /etc/config/wmesh; chmod 0644 /etc/config/wmesh
	say "config: installed default /etc/config/wmesh (enabled=0)"
fi

/etc/init.d/wmeshd enable 2>/dev/null || true

WPOST=$(sha256sum /etc/config/wireless 2>/dev/null | awk '{print $1}')
say "wireless sha256 (after):  ${WPOST:-<none>}"
if [ "$WPRE" != "$WPOST" ]; then
	say "ERROR: /etc/config/wireless changed — this must never happen."; exit 2
fi
say "OK: AP/wireless config unchanged (installer is additive)."

cat <<'NEXT'

Next steps:
  1) Network key (64 hex, same on every node):
       printf '%s' '<64-hex>' > /etc/wmeshd/net.key && chmod 600 /etc/wmeshd/net.key
  2) Role:
       # leaf:  uci set wmesh.global.net='<16-hex>'; uci set wmesh.global.controller_id='<32-hex>'
       #        uci set wmesh.global.backhaul_ssid='wmesh-bh'; uci set wmesh.global.backhaul_radio='wifi2'
       # root:  uci set wmesh.global.role='root';   uci set wmesh.global.net='<16-hex>'
  3) Enable + start:
       uci set wmesh.global.enabled='1' && uci commit wmesh && /etc/init.d/wmeshd start
  4) Verify (root): cat /tmp/wmesh-state.txt
NEXT
