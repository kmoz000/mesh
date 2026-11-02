#ifndef WMESH_HOSTAPD_CTRL_H
#define WMESH_HOSTAPD_CTRL_H

/* Compact hostapd/wpa_supplicant control-interface client over AF_UNIX
 * SOCK_DGRAM, modeled on hostap's BSD-licensed wpa_ctrl.c UNIX path. The target
 * ships no libwpa_client.so (hostapd links it statically), so wmeshd carries its
 * own minimal client. One datagram socket per connection; hostapd replies to the
 * client's bound address. */

struct hapd_ctrl;

/* Open a connection to the per-interface control socket at sockpath
 * (e.g. "/var/run/hostapd-wifi0/ath0"). NULL on failure. */
struct hapd_ctrl *hapd_open(const char *sockpath);

/* Close and free; unlinks the client's bound path. */
void hapd_close(struct hapd_ctrl *c);

/* Send cmd and collect the reply into reply[0..reply_len). Unsolicited event
 * datagrams ("<N>...") that arrive meanwhile are skipped. Returns the reply byte
 * count (not NUL-terminated), -11 on timeout, or a negative code on error. */
int hapd_request(struct hapd_ctrl *c, const char *cmd,
                 char *reply, int reply_len, int timeout_ms);

/* Receive one event datagram (use after sending ATTACH). Returns the byte count,
 * 0 on timeout, or a negative code on error. */
int hapd_recv_event(struct hapd_ctrl *c, char *buf, int buf_len, int timeout_ms);

#endif /* WMESH_HOSTAPD_CTRL_H */
