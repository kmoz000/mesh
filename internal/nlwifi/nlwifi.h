#ifndef WMESH_NLWIFI_H
#define WMESH_NLWIFI_H

#include <stdint.h>

/* Measured Wi-Fi telemetry read through the device's own libnl-genl / nl80211
 * (read-only: GET_STATION and GET_SURVEY dumps). This is the per-link metric
 * source (RSSI, PHY bitrate, channel airtime) that replaces inferred topology
 * links with measured ones. No QCA vendor nl80211 command is used. */

struct wifi_sta {
	uint8_t  mac[6];
	int8_t   signal_dbm;   /* NL80211_STA_INFO_SIGNAL (dBm) */
	uint8_t  _pad;
	uint32_t rx_bitrate;   /* RX PHY rate, units of 100 kbps */
	uint32_t tx_bitrate;   /* TX PHY rate, units of 100 kbps */
	uint32_t inactive_ms;  /* NL80211_STA_INFO_INACTIVE_TIME */
	uint32_t have;         /* bit0 signal, bit1 rx_rate, bit2 tx_rate */
};

struct wifi_survey {
	uint32_t           freq_mhz;  /* in-use channel frequency */
	int8_t             noise_dbm;
	uint8_t            _pad[3];
	unsigned long long active_ms; /* NL80211_SURVEY_INFO_TIME (channel active) */
	unsigned long long busy_ms;   /* NL80211_SURVEY_INFO_TIME_BUSY */
};

/* Dump associated stations on ifindex into out[0..max). Returns the count
 * written (>=0), or a negative error code. */
int wifi_dump_stations(int ifindex, struct wifi_sta *out, int max);

/* Fill out with the in-use channel's survey for ifindex. Returns 1 if an in-use
 * channel was found, 0 if none, or a negative error code. */
int wifi_survey_inuse(int ifindex, struct wifi_survey *out);

/* Fetch one station's metrics by MAC via a doit GET_STATION (read-only). On QCA
 * AP vaps the GET_STATION *dump* returns nothing, but a doit by MAC works; this
 * is how the backhaul peer's RSSI/rate is read from either end. Returns 1 if the
 * station was found, 0 if absent, or a negative error code. */
int wifi_get_station(int ifindex, const uint8_t mac[6], struct wifi_sta *out);

#endif /* WMESH_NLWIFI_H */
