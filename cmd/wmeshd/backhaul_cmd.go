//go:build linux

package main

import (
	"encoding/hex"
	"errors"
	"flag"
	"fmt"
	"os"
	"strings"
	"time"

	"wantasticmesh/internal/backhaul"
	"wantasticmesh/internal/wmproto"
)

// runBackhaul drives wmeshd's own wireless backhaul STA link in-process:
//
//	backhaul form -radio wifi2 -if ath2 -ssid wmesh-bh [-bssid ..] \
//	              (-net <hex> | -key <hex> | -key-file f) [-4addr] [-freq 6595] [-timeout 20]
//	backhaul status -if ath2
//	backhaul down   -radio wifi2 -if ath2
//
// It uses only standard nl80211 + the wpa_supplicant control socket; it issues
// no QCA vendor command. form creates the vdev, adopts it into the running
// global supplicant, applies the WPA3-SAE network and waits for association,
// then prints the measured peer RSSI/rate read back over nl80211.
func runBackhaul(args []string) error {
	if len(args) == 0 {
		return errors.New("usage: backhaul <form|status|down> ...")
	}
	switch args[0] {
	case "form":
		return backhaulForm(args[1:])
	case "status":
		return backhaulStatus(args[1:])
	case "down":
		return backhaulDown(args[1:])
	default:
		return fmt.Errorf("unknown backhaul action %q (want form|status|down)", args[0])
	}
}

// saePassword resolves the SAE password from the mutually-exclusive sources:
// -net derives it as wmproto.BackhaulPSK(masterKey) (the real mesh flow), while
// -key / -key-file supply it directly (for ad-hoc tests).
func saePassword(netHex, keyHex, keyFile string) (string, error) {
	n := 0
	for _, s := range []string{netHex, keyHex, keyFile} {
		if s != "" {
			n++
		}
	}
	if n != 1 {
		return "", errors.New("exactly one of -net, -key, -key-file is required")
	}
	switch {
	case netHex != "":
		mk, err := hex.DecodeString(netHex)
		if err != nil {
			return "", fmt.Errorf("-net: %w", err)
		}
		return wmproto.BackhaulPSK(mk)
	case keyHex != "":
		return keyHex, nil
	default:
		b, err := os.ReadFile(keyFile)
		if err != nil {
			return "", err
		}
		return strings.TrimSpace(string(b)), nil
	}
}

func backhaulForm(args []string) error {
	fs := flag.NewFlagSet("backhaul form", flag.ContinueOnError)
	radio := fs.String("radio", "", "radio netdev hosting the STA vdev, e.g. wifi2 (for its phy)")
	ifn := fs.String("if", "", "STA vdev name to create, e.g. ath2")
	ssid := fs.String("ssid", "", "backhaul SSID, e.g. wmesh-bh")
	bssid := fs.String("bssid", "", "optional AP BSSID lock")
	netHex := fs.String("net", "", "network master key (hex); SAE password is derived from it")
	keyHex := fs.String("key", "", "SAE password directly (hex string)")
	keyFile := fs.String("key-file", "", "file holding the SAE password")
	fourAddr := fs.Bool("4addr", false, "create the vdev with 4addr (WDS/Multi-AP backhaul)")
	freq := fs.Int("freq", 0, "optional scan frequency in MHz, e.g. 6595")
	timeout := fs.Int("timeout", 20, "association timeout in seconds")
	globalCtrl := fs.String("global-ctrl", "", "global supplicant ctrl socket (default /var/run/wpa_supplicantglobal)")
	ctrlDir := fs.String("ctrl-dir", "", "per-interface ctrl dir (default /var/run/wpa_supplicant)")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if *radio == "" || *ifn == "" || *ssid == "" {
		return errors.New("backhaul form needs -radio, -if and -ssid")
	}
	if os.Geteuid() != 0 {
		return errors.New("backhaul form needs root (NEW_INTERFACE + supplicant control)")
	}
	pw, err := saePassword(*netHex, *keyHex, *keyFile)
	if err != nil {
		return err
	}
	phy, err := backhaul.PhyIndexOf(*radio)
	if err != nil {
		return fmt.Errorf("resolve phy for %s: %w", *radio, err)
	}
	cfg := backhaul.Config{
		Phy: phy, IfName: *ifn, SSID: *ssid, BSSID: *bssid,
		SAEPassword: pw, FourAddr: *fourAddr, ScanFreqMHz: *freq,
		GlobalCtrl: *globalCtrl, CtrlDir: *ctrlDir,
	}
	fmt.Printf("forming backhaul: %s (phy%d) -> SSID %q%s\n", *ifn, phy, *ssid,
		map[bool]string{true: " [4addr]", false: ""}[*fourAddr])
	link, err := backhaul.Form(cfg, time.Duration(*timeout)*time.Second)
	if err != nil {
		return err
	}
	fmt.Printf("associated: %s <-> %s\n", *ifn, link.PeerBSSID())
	printPeer(link)
	return nil
}

func backhaulStatus(args []string) error {
	fs := flag.NewFlagSet("backhaul status", flag.ContinueOnError)
	ifn := fs.String("if", "", "STA vdev name, e.g. ath2")
	globalCtrl := fs.String("global-ctrl", "", "global supplicant ctrl socket")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if *ifn == "" {
		return errors.New("backhaul status needs -if")
	}
	link, err := backhaul.Attach(backhaul.Config{IfName: *ifn, GlobalCtrl: *globalCtrl})
	if err != nil {
		return err
	}
	defer link.Detach()
	assoc, err := link.Associated()
	if err != nil {
		return err
	}
	fmt.Printf("%s: associated=%v peer=%s\n", *ifn, assoc, link.PeerBSSID())
	if assoc {
		printPeer(link)
	}
	return nil
}

func backhaulDown(args []string) error {
	fs := flag.NewFlagSet("backhaul down", flag.ContinueOnError)
	ifn := fs.String("if", "", "STA vdev name to remove, e.g. ath2")
	globalCtrl := fs.String("global-ctrl", "", "global supplicant ctrl socket")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if *ifn == "" {
		return errors.New("backhaul down needs -if")
	}
	if os.Geteuid() != 0 {
		return errors.New("backhaul down needs root")
	}
	link, err := backhaul.Attach(backhaul.Config{IfName: *ifn, GlobalCtrl: *globalCtrl})
	if err != nil {
		return err
	}
	if err := link.Teardown(); err != nil {
		return err
	}
	fmt.Printf("%s torn down\n", *ifn)
	return nil
}

// printPeer prints the measured peer metrics read over nl80211 by MAC.
func printPeer(link *backhaul.Link) {
	st, ok, err := link.PeerSignal()
	switch {
	case err != nil:
		fmt.Printf("  peer telemetry: %v\n", err)
	case !ok:
		fmt.Printf("  peer telemetry: not associated\n")
	default:
		fmt.Printf("  peer: signal=%ddBm rx=%.1fMbit/s tx=%.1fMbit/s\n",
			st.SignalDBm, st.RxMbps(), st.TxMbps())
	}
}
