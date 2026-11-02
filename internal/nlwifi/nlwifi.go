//go:build linux

// Package nlwifi reads measured Wi-Fi telemetry through the device's own
// libnl-genl / nl80211 (cgo): associated-station metrics (RSSI, PHY bitrate,
// inactivity) and per-channel survey (airtime busy/active). It is strictly
// read-only and issues no QCA vendor nl80211 command. These measurements are the
// input that lets the controller build a topology from real link quality instead
// of inferring it from passive 1905 observation.
package nlwifi

/*
#include <stdlib.h>
#include "nlwifi.h"
*/
import "C"

import (
	"errors"
	"os"
	"strconv"
	"strings"
	"unsafe"
)

// Station is one associated station's measured metrics. HaveSignal/HaveRx/HaveTx
// report whether the driver supplied that field (zero is otherwise ambiguous).
type Station struct {
	MAC              [6]byte
	SignalDBm        int8
	RxBitrate100kbps uint32 // RX PHY rate in units of 100 kbps (0 = unknown)
	TxBitrate100kbps uint32 // TX PHY rate in units of 100 kbps (0 = unknown)
	InactiveMs       uint32
	HaveSignal       bool
	HaveRx           bool
	HaveTx           bool
}

// RxMbps returns the RX PHY rate in Mbit/s.
func (s Station) RxMbps() float64 { return float64(s.RxBitrate100kbps) / 10 }

// TxMbps returns the TX PHY rate in Mbit/s.
func (s Station) TxMbps() float64 { return float64(s.TxBitrate100kbps) / 10 }

// Survey is the in-use channel's airtime survey. Utilization is busy/active.
type Survey struct {
	FreqMHz  uint32
	NoiseDBm int8
	ActiveMs uint64
	BusyMs   uint64
}

// Utilization returns channel busy fraction in [0,1], or 0 if unmeasured.
func (s Survey) Utilization() float64 {
	if s.ActiveMs == 0 {
		return 0
	}
	u := float64(s.BusyMs) / float64(s.ActiveMs)
	if u > 1 {
		u = 1
	}
	return u
}

// ErrNL is returned when the nl80211 dump fails. The wrapped code is the C return.
var ErrNL = errors.New("nlwifi: nl80211 dump failed")

const maxStations = 128

// Stations returns the stations associated on the interface at ifindex.
func Stations(ifindex int) ([]Station, error) {
	buf := make([]C.struct_wifi_sta, maxStations)
	n := int(C.wifi_dump_stations(C.int(ifindex), &buf[0], C.int(len(buf))))
	if n < 0 {
		return nil, errCode(n)
	}
	out := make([]Station, n)
	for i := 0; i < n; i++ {
		out[i] = staFromC(buf[i])
	}
	return out, nil
}

// staFromC converts one C wifi_sta into a Station.
func staFromC(c C.struct_wifi_sta) Station {
	var s Station
	for j := 0; j < 6; j++ {
		s.MAC[j] = byte(c.mac[j])
	}
	s.SignalDBm = int8(c.signal_dbm)
	s.RxBitrate100kbps = uint32(c.rx_bitrate)
	s.TxBitrate100kbps = uint32(c.tx_bitrate)
	s.InactiveMs = uint32(c.inactive_ms)
	have := uint32(c.have)
	s.HaveSignal = have&1 != 0
	s.HaveRx = have&2 != 0
	s.HaveTx = have&4 != 0
	return s
}

// GetStation fetches one station's metrics by MAC on ifindex via a doit
// GET_STATION (read-only). ok is false when the station is not associated. This
// is the telemetry path that works on QCA AP vaps, where the GET_STATION dump
// returns nothing but a by-MAC query succeeds; it reads the backhaul peer's RSSI
// and PHY rate from either end of a link.
func GetStation(ifindex int, mac [6]byte) (Station, bool, error) {
	var c C.struct_wifi_sta
	var m [6]C.uint8_t
	for i := 0; i < 6; i++ {
		m[i] = C.uint8_t(mac[i])
	}
	rc := int(C.wifi_get_station(C.int(ifindex), &m[0], &c))
	if rc < 0 {
		return Station{}, false, errCode(rc)
	}
	if rc == 0 {
		return Station{}, false, nil
	}
	return staFromC(c), true, nil
}

// SurveyInUse returns the in-use channel's airtime survey for ifindex. ok is
// false when the driver reports no in-use channel.
func SurveyInUse(ifindex int) (sv Survey, ok bool, err error) {
	var c C.struct_wifi_survey
	rc := int(C.wifi_survey_inuse(C.int(ifindex), &c))
	if rc < 0 {
		return Survey{}, false, errCode(rc)
	}
	if rc == 0 {
		return Survey{}, false, nil
	}
	sv.FreqMHz = uint32(c.freq_mhz)
	sv.NoiseDBm = int8(c.noise_dbm)
	sv.ActiveMs = uint64(c.active_ms)
	sv.BusyMs = uint64(c.busy_ms)
	return sv, true, nil
}

func errCode(n int) error {
	return errors.New(ErrNL.Error() + " (code " + strconv.Itoa(n) + ")")
}

// IfIndex resolves an interface name to its kernel ifindex via sysfs (no net
// package, TinyGo/cgo-neutral), matching the rest of the tree.
func IfIndex(iface string) (int, error) {
	if iface == "" {
		return 0, errors.New("nlwifi: empty interface name")
	}
	b, err := os.ReadFile("/sys/class/net/" + iface + "/ifindex")
	if err != nil {
		return 0, err
	}
	return strconv.Atoi(strings.TrimSpace(string(b)))
}

// keep unsafe imported if future helpers need it (CString paths etc.)
var _ = unsafe.Pointer(nil)
