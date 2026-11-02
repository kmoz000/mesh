#include "ctrl.h"

#include <stdlib.h>
#include <string.h>
#include <stdio.h>
#include <unistd.h>
#include <time.h>
#include <errno.h>
#include <sys/socket.h>
#include <sys/un.h>
#include <sys/time.h>

struct hapd_ctrl {
	int  fd;
	char local[108]; /* bound client path, for unlink on close */
};

static void set_rcv_timeout(int fd, int ms) {
	struct timeval tv;
	if (ms <= 0) ms = 2000;
	tv.tv_sec = ms / 1000;
	tv.tv_usec = (ms % 1000) * 1000;
	setsockopt(fd, SOL_SOCKET, SO_RCVTIMEO, &tv, sizeof(tv));
}

struct hapd_ctrl *hapd_open(const char *sockpath) {
	static unsigned long ctr = 0;
	struct hapd_ctrl *c;
	struct sockaddr_un local, dest;

	if (!sockpath) return NULL;
	c = (struct hapd_ctrl *)calloc(1, sizeof(*c));
	if (!c) return NULL;
	c->fd = socket(AF_UNIX, SOCK_DGRAM, 0);
	if (c->fd < 0) { free(c); return NULL; }

	memset(&local, 0, sizeof(local));
	local.sun_family = AF_UNIX;
	snprintf(c->local, sizeof(c->local), "/tmp/wmesh-ctrl-%d-%lu-%ld",
	         (int)getpid(), ctr++, (long)time(NULL));
	snprintf(local.sun_path, sizeof(local.sun_path), "%s", c->local);
	unlink(c->local);
	if (bind(c->fd, (struct sockaddr *)&local, sizeof(local)) < 0) {
		close(c->fd); free(c); return NULL;
	}

	memset(&dest, 0, sizeof(dest));
	dest.sun_family = AF_UNIX;
	snprintf(dest.sun_path, sizeof(dest.sun_path), "%s", sockpath);
	if (connect(c->fd, (struct sockaddr *)&dest, sizeof(dest)) < 0) {
		close(c->fd); unlink(c->local); free(c); return NULL;
	}
	return c;
}

void hapd_close(struct hapd_ctrl *c) {
	if (!c) return;
	if (c->fd >= 0) close(c->fd);
	if (c->local[0]) unlink(c->local);
	free(c);
}

int hapd_request(struct hapd_ctrl *c, const char *cmd,
                 char *reply, int reply_len, int timeout_ms) {
	if (!c || c->fd < 0) return -1;
	if (!cmd || !reply || reply_len <= 0) return -2;
	if (send(c->fd, cmd, strlen(cmd), 0) < 0) return -3;
	set_rcv_timeout(c->fd, timeout_ms);
	for (;;) {
		int n = recv(c->fd, reply, reply_len, 0);
		if (n < 0)
			return (errno == EAGAIN || errno == EWOULDBLOCK) ? -11 : -4;
		/* A zero-length datagram is a valid *empty* reply (e.g. STA-FIRST on a
		 * BSS with no stations), not a spurious wake-up, so return it. An event
		 * is never empty and always carries a "<N>" priority prefix. */
		if (n == 0) return 0;
		if (reply[0] == '<') continue; /* unsolicited event, skip */
		return n;
	}
}

int hapd_recv_event(struct hapd_ctrl *c, char *buf, int buf_len, int timeout_ms) {
	int n;
	if (!c || c->fd < 0) return -1;
	if (!buf || buf_len <= 0) return -2;
	set_rcv_timeout(c->fd, timeout_ms);
	n = recv(c->fd, buf, buf_len, 0);
	if (n < 0)
		return (errno == EAGAIN || errno == EWOULDBLOCK) ? 0 : -4;
	return n;
}
