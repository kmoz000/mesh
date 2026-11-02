#ifndef WMESH_BACKHAUL_VDEV_H
#define WMESH_BACKHAUL_VDEV_H

#include <stdint.h>

/* Backhaul STA vdev lifecycle over standard nl80211 (no QCA vendor command):
 * create a managed (station) vdev on a phy, delete it, and bring an interface
 * up. This is the create path that `iw phy <phy> interface add <if> type
 * managed` exercises and that was proven to work on the target; the daemon does
 * it in-process instead of shelling out. Association itself is driven over the
 * wpa_supplicant control socket, not here. */

/* Create a managed (NL80211_IFTYPE_STATION) vdev named ifname on wiphy `phy`.
 * fouraddr != 0 sets NL80211_ATTR_4ADDR (WDS/Multi-AP backhaul). Returns 0 on
 * success or a negative error code (the netlink ack error is folded in). */
int bh_new_sta_vdev(int phy, const char *ifname, int fouraddr);

/* Delete the vdev at ifindex (NL80211_CMD_DEL_INTERFACE). 0 or negative. */
int bh_del_vdev(int ifindex);

/* Bring ifname administratively up (SIOCSIFFLAGS |= IFF_UP). 0 or -errno. */
int bh_if_up(const char *ifname);

#endif /* WMESH_BACKHAUL_VDEV_H */
