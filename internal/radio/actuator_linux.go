//go:build linux

package radio

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"wantasticmesh/internal/hostapd"
)

// csaEventTimeout bounds the wait for hostapd's AP-CSA-FINISHED after a
// chan_switch. A CSA is announced over CSACount beacons (~0.5 s at a 100 ms
// beacon interval) and the driver then retunes; this allows ample margin.
const csaEventTimeout = 5 * time.Second

// LinuxActuator is the production Actuator. Channel/width changes go through
// `hostapd_cli chan_switch` (CSA, keeps clients associated); tx-power changes go
// through nl80211 NL80211_CMD_SET_WIPHY on the resolved generic family id. It
// never drives a raw nl80211 SET_CHANNEL and never a QCA vendor command.
//
// It is driven only by the transaction machine, which has already validated the
// change against capability (channel in set, width supported, power within
// range). DEVICE EXECUTION IS PENDING: no real hostapd_cli or socket call has
// been made; the hostapd control path (CtrlPath) and the width keyword
// (PhyMode) are unconfirmed on device.
type LinuxActuator struct {
	// Caps is the capability snapshot used to map a radio index to its netdev,
	// band and operating state (for CSA planning and verification).
	Caps *Capabilities
	// CtrlPath is the hostapd control-interface directory holding the per-radio
	// interface sockets (e.g. /var/run/hostapd-wifi0); the socket for an iface is
	// CtrlPath/<iface>. A CSA is refused if it is empty.
	CtrlPath string
	// CSACount is the number of beacons the switch is announced over.
	CSACount int
	// PhyMode overrides the hostapd width keyword (ht/vht/he/eht).
	PhyMode string
	// NL80211ID is the resolved nl80211 generic-netlink family id (never
	// hardcoded; set by NewLinuxActuator).
	NL80211ID uint16

	conn *genlConn
}

// NewLinuxActuator opens a netlink socket and resolves the nl80211 family id by
// name. caps is the snapshot it plans against.
func NewLinuxActuator(caps *Capabilities) (*LinuxActuator, error) {
	c, err := openGenl()
	if err != nil {
		return nil, err
	}
	res := &LinuxFamilyResolver{conn: c}
	fi, err := res.Resolve(genlFamilyNameNL80211)
	if err != nil {
		c.Close()
		return nil, err
	}
	return &LinuxActuator{
		Caps:      caps,
		CSACount:  5,
		NL80211ID: fi.ID,
		conn:      c,
	}, nil
}

// Close releases the socket.
func (a *LinuxActuator) Close() error {
	if a == nil || a.conn == nil {
		return nil
	}
	return a.conn.Close()
}

var errNoCaps = errors.New("radio: actuator has no capability snapshot")

// Apply actuates the change.
func (a *LinuxActuator) Apply(c Change) error {
	mech, err := c.Mechanism()
	if err != nil {
		return err
	}
	switch mech {
	case MechHostapdCSA:
		return a.applyCSA(c)
	case MechNL80211Power:
		return a.applyTxPower(c)
	default:
		return ErrState
	}
}

func (a *LinuxActuator) radio(idx uint8) (*Radio, error) {
	if a.Caps == nil {
		return nil, errNoCaps
	}
	r := a.Caps.radio(idx)
	if r == nil {
		return nil, ErrNoRadio
	}
	return r, nil
}

func (a *LinuxActuator) applyCSA(c Change) error {
	r, err := a.radio(c.RadioIndex)
	if err != nil {
		return err
	}
	iface, csArgs, err := hostapdChanSwitchArgs(r, c, a.csaCount(), a.PhyMode)
	if err != nil {
		return err
	}
	if a.CtrlPath == "" {
		return ErrNoHostapdCtrl
	}
	// Talk to hostapd's per-interface control socket directly (the device has no
	// hostapd_cli-equivalent library; see internal/hostapd), issue the CSA, and
	// block on the AP-CSA-FINISHED event rather than a blind sleep. csArgs[0] is
	// the "chan_switch" verb, which the control-interface command spells
	// CHAN_SWITCH, so only the tail is passed.
	sock := filepath.Join(a.CtrlPath, iface)
	conn, err := hostapd.Open(sock)
	if err != nil {
		return errors.New("radio: open hostapd control socket " + sock + ": " + err.Error())
	}
	defer conn.Close()
	return conn.ChanSwitch(csArgs[1:], csaEventTimeout)
}

func (a *LinuxActuator) applyTxPower(c Change) error {
	r, err := a.radio(c.RadioIndex)
	if err != nil {
		return err
	}
	if r.Netdev == "" {
		return fmt.Errorf("radio %d has no netdev to set tx power on", c.RadioIndex)
	}
	ifindex, err := ifindexOf(r.Netdev)
	if err != nil {
		return err
	}
	scratch := make([]byte, 256)
	return a.conn.execute(false,
		func(seq uint32) ([]byte, error) {
			return buildSetWiphyTxPower(scratch, a.NL80211ID, ifindex, c.TxPower, seq, a.conn.pid)
		},
		func(_ uint8, _ []byte) error { return nil },
	)
}

