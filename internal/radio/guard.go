package radio

import (
	"errors"
	"time"
)

// Tx-power guard orchestration. GuardedTxPower (guard_linux.go) wires the real
// netlink socket to this device-free driver; the driver itself touches no
// hardware, so it is unit-tested with a mock Actuator/Store/reader exactly like
// the Transaction machine it builds on. The driver reads the current power,
// applies the target through a guarded Transaction (SET_WIPHY LIMITED), verifies
// the read-back within tolerance, and then either commits the change or reverts
// to the original.

// TxPowerToleranceDBm is the +/- window VERIFY accepts between the requested
// level and the level read back, because the LIMITED setting lets a driver round
// a requested tx power down to the nearest regulatory step rather than honour it
// to the dBm.
const TxPowerToleranceDBm = 1

// GuardWatchdog is the short deadline the guard arms before it actuates, so a
// change that is neither committed nor reverted in-process cannot strand the
// radio. The guard always resolves (commit or revert) synchronously, so this
// timer is a backstop, not the normal path.
const GuardWatchdog = 5 * time.Second

var (
	// ErrTxPowerReadback means the driver exposed no tx-power level to read
	// back, so neither a before value nor a verify is possible.
	ErrTxPowerReadback = errors.New("radio: tx-power readback unavailable on this interface")
	// ErrNoNetdev means no radio in the snapshot is bound to the named interface.
	ErrNoNetdev = errors.New("radio: no radio bound to interface")
	// ErrTxPowerVerify means the level read back was outside tolerance of the
	// requested level; the change was rolled back to the original. The wrapped
	// error carries the device-reported detail.
	ErrTxPowerVerify = errors.New("radio: tx-power verify out of tolerance, rolled back")
	// ErrGuardRollback means a rollback (a deliberate revert, or an automatic one
	// after a failed verify) could not be actuated; the radio may be left changed
	// and the error must surface.
	ErrGuardRollback = errors.New("radio: tx-power rollback failed")
)

// txPowerReader reads the current tx power of an interface in whole dBm. ok is
// false when the driver exposes no level. The Linux implementation
// (*LinuxActuator) issues GET_INTERFACE and falls back to GET_WIPHY; tests
// supply a mock.
type txPowerReader interface {
	ReadTxPowerDBm(iface string) (dBm int, ok bool, err error)
}

// txPowerWithinTolerance reports whether a read-back level is close enough to the
// target to count as applied. Drivers round to regulatory steps, so VERIFY uses
// this window rather than exact equality.
func txPowerWithinTolerance(readDBm, targetDBm, tol int) bool {
	d := readDBm - targetDBm
	if d < 0 {
		d = -d
	}
	return d <= tol
}

// radioByNetdev returns the radio bound to a netdev, or nil.
func (c *Capabilities) radioByNetdev(iface string) *Radio {
	for i := range c.Radios {
		if c.Radios[i].Netdev == iface {
			return &c.Radios[i]
		}
	}
	return nil
}

// runGuardedTxPower is the device-free core of GuardedTxPower. caps is the
// capability snapshot, act/reader/store/clock the effect interfaces, and timeout
// the watchdog deadline. before/after are whole-dBm readings; after is the level
// the driver actually set, read between APPLY and the commit/revert decision so
// it reflects the change even when the net effect is reverted.
func runGuardedTxPower(caps *Capabilities, iface string, targetDBm int, revert bool,
	act Actuator, reader txPowerReader, store Store, clock Clock, timeout time.Duration) (before, after int, err error) {

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

	// 1. Read the current power.
	before, ok, err := reader.ReadTxPowerDBm(iface)
	if err != nil {
		return 0, 0, err
	}
	if !ok {
		return 0, 0, ErrTxPowerReadback
	}
	if before < -128 || before > 127 {
		return before, 0, ErrTxPowerRange
	}
	// Record the real before value so the rollback restores exactly it. KnownRange
	// is left untouched: when the probe never proved a range, CheckTxPower below
	// refuses rather than guess.
	r.CurTxPower = int8(before)
	r.Known |= KnownTxPower

	// 4. Never set a power outside the radio's reported range, or when the range
	// is unknown.
	if targetDBm < -128 || targetDBm > 127 {
		return before, 0, ErrTxPowerRange
	}
	if cerr := caps.CheckTxPower(idx, int8(targetDBm)); cerr != nil {
		return before, 0, cerr
	}

	change := Change{RadioIndex: idx, SetTxPower: true, TxPower: int8(targetDBm)}

	// 2. Guarded transaction: APPLY sets the target, its rollback record restores
	// before, and the watchdog is armed with a short deadline.
	tx := NewTransaction(caps, act, store, clock)
	if perr := tx.Prepare(change, timeout); perr != nil {
		return before, 0, perr
	}
	if aerr := tx.Apply(); aerr != nil {
		// Apply failed: the transaction already attempted its own rollback.
		return before, 0, aerr
	}

	// after = the level the driver actually set, read before any revert.
	if v, vok, rerr := reader.ReadTxPowerDBm(iface); rerr == nil {
		if vok {
			after = v
		} else {
			after = before
		}
	}

	// VERIFY reads the level back and checks tolerance. A failure rolls the
	// transaction back to before automatically.
	if verr := tx.Verify(); verr != nil {
		if tx.State() == StateFailed {
			return before, after, errors.Join(ErrGuardRollback, verr)
		}
		return before, after, errors.Join(ErrTxPowerVerify, verr)
	}

	// 3. Resolve the guard.
	if revert {
		// A probe: do not COMMIT. Force the armed watchdog so the change is
		// reverted to before and the net effect is a no-op. Passing a time past
		// the deadline fires Tick's rollback branch deterministically, with no
		// sleeping and no wall-clock race.
		fired := tx.Tick(clock.Now().Add(timeout + time.Nanosecond))
		if !fired || tx.State() != StateRolledBack {
			return before, after, ErrGuardRollback
		}
		return before, after, nil
	}
	if cerr := tx.Commit(); cerr != nil {
		return before, after, cerr
	}
	return before, after, nil
}
