//go:build linux

package radio

import "time"

// CSASettle is how long GuardedChannelSwitch waits after issuing the chan_switch
// before it reads the operating channel back. A CSA is announced over a few
// beacons (CSACount, ~5) at a ~100 ms beacon interval, so the switch is not
// instantaneous; this backstop gives the driver time to land on the new channel
// before VERIFY. DEVICE EXECUTION IS PENDING: this value is not yet tuned on
// hardware.
const CSASettle = 2 * time.Second

// GuardedChannelSwitch moves iface to targetChannel via hostapd CSA, verifies
// the operating channel changed, and rolls back to the original channel if
// verify fails or revert is true. before/after are channel numbers.
//
// It probes the radios, opens a netlink socket, discovers the hostapd control
// socket for iface, and hands the real socket/exec-backed Actuator and
// oper-channel reader to runGuardedChannelSwitch (guardchan.go), which holds all
// the orchestration and is unit-tested with mocks. DEVICE EXECUTION IS PENDING:
// the hostapd_cli and socket calls here are compiled but have not run against a
// live device.
func GuardedChannelSwitch(iface string, targetChannel int, revert bool) (before, after int, err error) {
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

	// Bind iface to its radio in the local snapshot. A CSA is issued with
	// `hostapd_cli -i <iface>`, so r.Netdev must be the exact AP vif named by the
	// caller, not whichever sibling vif sysfs enumeration bound first. Resolve the
	// wiphy from the interface and attach iface to that radio unconditionally.
	if r := caps.radioByNetdev(iface); r != nil {
		r.Netdev = iface
	} else if ifindex, e := ifindexOf(iface); e == nil {
		if _, _, wiphy, hasWiphy, e2 := act.txPowerViaInterface(ifindex); e2 == nil && hasWiphy {
			if r := caps.radio(uint8(wiphy)); r != nil {
				r.Netdev = iface
			}
		}
	}

	// Discover the hostapd control-interface directory for iface and point the
	// actuator at it (`hostapd_cli -p <dir>`). Without it a CSA cannot be issued,
	// so refuse with a clear error rather than let hostapd_cli pick a default.
	dir, derr := hostapdCtrlDir("/", iface)
	if derr != nil {
		return 0, 0, derr
	}
	act.CtrlPath = dir

	// A MemStore is enough: the guard resolves (commit or revert) synchronously in
	// this process, so the rollback record never needs to outlive it.
	store := &MemStore{}
	settle := func() { time.Sleep(CSASettle) }
	return runGuardedChannelSwitch(&caps, iface, targetChannel, revert, act, act, store, RealClock{}, settle, GuardWatchdog)
}
