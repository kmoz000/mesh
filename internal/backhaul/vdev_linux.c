#include "vdev.h"

#include <stdint.h>
#include <string.h>
#include <errno.h>
#include <sys/socket.h>
#include <sys/ioctl.h>
#include <sys/time.h>
#include <net/if.h>
#include <unistd.h>
#include <linux/netlink.h>
#include <linux/genetlink.h>
#include "nl80211.h" /* via -I ../nlwifi (CFLAGS) — authoritative enum values */

/* libnl-3 / libnl-genl-3 (opaque; the device provides the libs). Same minimal
 * surface nlwifi uses, plus nla_put_string / nla_put_u8 for interface create. */
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
extern int             nla_put_u8(struct nl_msg *, int attrtype, uint8_t value);
extern int             nla_put_string(struct nl_msg *, int attrtype, const char *str);
extern int             nl_send_auto(struct nl_sock *, struct nl_msg *);

/* Send a prepared command message and wait for its netlink ACK, returning the
 * ack error (0 = success) or a negative transport code. Other messages the
 * kernel emits first (e.g. the NEW_INTERFACE notification) are skipped until the
 * terminating NLMSG_ERROR/ACK. A 3 s socket receive timeout bounds the wait. */
static int send_and_ack(struct nl_sock *sk, struct nl_msg *msg) {
	int fd = nl_socket_get_fd(sk);
	struct timeval tv = { .tv_sec = 3, .tv_usec = 0 };
	char buf[8192];
	int rc;

	setsockopt(fd, SOL_SOCKET, SO_RCVTIMEO, &tv, sizeof(tv));
	rc = nl_send_auto(sk, msg);
	nlmsg_free(msg);
	if (rc < 0) return -20;

	for (;;) {
		int n = recv(fd, buf, sizeof(buf), 0);
		char *p = buf;
		int left;
		if (n <= 0) return -21; /* timeout or error with no ack */
		left = n;
		while (left >= (int)sizeof(struct nlmsghdr)) {
			struct nlmsghdr *nlh = (struct nlmsghdr *)p;
			int aligned;
			if (nlh->nlmsg_len < sizeof(*nlh) || (int)nlh->nlmsg_len > left)
				return -22;
			if (nlh->nlmsg_type == NLMSG_ERROR) {
				struct nlmsgerr *e = (struct nlmsgerr *)NLMSG_DATA(nlh);
				return e->error; /* 0 = ack/success, else -errno from kernel */
			}
			aligned = NLMSG_ALIGN(nlh->nlmsg_len);
			p += aligned; left -= aligned;
		}
	}
}

static struct nl_sock *open_nl80211(int *fam_out) {
	struct nl_sock *sk = nl_socket_alloc();
	int fam;
	if (!sk) return 0;
	if (genl_connect(sk)) { nl_socket_free(sk); return 0; }
	fam = genl_ctrl_resolve(sk, "nl80211");
	if (fam < 0) { nl_socket_free(sk); return 0; }
	*fam_out = fam;
	return sk;
}

int bh_new_sta_vdev(int phy, const char *ifname, int fouraddr) {
	struct nl_sock *sk;
	struct nl_msg *msg;
	int fam;
	if (!ifname || phy < 0) return -10;
	sk = open_nl80211(&fam);
	if (!sk) return -11;
	msg = nlmsg_alloc();
	if (!msg) { nl_socket_free(sk); return -12; }
	genlmsg_put(msg, 0, 0, fam, 0, NLM_F_REQUEST | NLM_F_ACK,
	            NL80211_CMD_NEW_INTERFACE, 0);
	nla_put_u32(msg, NL80211_ATTR_WIPHY, (uint32_t)phy);
	nla_put_string(msg, NL80211_ATTR_IFNAME, ifname);
	nla_put_u32(msg, NL80211_ATTR_IFTYPE, NL80211_IFTYPE_STATION);
	if (fouraddr) nla_put_u8(msg, NL80211_ATTR_4ADDR, 1);
	{
		int rc = send_and_ack(sk, msg);
		nl_socket_free(sk);
		return rc;
	}
}

int bh_del_vdev(int ifindex) {
	struct nl_sock *sk;
	struct nl_msg *msg;
	int fam;
	if (ifindex <= 0) return -10;
	sk = open_nl80211(&fam);
	if (!sk) return -11;
	msg = nlmsg_alloc();
	if (!msg) { nl_socket_free(sk); return -12; }
	genlmsg_put(msg, 0, 0, fam, 0, NLM_F_REQUEST | NLM_F_ACK,
	            NL80211_CMD_DEL_INTERFACE, 0);
	nla_put_u32(msg, NL80211_ATTR_IFINDEX, (uint32_t)ifindex);
	{
		int rc = send_and_ack(sk, msg);
		nl_socket_free(sk);
		return rc;
	}
}

int bh_if_up(const char *ifname) {
	int fd, rc;
	struct ifreq ifr;
	if (!ifname) return -1;
	fd = socket(AF_INET, SOCK_DGRAM, 0);
	if (fd < 0) return -errno;
	memset(&ifr, 0, sizeof(ifr));
	strncpy(ifr.ifr_name, ifname, IFNAMSIZ - 1);
	if (ioctl(fd, SIOCGIFFLAGS, &ifr) < 0) { rc = -errno; close(fd); return rc; }
	ifr.ifr_flags |= IFF_UP;
	if (ioctl(fd, SIOCSIFFLAGS, &ifr) < 0) { rc = -errno; close(fd); return rc; }
	close(fd);
	return 0;
}
