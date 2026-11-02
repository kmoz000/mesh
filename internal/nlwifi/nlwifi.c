#include "nlwifi.h"

#include <stdint.h>
#include <string.h>
#include <sys/socket.h>
#include <linux/netlink.h>
#include <linux/genetlink.h>
#include "nl80211.h"

/* --- libnl-3 / libnl-genl-3 prototypes (opaque; the device provides the libs).
 * We use libnl only for the socket, family resolution and message build/send,
 * then read and parse the dump ourselves from the socket fd, so no libnl
 * callback/enum surface is needed (those constants vary; functions are stable). */
struct nl_sock;
struct nl_msg;
extern struct nl_sock *nl_socket_alloc(void);
extern void            nl_socket_free(struct nl_sock *);
extern int             nl_socket_get_fd(const struct nl_sock *);
extern int             genl_connect(struct nl_sock *);
extern int             genl_ctrl_resolve(struct nl_sock *, const char *);
extern struct nl_msg  *nlmsg_alloc(void);
extern void            nlmsg_free(struct nl_msg *);
extern void           *genlmsg_put(struct nl_msg *, uint32_t port, uint32_t seq,
                                   int family, int hdrlen, int flags,
                                   uint8_t cmd, uint8_t version);
extern int             nla_put_u32(struct nl_msg *, int attrtype, uint32_t value);
extern int             nla_put(struct nl_msg *, int attrtype, int attrlen, const void *data);
extern int             nl_send_auto(struct nl_sock *, struct nl_msg *);

/* --- minimal netlink attribute walking (manual, header-free of libnl) --- */
#define WNLA_HDRLEN ((int)NLA_ALIGN(sizeof(struct nlattr)))

static int wnla_ok(const struct nlattr *a, int rem) {
	return rem >= (int)sizeof(*a) && a->nla_len >= sizeof(*a) &&
	       (int)a->nla_len <= rem;
}
static struct nlattr *wnla_next(const struct nlattr *a, int *rem) {
	int len = NLA_ALIGN(a->nla_len);
	*rem -= len;
	return (struct nlattr *)((char *)a + len);
}
static void *wnla_data(const struct nlattr *a) { return (char *)a + WNLA_HDRLEN; }
static int   wnla_len(const struct nlattr *a)  { return (int)a->nla_len - WNLA_HDRLEN; }
static int   wnla_type(const struct nlattr *a) {
	return a->nla_type & ~(NLA_F_NESTED | NLA_F_NET_BYTEORDER);
}
static uint32_t wnla_u32(const struct nlattr *a) {
	uint32_t v = 0; if (wnla_len(a) >= 4) memcpy(&v, wnla_data(a), 4); return v;
}
static uint16_t wnla_u16(const struct nlattr *a) {
	uint16_t v = 0; if (wnla_len(a) >= 2) memcpy(&v, wnla_data(a), 2); return v;
}
static uint64_t wnla_u64(const struct nlattr *a) {
	uint64_t v = 0; if (wnla_len(a) >= 8) memcpy(&v, wnla_data(a), 8); return v;
}

/* find attribute of type t within [attrs, attrs+len). */
static struct nlattr *find_attr(struct nlattr *attrs, int len, int t) {
	struct nlattr *a; int rem;
	for (a = attrs, rem = len; wnla_ok(a, rem); a = wnla_next(a, &rem))
		if (wnla_type(a) == t) return a;
	return 0;
}

typedef void (*msg_cb)(struct nlattr *attrs, int len, void *ctx);

/* send a GET_<cmd> dump for ifindex and feed each reply message's top-level
 * attribute region to cb. */
