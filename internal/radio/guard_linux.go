//go:build linux

package radio

// GuardedTxPower runs a reversible tx-power change on iface: it reads the
// current power, applies targetDBm via nl80211 SET_WIPHY, verifies by reading
// the power back, and rolls back to the original if verify fails or if revert is
// true. before/after are the readings in whole dBm.
//
// It probes the radios, opens a netlink socket, and hands the real socket-backed
// Actuator/reader to runGuardedTxPower (guard.go), which holds all the
// orchestration and is unit-tested with mocks. DEVICE EXECUTION IS PENDING: the
// socket calls here are compiled but have not run against a live kernel.
func GuardedTxPower(iface string, targetDBm int, revert bool) (before, after int, err error) {
	prober, err := NewLinuxProber("/")
	if err != nil {
		return 0, 0, err
	}
	caps, perr := prober.Probe()
	prober.Close()
	if perr != nil {
		return 0, 0, perr
	}

	act, err := NewLinuxActuator(&caps)
	if err != nil {
		return 0, 0, err
	}
	defer act.Close()

	// Resolve the iface's wiphy. sysfs enumeration binds only the first netdev of
	// each phy to a Radio, so a caller naming another vif on the same phy (e.g. a
	// backhaul STA vif alongside the AP) would miss radioByNetdev. In that case
	// read the wiphy index straight from the interface and attach iface to that
	// radio in the local snapshot, so the readback and the SET_WIPHY both land on
	// the right radio.
	if caps.radioByNetdev(iface) == nil {
		if ifindex, e := ifindexOf(iface); e == nil {
			if _, _, wiphy, hasWiphy, e2 := act.txPowerViaInterface(ifindex); e2 == nil && hasWiphy {
				if r := caps.radio(uint8(wiphy)); r != nil {
					r.Netdev = iface
				}
			}
		}
	}

	// A MemStore is enough: the guard resolves (commit or revert) synchronously
	// in this process, so the rollback record never needs to outlive it.
	store := &MemStore{}
	return runGuardedTxPower(&caps, iface, targetDBm, revert, act, act, store, RealClock{}, GuardWatchdog)
}
