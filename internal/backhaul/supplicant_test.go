package backhaul

import (
	"strings"
	"testing"
)

func TestInterfaceAddCmd(t *testing.T) {
	got := interfaceAddCmd("ath2", "/var/run/wpa_supplicant")
	want := "INTERFACE_ADD ath2\t\tnl80211\t/var/run/wpa_supplicant"
	if got != want {
		t.Fatalf("interfaceAddCmd:\n got %q\nwant %q", got, want)
	}
	// Exactly four TAB-separated fields after the verb (ifname, empty-config,
	// driver, ctrl-dir) — an empty config field must be preserved, not dropped.
	fields := strings.Split(strings.TrimPrefix(got, "INTERFACE_ADD "), "\t")
	if len(fields) != 4 || fields[1] != "" {
		t.Fatalf("bad field layout %q -> %v", got, fields)
	}
}

func TestNetCommandsFullAndMinimal(t *testing.T) {
	full := netCommands(Config{
		SSID: "wmesh-bh", SAEPassword: "deadbeef",
		BSSID: "02:00:5e:10:00:01", ScanFreqMHz: 6595,
	}, "0")
	joined := strings.Join(full, "\n")
	for _, must := range []string{
		`SET_NETWORK 0 ssid "wmesh-bh"`,
		`SET_NETWORK 0 key_mgmt SAE`,
		`SET_NETWORK 0 sae_password "deadbeef"`,
		`SET_NETWORK 0 ieee80211w 2`,
		`SET_NETWORK 0 bssid 02:00:5e:10:00:01`,
		`SET_NETWORK 0 scan_freq 6595`,
		`SET_NETWORK 0 freq_list 6595`,
		`ENABLE_NETWORK 0`,
	} {
		if !strings.Contains(joined, must) {
			t.Errorf("full sequence missing %q\n%s", must, joined)
		}
	}
	// ENABLE_NETWORK must be last so configuration is complete before the join.
	if full[len(full)-1] != "ENABLE_NETWORK 0" {
		t.Errorf("ENABLE_NETWORK must be last, got %q", full[len(full)-1])
	}

	// Minimal: no bssid, no freq -> those lines absent, core ones present.
	min := strings.Join(netCommands(Config{SSID: "x", SAEPassword: "y"}, "3"), "\n")
	if strings.Contains(min, "bssid") || strings.Contains(min, "scan_freq") {
		t.Errorf("minimal sequence leaked optional params:\n%s", min)
	}
	if !strings.Contains(min, `SET_NETWORK 3 key_mgmt SAE`) {
		t.Errorf("minimal sequence missing core SAE line:\n%s", min)
	}
}

func TestNetIDFromReply(t *testing.T) {
	cases := map[string]string{
		"0\n":             "0",
		"  12 \n":         "12",
		"3":               "3",
		"FAIL\n":          "",
		"":                "",
		"UNKNOWN COMMAND": "",
		"0\nextra":        "0",
	}
	for in, want := range cases {
		if got := netIDFromReply(in); got != want {
			t.Errorf("netIDFromReply(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestStatusParsing(t *testing.T) {
	st := "bssid=02:00:5e:10:00:01\nfreq=6595\nssid=wmesh-bh\nwpa_state=COMPLETED\nkey_mgmt=SAE\n"
	if !statusComplete(st) {
		t.Error("statusComplete should be true for COMPLETED")
	}
	if got := statusField(st, "bssid"); got != "02:00:5e:10:00:01" {
		t.Errorf("bssid field = %q", got)
	}
	if got := statusField(st, "freq"); got != "6595" {
		t.Errorf("freq field = %q", got)
	}
	if statusField(st, "missing") != "" {
		t.Error("absent field should be empty")
	}
	if statusComplete("wpa_state=SCANNING\n") {
		t.Error("SCANNING must not count as complete")
	}
}

func TestParseMAC(t *testing.T) {
	mac, ok := parseMAC("02:00:5e:10:00:01")
	if !ok || mac != [6]byte{0x02, 0x00, 0x5e, 0x10, 0x00, 0x01} {
		t.Fatalf("parseMAC good input failed: %v ok=%v", mac, ok)
	}
	for _, bad := range []string{"", "02:03", "zz:03:7f:12:cd:ce", "02-03-7f-12-cd-ce"} {
		if _, ok := parseMAC(bad); ok {
			t.Errorf("parseMAC(%q) should fail", bad)
		}
	}
}