static int do_dump(uint8_t cmd, int ifindex, msg_cb cb, void *ctx) {
	struct nl_sock *sk;
	struct nl_msg *msg;
	int fam, fd, rc, done = 0, ret = 0;
	char buf[32768];

	sk = nl_socket_alloc();
	if (!sk) return -1;
	if (genl_connect(sk)) { nl_socket_free(sk); return -2; }
	fam = genl_ctrl_resolve(sk, "nl80211");
	if (fam < 0) { nl_socket_free(sk); return -3; }
	msg = nlmsg_alloc();
	if (!msg) { nl_socket_free(sk); return -4; }
	genlmsg_put(msg, 0, 0, fam, 0, NLM_F_DUMP, cmd, 0);
	nla_put_u32(msg, NL80211_ATTR_IFINDEX, (uint32_t)ifindex);
	fd = nl_socket_get_fd(sk);
	rc = nl_send_auto(sk, msg);
	nlmsg_free(msg);
	if (rc < 0) { nl_socket_free(sk); return -5; }

	while (!done) {
		int n = recv(fd, buf, sizeof(buf), 0);
		char *p = buf;
		int left;
		if (n <= 0) { ret = -6; break; }
		left = n;
		while (left >= (int)sizeof(struct nlmsghdr)) {
			struct nlmsghdr *nlh = (struct nlmsghdr *)p;
			int aligned;
			if (nlh->nlmsg_len < sizeof(*nlh) || (int)nlh->nlmsg_len > left) {
				done = 1; break;
			}
			if (nlh->nlmsg_type == NLMSG_DONE) { done = 1; break; }
			if (nlh->nlmsg_type == NLMSG_ERROR) { done = 1; ret = -7; break; }
			{
				struct genlmsghdr *gnlh =
					(struct genlmsghdr *)((char *)nlh + NLMSG_HDRLEN);
				struct nlattr *attrs =
					(struct nlattr *)((char *)gnlh + GENL_HDRLEN);
				int alen = (int)nlh->nlmsg_len - NLMSG_HDRLEN - GENL_HDRLEN;
				if (alen > 0) cb(attrs, alen, ctx);
			}
			aligned = NLMSG_ALIGN(nlh->nlmsg_len);
			p += aligned; left -= aligned;
		}
	}
	nl_socket_free(sk);
	return ret;
}

/* --- GET_STATION --- */
struct sta_ctx { struct wifi_sta *out; int max; int n; };

/* parse a nested RATE_INFO, returning the bitrate in units of 100 kbps. */
static uint32_t parse_rate(struct nlattr *rate, int len) {
	struct nlattr *a;
	a = find_attr(rate, len, NL80211_RATE_INFO_BITRATE32);
	if (a) return wnla_u32(a);
	a = find_attr(rate, len, NL80211_RATE_INFO_BITRATE);
	if (a) return (uint32_t)wnla_u16(a);
	return 0;
}

static void sta_cb(struct nlattr *attrs, int len, void *vctx) {
	struct sta_ctx *c = (struct sta_ctx *)vctx;
	struct nlattr *mac, *info, *a;
	struct wifi_sta *s;
	if (c->n >= c->max) return;
	mac = find_attr(attrs, len, NL80211_ATTR_MAC);
	if (!mac || wnla_len(mac) < 6) return;
	s = &c->out[c->n];
	memset(s, 0, sizeof(*s));
	memcpy(s->mac, wnla_data(mac), 6);
	info = find_attr(attrs, len, NL80211_ATTR_STA_INFO);
	if (info) {
		int ilen = wnla_len(info);
		struct nlattr *in = (struct nlattr *)wnla_data(info);
		a = find_attr(in, ilen, NL80211_STA_INFO_SIGNAL);
		if (a) { s->signal_dbm = *(int8_t *)wnla_data(a); s->have |= 1; }
		a = find_attr(in, ilen, NL80211_STA_INFO_INACTIVE_TIME);
		if (a) s->inactive_ms = wnla_u32(a);
		a = find_attr(in, ilen, NL80211_STA_INFO_RX_BITRATE);
		if (a) { s->rx_bitrate = parse_rate((struct nlattr *)wnla_data(a), wnla_len(a)); if (s->rx_bitrate) s->have |= 2; }
		a = find_attr(in, ilen, NL80211_STA_INFO_TX_BITRATE);
		if (a) { s->tx_bitrate = parse_rate((struct nlattr *)wnla_data(a), wnla_len(a)); if (s->tx_bitrate) s->have |= 4; }
	}
	c->n++;
}

int wifi_dump_stations(int ifindex, struct wifi_sta *out, int max) {
	struct sta_ctx c; int rc;
	if (!out || max <= 0) return -10;
	c.out = out; c.max = max; c.n = 0;
	rc = do_dump(NL80211_CMD_GET_STATION, ifindex, sta_cb, &c);
	if (rc < 0 && c.n == 0) return rc;
	return c.n;
}

/* doit (non-dump) GET_STATION for one (ifindex, mac): QCA AP vaps answer the
 * by-MAC query even when the dump is empty. Reads the single reply datagram. */
