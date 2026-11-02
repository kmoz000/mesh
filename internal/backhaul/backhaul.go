//go:build linux

// Package backhaul forms wmeshd's own wireless backhaul STA link in-process,
// using only standard Linux wireless interfaces — no QCA vendor command and no
// shelling out. It is the compiled form of the path proven on the target
// (research/BACKHAUL-FORMATION.md): create a managed (station) vdev via
// NL80211_CMD_NEW_INTERFACE, hand it to the already-running global
// wpa_supplicant with INTERFACE_ADD, configure a WPA3-SAE network over the
// control socket, and confirm association. The SAE password is the
// network-derived backhaul key (wmproto.BackhaulPSK); both ends compute it, so
// there is no provisioning/DPP/WPS.
//
// Telemetry for the formed link is read back through internal/nlwifi's by-MAC
// GET_STATION (the QCA AP-vap dump is empty; the doit-by-MAC query works).
package backhaul

/*
#cgo CFLAGS: -I${SRCDIR}/../nlwifi
#include <stdlib.h>
#include "vdev.h"
*/
import "C"

import (
	"errors"
	"fmt"
	"os"
	"strconv"
	"strings"
	"time"
	"unsafe"

	"wantasticmesh/internal/hostapd"
	"wantasticmesh/internal/nlwifi"
)

// Link is a formed (or forming) backhaul STA link. It keeps the global-socket
// connection and the interface name so the link can be queried and torn down.
type Link struct {
	cfg     Config
	ifindex int
	netID   string
	ifpfx   string // "IFNAME=<if> " — routes a command to the iface over the global socket
	g       *hostapd.Conn
}

// Form creates the vdev, adopts it into the running global supplicant, applies
// the SAE network and waits up to timeout for association. On any failure it
// rolls back what it created (network, interface adoption, vdev).
func Form(cfg Config, timeout time.Duration) (*Link, error) {
	cfg = cfg.withDefaults()
	if cfg.IfName == "" || cfg.SSID == "" || cfg.SAEPassword == "" {
		return nil, errors.New("backhaul: IfName, SSID and SAEPassword are required")
	}
	if timeout <= 0 {
		timeout = 15 * time.Second
	}

	// 1. Create the managed vdev and bring it up.
	if err := createVdev(cfg.Phy, cfg.IfName, cfg.FourAddr); err != nil {
		return nil, err
	}
	l := &Link{cfg: cfg, ifpfx: "IFNAME=" + cfg.IfName + " "}
	ok := false
	defer func() {
		if !ok {
			l.Teardown()
		}
	}()

	ifindex, err := nlwifi.IfIndex(cfg.IfName)
	if err != nil {
		return nil, fmt.Errorf("backhaul: resolve ifindex %s: %w", cfg.IfName, err)
	}
	l.ifindex = ifindex

	// 2. Adopt the vdev into the running global supplicant.
	g, err := hostapd.Open(cfg.GlobalCtrl)
	if err != nil {
		return nil, fmt.Errorf("backhaul: open global supplicant %s: %w", cfg.GlobalCtrl, err)
	}
	l.g = g
	if err := g.RequestOK(interfaceAddCmd(cfg.IfName, cfg.CtrlDir), 3*time.Second); err != nil {
		// A pre-existing adoption answers with an error we tolerate; anything
		// else is fatal. Probe with a STATUS to decide.
		if _, serr := g.Request(l.ifpfx+"STATUS", 2*time.Second); serr != nil {
			return nil, fmt.Errorf("backhaul: INTERFACE_ADD %s: %w", cfg.IfName, err)
		}
	}

	// 3. Configure the SAE network over the interface (via the global socket).
	_ = g.RequestOK(l.ifpfx+"SET sae_pwe 2", 2*time.Second) // 6 GHz needs H2E; best effort
	reply, err := g.Request(l.ifpfx+"ADD_NETWORK", 3*time.Second)
	if err != nil {
		return nil, fmt.Errorf("backhaul: ADD_NETWORK: %w", err)
	}
	netID := netIDFromReply(reply)
	if netID == "" {
		return nil, fmt.Errorf("backhaul: ADD_NETWORK gave no id: %q", strings.TrimSpace(reply))
	}
	l.netID = netID
	for _, cmd := range netCommands(cfg, netID) {
		if err := g.RequestOK(l.ifpfx+cmd, 2*time.Second); err != nil {
			return nil, fmt.Errorf("backhaul: %q: %w", cmd, err)
		}
	}

	// 4. Wait for association.
	if err := l.waitAssociated(timeout); err != nil {
		return nil, err
	}
	ok = true
	return l, nil
}

// waitAssociated polls STATUS until wpa_state=COMPLETED or the deadline.
func (l *Link) waitAssociated(timeout time.Duration) error {
	deadline := time.Now().Add(timeout)
	for {
		st, err := l.g.Request(l.ifpfx+"STATUS", 2*time.Second)
		if err == nil && statusComplete(st) {
			return nil
		}
		if time.Now().After(deadline) {
			return fmt.Errorf("backhaul: %s did not associate within %s", l.cfg.IfName, timeout)
		}
		time.Sleep(500 * time.Millisecond)
	}
}

