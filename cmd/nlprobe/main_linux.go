//go:build linux

// nlprobe is a read-only diagnostic for the nl80211 telemetry binding
// (internal/nlwifi): per-interface channel survey (airtime) and associated
// station metrics (RSSI, PHY bitrate). It changes nothing on the radio.
// Usage: nlprobe <iface> [iface...]
package main

import (
	"fmt"
	"os"

	"wantasticmesh/internal/nlwifi"
)

func main() {
	if len(os.Args) < 2 {
		fmt.Fprintln(os.Stderr, "usage: nlprobe <iface> [iface...]")
		os.Exit(2)
	}
	for _, iface := range os.Args[1:] {
		idx, err := nlwifi.IfIndex(iface)
		if err != nil {
			fmt.Printf("=== %s: ifindex error: %v\n", iface, err)
			continue
		}
		fmt.Printf("=== %s (ifindex %d) ===\n", iface, idx)
		if sv, ok, err := nlwifi.SurveyInUse(idx); err != nil {
			fmt.Printf("  survey: error: %v\n", err)
		} else if ok {
			fmt.Printf("  survey: freq=%dMHz noise=%ddBm util=%.1f%% (busy=%d active=%d ms)\n",
				sv.FreqMHz, sv.NoiseDBm, sv.Utilization()*100, sv.BusyMs, sv.ActiveMs)
		} else {
			fmt.Println("  survey: no in-use channel reported")
		}
		stas, err := nlwifi.Stations(idx)
		if err != nil {
			fmt.Printf("  stations: error: %v\n", err)
			continue
		}
		fmt.Printf("  stations: %d\n", len(stas))
		for _, s := range stas {
			fmt.Printf("    %02x:%02x:%02x:%02x:%02x:%02x signal=%ddBm rx=%.1f tx=%.1f Mbps inactive=%dms\n",
				s.MAC[0], s.MAC[1], s.MAC[2], s.MAC[3], s.MAC[4], s.MAC[5],
				s.SignalDBm, s.RxMbps(), s.TxMbps(), s.InactiveMs)
		}
	}
	fmt.Println("NLPROBE-OK")
}