static int do_station_get(int ifindex, const uint8_t mac[6], msg_cb cb, void *ctx) {
	struct nl_sock *sk;
	struct nl_msg *msg;
	int fam, fd, rc, ret = 0, n;
	char buf[8192];
	struct nlmsghdr *nlh;

	sk = nl_socket_alloc();
	if (!sk) return -1;
	if (genl_connect(sk)) { nl_socket_free(sk); return -2; }
	fam = genl_ctrl_resolve(sk, "nl80211");
	if (fam < 0) { nl_socket_free(sk); return -3; }
	msg = nlmsg_alloc();
	if (!msg) { nl_socket_free(sk); return -4; }
	genlmsg_put(msg, 0, 0, fam, 0, NLM_F_REQUEST, NL80211_CMD_GET_STATION, 0);
	nla_put_u32(msg, NL80211_ATTR_IFINDEX, (uint32_t)ifindex);
	nla_put(msg, NL80211_ATTR_MAC, 6, mac);
	fd = nl_socket_get_fd(sk);
	rc = nl_send_auto(sk, msg);
	nlmsg_free(msg);
	if (rc < 0) { nl_socket_free(sk); return -5; }

	n = recv(fd, buf, sizeof(buf), 0);
	if (n < (int)sizeof(struct nlmsghdr)) { nl_socket_free(sk); return -6; }
	nlh = (struct nlmsghdr *)buf;
	if (nlh->nlmsg_type == NLMSG_ERROR) {
		struct nlmsgerr *e = (struct nlmsgerr *)NLMSG_DATA(nlh);
		/* error==0 is a bare ACK (station not present); otherwise a real error */
		ret = (e->error == 0) ? 0 : -7;
	} else if ((int)nlh->nlmsg_len >= NLMSG_HDRLEN + GENL_HDRLEN &&
	           (int)nlh->nlmsg_len <= n) {
		struct genlmsghdr *g = (struct genlmsghdr *)((char *)nlh + NLMSG_HDRLEN);
		struct nlattr *attrs = (struct nlattr *)((char *)g + GENL_HDRLEN);
		int alen = (int)nlh->nlmsg_len - NLMSG_HDRLEN - GENL_HDRLEN;
		if (alen > 0) { cb(attrs, alen, ctx); ret = 1; }
	}
	nl_socket_free(sk);
	return ret;
}

int wifi_get_station(int ifindex, const uint8_t mac[6], struct wifi_sta *out) {
	struct sta_ctx c;
	int rc;
	if (!out || !mac) return -10;
	c.out = out; c.max = 1; c.n = 0;
	rc = do_station_get(ifindex, mac, sta_cb, &c);
	if (rc < 0) return rc;
	return c.n > 0 ? 1 : 0;
}

/* --- GET_SURVEY --- */
struct survey_ctx { struct wifi_survey *out; int found; };

static void survey_cb(struct nlattr *attrs, int len, void *vctx) {
	struct survey_ctx *c = (struct survey_ctx *)vctx;
	struct nlattr *info, *a;
	int ilen;
	struct nlattr *in;
	if (c->found) return;
	info = find_attr(attrs, len, NL80211_ATTR_SURVEY_INFO);
	if (!info) return;
	ilen = wnla_len(info);
	in = (struct nlattr *)wnla_data(info);
	if (!find_attr(in, ilen, NL80211_SURVEY_INFO_IN_USE)) return; /* only the active channel */
	memset(c->out, 0, sizeof(*c->out));
	a = find_attr(in, ilen, NL80211_SURVEY_INFO_FREQUENCY);
	if (a) c->out->freq_mhz = wnla_u32(a);
	a = find_attr(in, ilen, NL80211_SURVEY_INFO_NOISE);
	if (a) c->out->noise_dbm = *(int8_t *)wnla_data(a);
	a = find_attr(in, ilen, NL80211_SURVEY_INFO_TIME);
	if (a) c->out->active_ms = wnla_u64(a);
	a = find_attr(in, ilen, NL80211_SURVEY_INFO_TIME_BUSY);
	if (a) c->out->busy_ms = wnla_u64(a);
	c->found = 1;
}

int wifi_survey_inuse(int ifindex, struct wifi_survey *out) {
	struct survey_ctx c; int rc;
	if (!out) return -10;
	c.out = out; c.found = 0;
	rc = do_dump(NL80211_CMD_GET_SURVEY, ifindex, survey_cb, &c);
	if (rc < 0 && !c.found) return rc;
	return c.found ? 1 : 0;
}
