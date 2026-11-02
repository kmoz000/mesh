package radio

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"time"
)

// Channel-switch guard orchestration. GuardedChannelSwitch (guardchan_linux.go)
// wires the real hostapd_cli actuator and the nl80211 oper-channel reader to this
// device-free driver; the driver itself touches no hardware, so it is unit-tested
// with a mock Actuator/Store/reader exactly like GuardedTxPower (guard.go) and the
// Transaction machine both build on. The driver reads the current operating
// channel and width, applies the target channel through a guarded Transaction
// (hostapd CSA, preserving the current width), verifies the operating channel
// changed, and then either commits the change or reverts to the original channel.
//
// A channel change is a CSA (chan_switch): it keeps associated clients, unlike a
// raw retune. The VERIFY read-back of the operating channel goes through a
// GET_INTERFACE *dump* matched by ifindex, because a doit GET_INTERFACE reply is
// not delivered to the handler on the QCA 5.4 target (see
// operChannelViaInterface in actuator_linux.go, parallel to txPowerViaInterface).

var (
	// ErrChanReadback means the driver exposed no operating channel to read
	// back, so neither a before value nor a verify is possible.
	ErrChanReadback = errors.New("radio: operating-channel readback unavailable on this interface")
	// ErrChanWidthUnknown means the operating width could not be read, so the CSA
	// cannot preserve it and no complete rollback can be built; refuse rather than
	// guess a width.
	ErrChanWidthUnknown = errors.New("radio: operating width unknown, refusing channel switch")
	// ErrChanVerify means the operating channel read back after the switch was not
	// the requested channel; the change was rolled back to the original. The
	// wrapped error carries the device-reported detail.
	ErrChanVerify = errors.New("radio: channel-switch verify failed, rolled back")
	// ErrChanRollback means a rollback (a deliberate revert, or an automatic one
	// after a failed verify) could not be actuated; the radio may be left changed
	// and the error must surface.
	ErrChanRollback = errors.New("radio: channel-switch rollback failed")
	// ErrNoHostapdCtrl means no hostapd control socket was found for the interface,
	// so a CSA cannot be issued.
	ErrNoHostapdCtrl = errors.New("radio: no hostapd control socket found for interface")
)

// operChannelReader reads the current operating channel number and width (MHz) of
// an interface. haveCh is false when the driver exposes no channel. The Linux
// implementation (*LinuxActuator) issues a GET_INTERFACE dump and matches the
// iface's ifindex; tests supply a mock.
type operChannelReader interface {
	ReadOperChannel(iface string) (channel, width int, haveCh bool, err error)
}

// chan24Max is the highest 2.4 GHz channel this layer will switch to. Channel 14
// is Japan-only DSSS and never a CSA target here; 1..13 is the allowed set.
const chan24Max = 13

// hostapdCtrlDir locates the hostapd control-interface directory that serves
// iface. hostapd on the target exposes one UNIX control socket per interface at
// /var/run/hostapd-<radio>/<iface> (e.g. /var/run/hostapd-wifi0/ath0). It walks
// the per-radio directories under <root>/var/run named "hostapd-*" and returns
// the one holding a socket whose name is exactly iface, to pass as
// `hostapd_cli -p <dir>`. root is "/" in production and a temp tree in tests.
// Without the socket a CSA cannot be issued, so the caller refuses. It uses
// os.ReadDir + os.Lstat (the sysfs.go pattern), not filepath.Glob, so it stays
// TinyGo-safe and host-testable.
func hostapdCtrlDir(root, iface string) (string, error) {
	if iface == "" {
		return "", ErrCSANoNetdev
	}
	if root == "" {
		root = "/"
	}
	runDir := filepath.Join(root, "var", "run")
	ents, err := os.ReadDir(runDir)
	if err != nil {
		if os.IsNotExist(err) {
			return "", ErrNoHostapdCtrl
		}
		return "", err
	}
	for _, ent := range ents {
		if !ent.IsDir() || !strings.HasPrefix(ent.Name(), "hostapd-") {
			continue
		}
		dir := filepath.Join(runDir, ent.Name())
		sock := filepath.Join(dir, iface)
		fi, serr := os.Lstat(sock)
		if serr != nil {
			continue
		}
		if fi.Mode()&os.ModeSocket == 0 {
			continue
		}
		return dir, nil
	}
	return "", ErrNoHostapdCtrl
}