// Associated reports whether the link is currently associated (COMPLETED).
func (l *Link) Associated() (bool, error) {
	st, err := l.g.Request(l.ifpfx+"STATUS", 2*time.Second)
	if err != nil {
		return false, err
	}
	return statusComplete(st), nil
}

// PeerBSSID returns the currently-associated AP BSSID, from STATUS, or "".
func (l *Link) PeerBSSID() string {
	st, err := l.g.Request(l.ifpfx+"STATUS", 2*time.Second)
	if err != nil {
		return ""
	}
	return statusField(st, "bssid")
}

// PeerSignal reads the backhaul AP's measured metrics from the STA side via
// nl80211 GET_STATION by MAC. ok is false when the peer is not (yet) associated.
func (l *Link) PeerSignal() (nlwifi.Station, bool, error) {
	bssid := l.cfg.BSSID
	if bssid == "" {
		bssid = l.PeerBSSID()
	}
	mac, mok := parseMAC(bssid)
	if !mok {
		return nlwifi.Station{}, false, errors.New("backhaul: no valid peer BSSID to query")
	}
	return nlwifi.GetStation(l.ifindex, mac)
}

// IfName returns the interface name of the link.
func (l *Link) IfName() string { return l.cfg.IfName }

// Attach connects to an already-formed link's running supplicant for status or
// teardown, without creating or reconfiguring anything; the interface must exist.
func Attach(cfg Config) (*Link, error) {
	cfg = cfg.withDefaults()
	if cfg.IfName == "" {
		return nil, errors.New("backhaul: IfName is required")
	}
	g, err := hostapd.Open(cfg.GlobalCtrl)
	if err != nil {
		return nil, fmt.Errorf("backhaul: open global supplicant %s: %w", cfg.GlobalCtrl, err)
	}
	l := &Link{cfg: cfg, ifpfx: "IFNAME=" + cfg.IfName + " ", g: g}
	if ix, ierr := nlwifi.IfIndex(cfg.IfName); ierr == nil {
		l.ifindex = ix
	}
	return l, nil
}

// Detach closes the control connection without changing the link.
func (l *Link) Detach() {
	if l.g != nil {
		l.g.Close()
		l.g = nil
	}
}

// Teardown removes the network, de-adopts the interface from the global
// supplicant and deletes the vdev. It is safe to call on a partially-formed
// link (each step is best-effort) and closes the control connection.
func (l *Link) Teardown() error {
	var first error
	note := func(e error) {
		if e != nil && first == nil {
			first = e
		}
	}
	if l.g != nil {
		// Remove any configured networks on the interface, then de-adopt it from
		// the global supplicant. "all" covers both a link we formed (known netID)
		// and one we merely Attach-ed to. Results are best-effort: the interface
		// or networks may already be gone.
		_, _ = l.g.Request(l.ifpfx+"REMOVE_NETWORK all", 2*time.Second)
		_, _ = l.g.Request("INTERFACE_REMOVE "+l.cfg.IfName, 2*time.Second)
		l.g.Close()
		l.g = nil
	}
	if err := delVdevByName(l.cfg.IfName); err != nil {
		note(err)
	}
	return first
}

// --- cgo wrappers ---

func createVdev(phy int, ifname string, fourAddr bool) error {
	cif := C.CString(ifname)
	defer C.free(unsafe.Pointer(cif))
	fa := C.int(0)
	if fourAddr {
		fa = 1
	}
	if rc := int(C.bh_new_sta_vdev(C.int(phy), cif, fa)); rc != 0 {
		return fmt.Errorf("backhaul: create vdev %q on phy%d failed (nl ack %d)", ifname, phy, rc)
	}
	if rc := int(C.bh_if_up(cif)); rc != 0 {
		return fmt.Errorf("backhaul: bring %q up failed (%d)", ifname, rc)
	}
	return nil
}

func delVdevByName(ifname string) error {
	ifindex, err := nlwifi.IfIndex(ifname)
	if err != nil {
		return nil // already gone
	}
	if rc := int(C.bh_del_vdev(C.int(ifindex))); rc != 0 {
		return fmt.Errorf("backhaul: delete vdev %q (ifindex %d) failed (nl ack %d)", ifname, ifindex, rc)
	}
	return nil
}

// PhyIndexOf resolves a radio netdev name (e.g. "wifi2") to its wiphy index via
// sysfs, the value NL80211_ATTR_WIPHY wants for interface creation.
func PhyIndexOf(radioNetdev string) (int, error) {
	b, err := os.ReadFile("/sys/class/net/" + radioNetdev + "/phy80211/index")
	if err != nil {
		return 0, err
	}
	return strconv.Atoi(strings.TrimSpace(string(b)))
}