// Verify re-reads device state and confirms the change took effect. For a CSA it
// compares the operating channel/width. For tx power it reads the level back
// (GET_INTERFACE NL80211_ATTR_WIPHY_TX_POWER_LEVEL, confirmed on device: `iw dev
// <iface> info` surfaces the same value as "txpower NN.NN dBm") and accepts it
// when it is within TxPowerToleranceDBm of the request, because the LIMITED
// setting lets the driver round down to a regulatory step.
func (a *LinuxActuator) Verify(c Change) error {
	mech, err := c.Mechanism()
	if err != nil {
		return err
	}
	if mech == MechNL80211Power {
		return a.verifyTxPower(c)
	}
	if mech != MechHostapdCSA {
		return nil
	}
	r, err := a.radio(c.RadioIndex)
	if err != nil {
		return err
	}
	ifindex, err := ifindexOf(r.Netdev)
	if err != nil {
		return err
	}
	curCh, curWidth, haveC, haveW, err := a.operChannelViaInterface(ifindex)
	if err != nil {
		return err
	}
	// Fail closed: a CSA verify that cannot read the operating channel back is a
	// failure, not a pass. (The earlier doit GET_INTERFACE never delivered its
	// reply on the QCA 5.4 target, so haveC stayed false and VERIFY vacuously
	// passed; operChannelViaInterface uses the dump, which is reliable.)
	if !haveC {
		return errors.New("radio: CSA verify: could not read operating channel back")
	}
	if curCh != c.Channel {
		return errors.New("radio: CSA verify: channel is " + strconv.Itoa(int(curCh)) + ", wanted " + strconv.Itoa(int(c.Channel)))
	}
	if haveW && c.Width != 0 && curWidth != c.Width {
		return errors.New("radio: CSA verify: width is " + strconv.Itoa(int(curWidth)) + ", wanted " + strconv.Itoa(int(c.Width)))
	}
	return nil
}

// ReadOperChannel reads iface's current operating channel number and width (MHz)
// via a GET_INTERFACE dump matched by ifindex. It implements operChannelReader so
// GuardedChannelSwitch drives it with no device coupling in the orchestration.
// haveCh is false when the driver exposes no operating channel.
func (a *LinuxActuator) ReadOperChannel(iface string) (channel, width int, haveCh bool, err error) {
	ifindex, err := ifindexOf(iface)
	if err != nil {
		return 0, 0, false, err
	}
	ch, wdt, hc, _, err := a.operChannelViaInterface(ifindex)
	if err != nil {
		return 0, 0, false, err
	}
	return int(ch), int(wdt), hc, nil
}

// operChannelViaInterface issues a GET_INTERFACE dump and returns the operating
// channel number and width (MHz) from the message whose NL80211_ATTR_IFINDEX
// matches ifindex, each with a found flag. It mirrors txPowerViaInterface: on the
// QCA 5.4 target a doit (filtered) GET_INTERFACE reply is not delivered to the
// handler, while the dump (one message per interface, same per-interface attrs)
// is reliable, so we dump and pick our ifindex.
func (a *LinuxActuator) operChannelViaInterface(ifindex uint32) (ch, width uint16, chOK, widthOK bool, err error) {
	scratch := make([]byte, 256)
	err = a.conn.execute(true,
		func(seq uint32) ([]byte, error) {
			w := newBuilder(scratch)
			w.putHeader(a.NL80211ID, nlmFRequest|nlmFDump, seq, a.conn.pid, nl80211CmdGetInterface, 0)
			return w.finalize()
		},
		func(_ uint8, attrs []byte) error {
			if idx, ok := topU32(attrs, nl80211AttrIfIndex); !ok || idx != ifindex {
				return nil
			}
			w, hw, c, hc, perr := parseOperChannel(attrs)
			if perr != nil {
				return perr
			}
			if hc {
				ch, chOK = c, true
			}
			if hw {
				width, widthOK = w, true
			}
			return nil
		},
	)
	return ch, width, chOK, widthOK, err
}

// verifyTxPower reads the tx power back and checks it against the requested
// level within tolerance. It is the real VERIFY for a tx-power change (the
// previous pass left it a no-op while the readback shape was unconfirmed).
func (a *LinuxActuator) verifyTxPower(c Change) error {
	r, err := a.radio(c.RadioIndex)
	if err != nil {
		return err
	}
	readDBm, ok, err := a.ReadTxPowerDBm(r.Netdev)
	if err != nil {
		return err
	}
	if !ok {
		return errors.New("radio: tx-power verify: driver reported no level")
	}
	if !txPowerWithinTolerance(readDBm, int(c.TxPower), TxPowerToleranceDBm) {
		return errors.New("radio: tx-power verify: read " + strconv.Itoa(readDBm) +
			" dBm, wanted " + strconv.Itoa(int(c.TxPower)) + " +/-" + strconv.Itoa(TxPowerToleranceDBm) + " dBm")
	}
	return nil
}

