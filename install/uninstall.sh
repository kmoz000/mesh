#!/bin/sh
# wmeshd uninstaller — ADDITIVE removal. Stops the daemon, removes the runtime
# backhaul STA vdev (also cleared by any reboot), disables and removes the
# service. It does NOT touch /etc/config/wireless.
WPRE=$(sha256sum /etc/config/wireless 2>/dev/null | awk '{print $1}')
STA_IF=$(uci -q get wmesh.global.backhaul_sta_if 2>/dev/null); [ -n "$STA_IF" ] || STA_IF=wmbhsta0

/etc/init.d/wmeshd stop 2>/dev/null || true
[ -x /usr/sbin/wmeshd ] && /usr/sbin/wmeshd backhaul down -if "$STA_IF" 2>/dev/null || true
/etc/init.d/wmeshd disable 2>/dev/null || true
rm -f /etc/init.d/wmeshd /usr/sbin/wmeshd

echo "removed daemon + service. Kept /etc/config/wmesh and /etc/wmeshd/ (remove by hand if wanted)."
echo "the backhaul STA vdev is runtime-only; a reboot also clears it."
WPOST=$(sha256sum /etc/config/wireless 2>/dev/null | awk '{print $1}')
[ "$WPRE" = "$WPOST" ] && echo "OK: AP/wireless config unchanged." || echo "!! wireless changed unexpectedly" >&2
