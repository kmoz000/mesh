package backhaul

import (
	"strconv"
	"strings"
)

// Config describes one backhaul STA link to form.
type Config struct {
	Phy         int    // wiphy index of the STA radio (see PhyIndexOf)
	IfName      string // vdev name to create, e.g. "ath2"
	SSID        string // backhaul SSID, e.g. "wmesh-bh"
	BSSID       string // optional AP BSSID lock "aa:bb:.."; "" to leave open
	SAEPassword string // SAE password = the derived backhaul key (hex string)
	FourAddr    bool   // create the vdev with 4addr (WDS/Multi-AP backhaul)
	ScanFreqMHz int    // optional: pin scan to this frequency (e.g. 6595)
	GlobalCtrl  string // global supplicant ctrl socket; default below
	CtrlDir     string // per-interface ctrl dir; default below
}

const (
	defaultGlobalCtrl = "/var/run/wpa_supplicantglobal"
	defaultCtrlDir    = "/var/run/wpa_supplicant"
)

func (c Config) withDefaults() Config {
	if c.GlobalCtrl == "" {
		c.GlobalCtrl = defaultGlobalCtrl
	}
	if c.CtrlDir == "" {
		c.CtrlDir = defaultCtrlDir
	}
	return c
}

// This file holds the pure, device-free helpers: the wpa_supplicant control
// command strings and the reply parsers. They are unit-tested without hardware.

// qc quotes a value the wpa_ctrl way (SET_NETWORK string params are quoted).
func qc(s string) string { return "\"" + s + "\"" }

// interfaceAddCmd builds the global-socket INTERFACE_ADD for a pre-created vdev.
// The fields are TAB-separated: ifname, config (empty — configured live over the
// control interface), driver, ctrl-interface dir. With the vdev already created
// via NL80211_CMD_NEW_INTERFACE, the running global supplicant adopts it (proven
// on device: INTERFACE_ADD fails only when no vdev exists yet).
func interfaceAddCmd(ifname, ctrlDir string) string {
	return "INTERFACE_ADD " + strings.Join([]string{ifname, "", "nl80211", ctrlDir}, "\t")
}

// netCommands is the per-interface command sequence that configures and enables
// the SAE backhaul network after ADD_NETWORK returned netID. It mirrors exactly
// the SET_NETWORK parameters proven to associate on the target: WPA3-SAE with
// management-frame protection required and CCMP, optionally bssid-locked and
// frequency-pinned so the 6 GHz scan is immediate.
func netCommands(cfg Config, netID string) []string {
	cmds := []string{
		"SET_NETWORK " + netID + " ssid " + qc(cfg.SSID),
		"SET_NETWORK " + netID + " key_mgmt SAE",
		"SET_NETWORK " + netID + " sae_password " + qc(cfg.SAEPassword),
		"SET_NETWORK " + netID + " ieee80211w 2",
		"SET_NETWORK " + netID + " pairwise CCMP",
		"SET_NETWORK " + netID + " group CCMP",
	}
	if cfg.BSSID != "" {
		cmds = append(cmds, "SET_NETWORK "+netID+" bssid "+cfg.BSSID)
	}
	if cfg.ScanFreqMHz > 0 {
		f := strconv.Itoa(cfg.ScanFreqMHz)
		cmds = append(cmds,
			"SET_NETWORK "+netID+" scan_freq "+f,
			"SET_NETWORK "+netID+" freq_list "+f)
	}
	cmds = append(cmds, "ENABLE_NETWORK "+netID)
	return cmds
}

// netIDFromReply extracts the numeric network id from an ADD_NETWORK reply, which
// is just the id on its own line (e.g. "0\n"). "" if the reply is not an id.
func netIDFromReply(r string) string {
	r = strings.TrimSpace(r)
	if r == "" || strings.HasPrefix(r, "FAIL") {
		return ""
	}
	if i := strings.IndexByte(r, '\n'); i >= 0 {
		r = r[:i]
	}
	r = strings.TrimSpace(r)
	for _, c := range r {
		if c < '0' || c > '9' {
			return ""
		}
	}
	return r
}

// statusField returns the value of key=val field key in a wpa_supplicant STATUS
// reply (one key=value per line), or "" if absent.
func statusField(status, key string) string {
	for _, ln := range strings.Split(status, "\n") {
		ln = strings.TrimSpace(ln)
		if strings.HasPrefix(ln, key+"=") {
			return ln[len(key)+1:]
		}
	}
	return ""
}

// statusComplete reports whether a STATUS reply shows a completed association.
func statusComplete(status string) bool {
	return statusField(status, "wpa_state") == "COMPLETED"
}

// parseMAC parses "aa:bb:cc:dd:ee:ff" into 6 bytes. ok=false on malformed input.
func parseMAC(s string) (mac [6]byte, ok bool) {
	parts := strings.Split(strings.TrimSpace(s), ":")
	if len(parts) != 6 {
		return mac, false
	}
	for i, p := range parts {
		v, err := strconv.ParseUint(p, 16, 8)
		if err != nil {
			return mac, false
		}
		mac[i] = byte(v)
	}
	return mac, true
}