// runGuardedChannelSwitch is the device-free core of GuardedChannelSwitch. caps is
// the capability snapshot, act/reader/store/clock the effect interfaces, settle an
// optional delay run once after APPLY so the CSA has elapsed before the read-back
// (nil in tests, a time.Sleep closure on device), and timeout the watchdog
// deadline. before/after are channel numbers; after is the channel the driver
// actually switched to, read between APPLY and the commit/revert decision so it
// reflects the change even when the net effect is reverted.
func runGuardedChannelSwitch(caps *Capabilities, iface string, targetChannel int, revert bool,
	act Actuator, reader operChannelReader, store Store, clock Clock, settle func(), timeout time.Duration) (before, after int, err error) {

	if clock == nil {
		clock = RealClock{}
	}
	if timeout <= 0 {
		// The guard must stay armed: the revert path forces this watchdog, and a
		// zero deadline would leave an applied change neither committed nor
		// reverted.
		timeout = GuardWatchdog
	}
	r := caps.radioByNetdev(iface)
	if r == nil {
		return 0, 0, ErrNoNetdev
	}
	idx := r.Index

	// 1. Read the current operating channel and width.
	before, width, haveCh, err := reader.ReadOperChannel(iface)
	if err != nil {
		return 0, 0, err
	}
	if !haveCh {
		return 0, 0, ErrChanReadback
	}
	if width == 0 {
		// Without the current width the CSA cannot preserve it and buildInverse has
		// no width to roll back to; refuse rather than guess.
		return before, 0, ErrChanWidthUnknown
	}
	if before < 1 || before > 0xffff || width > 0xffff {
		return before, 0, ErrChanReadback
	}
	// Record the real before values so the rollback restores exactly them (mirrors
	// guard.go recording the before tx power). buildInverse reads these from the
	// snapshot, so a stale probe value must not leak into the inverse.
	r.CurChannel = uint16(before)
	r.CurWidth = uint16(width)
	r.Known |= KnownChannel | KnownWidth

	// 4. Refuse a target the radio's capability/band does not allow, with zero
	// actuation. Range/band checks first, then the capability set, then the CSA
	// geometry, so an unplaceable combination is refused here instead of failing at
	// APPLY (which would fire a pointless rollback chan_switch).
	if targetChannel < 1 || targetChannel > 0xffff {
		return before, 0, ErrChannel
	}
	if r.Band == Band24 && targetChannel > chan24Max {
		return before, 0, ErrChannel
	}
	if cerr := caps.SupportsChannel(idx, uint16(targetChannel), uint16(width)); cerr != nil {
		return before, 0, cerr
	}
	if _, perr := planCSA(r, uint16(targetChannel), uint16(width)); perr != nil {
		return before, 0, perr
	}

	change := Change{RadioIndex: idx, SetChannel: true, Channel: uint16(targetChannel), Width: uint16(width)}

	// 2. Guarded transaction: APPLY switches to the target preserving width, its
	// rollback record restores before, and the watchdog is armed with a short
	// deadline.
	tx := NewTransaction(caps, act, store, clock)
	if perr := tx.Prepare(change, timeout); perr != nil {
		return before, 0, perr
	}
	if aerr := tx.Apply(); aerr != nil {
		// Apply failed: the transaction already attempted its own rollback.
		return before, 0, aerr
	}

	// Let the CSA elapse before reading the operating channel back. A 5-beacon
	// switch is not instantaneous, so an immediate read would still report the old
	// channel.
	if settle != nil {
		settle()
	}

	// after = the channel the driver actually switched to, read before any revert.
	if v, _, vok, rerr := reader.ReadOperChannel(iface); rerr == nil {
		if vok {
			after = v
		} else {
			after = before
		}
	}

	// VERIFY reads the operating channel back and checks it is the target. A
	// failure rolls the transaction back to before automatically.
	if verr := tx.Verify(); verr != nil {
		if tx.State() == StateFailed {
			return before, after, errors.Join(ErrChanRollback, verr)
		}
		return before, after, errors.Join(ErrChanVerify, verr)
	}

	// 3. Resolve the guard.
	if revert {
		// A probe: do not COMMIT. Force the armed watchdog so the change is reverted
		// to before and the net effect is a no-op. Passing a time past the deadline
		// fires Tick's rollback branch deterministically, with no sleeping and no
		// wall-clock race.
		fired := tx.Tick(clock.Now().Add(timeout + time.Nanosecond))
		if !fired || tx.State() != StateRolledBack {
			return before, after, ErrChanRollback
		}
		return before, after, nil
	}
	if cerr := tx.Commit(); cerr != nil {
		return before, after, cerr
	}
	return before, after, nil
}