// ReadTxPowerDBm reads iface's current tx power in whole dBm. It issues
// GET_INTERFACE and reads NL80211_ATTR_WIPHY_TX_POWER_LEVEL (u32 mBm); if the
// driver omits that attribute there, it falls back to GET_WIPHY for the iface's
// wiphy (the wiphy index comes from the same GET_INTERFACE reply when present,
// else from the capability snapshot). ok is false when neither exposes a level.
// It implements txPowerReader, so GuardedTxPower drives it with no device coupling
// in the orchestration.
func (a *LinuxActuator) ReadTxPowerDBm(iface string) (int, bool, error) {
	ifindex, err := ifindexOf(iface)
	if err != nil {
		return 0, false, err
	}
	dBm, ok, wiphy, haveWiphy, err := a.txPowerViaInterface(ifindex)
	if err != nil {
		return 0, false, err
	}
	if ok {
		return dBm, true, nil
	}
	if !haveWiphy && a.Caps != nil {
		if r := a.Caps.radioByNetdev(iface); r != nil {
			wiphy, haveWiphy = uint32(r.Index), true
		}
	}
	if !haveWiphy {
		return 0, false, nil
	}
	return a.txPowerViaWiphy(wiphy)
}

// txPowerViaInterface issues GET_INTERFACE for ifindex and returns the tx-power
// level (dBm) and the wiphy index from the same reply, each with a found flag.
func (a *LinuxActuator) txPowerViaInterface(ifindex uint32) (dBm int, levelOK bool, wiphy uint32, wiphyOK bool, err error) {
	scratch := make([]byte, 256)
	// Use a GET_INTERFACE dump and match our ifindex, rather than a doit request
	// filtered by ifindex: on the QCA 5.4 target the doit reply to a filtered
	// GET_INTERFACE is not delivered to the handler, while the dump (the same
	// path GET_WIPHY uses) is reliable. The dump carries one message per
	// interface, each with the same per-interface attrs including the tx-power
	// level, so we pick the message whose NL80211_ATTR_IFINDEX matches.
	err = a.conn.execute(true,
		func(seq uint32) ([]byte, error) {
			w := newBuilder(scratch)
			w.putHeader(a.NL80211ID, nlmFRequest|nlmFDump, seq, a.conn.pid, nl80211CmdGetInterface, 0)
			return w.finalize()
		},
		func(_ uint8, attrs []byte) error {
			if idx, ok := topU32(attrs, nl80211AttrIfIndex); !ok || idx != ifindex {
				return nil
			}
			if v, got := parseTxPowerLevelDBm(attrs); got {
				dBm, levelOK = v, true
			}
			if v, got := topU32(attrs, nl80211AttrWiphy); got {
				wiphy, wiphyOK = v, true
			}
			return nil
		},
	)
	return dBm, levelOK, wiphy, wiphyOK, err
}

// txPowerViaWiphy issues GET_WIPHY filtered to a single wiphy and returns the
// tx-power level in dBm, with a found flag. It uses the split dump (like
// fillWiphy), not a plain doit request: an unsplit GET_WIPHY for a multi-band
// radio overflows the kernel's 4 KiB reply skb and returns -ENOBUFS. The dump
// honours NL80211_ATTR_WIPHY as a filter, and the level is captured from
// whichever split message carries it.
func (a *LinuxActuator) txPowerViaWiphy(wiphy uint32) (int, bool, error) {
	var dBm int
	var ok bool
	scratch := make([]byte, 256)
	err := a.conn.execute(true,
		func(seq uint32) ([]byte, error) {
			w := newBuilder(scratch)
			w.putHeader(a.NL80211ID, nlmFRequest|nlmFDump, seq, a.conn.pid, nl80211CmdGetWiphy, 0)
			w.putU32(nl80211AttrWiphy, wiphy)
			w.putFlag(nl80211AttrSplitWiphyDump)
			return w.finalize()
		},
		func(_ uint8, attrs []byte) error {
			if ok {
				return nil
			}
			if v, got := parseTxPowerLevelDBm(attrs); got {
				dBm, ok = v, true
			}
			return nil
		},
	)
	return dBm, ok, err
}

func (a *LinuxActuator) csaCount() int {
	if a.CSACount <= 0 {
		return 5
	}
	return a.CSACount
}

// ifindexOf reads /sys/class/net/<dev>/ifindex (sysfs, no net package, so it is
// TinyGo-safe and matches sysfs.go's approach).
func ifindexOf(dev string) (uint32, error) {
	if dev == "" {
		return 0, ErrCSANoNetdev
	}
	b, err := os.ReadFile(filepath.Join("/sys/class/net", dev, "ifindex"))
	if err != nil {
		return 0, err
	}
	n, err := strconv.ParseUint(strings.TrimSpace(string(b)), 10, 32)
	if err != nil {
		return 0, err
	}
	return uint32(n), nil
}
