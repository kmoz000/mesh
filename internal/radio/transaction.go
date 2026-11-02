package radio

import (
	"errors"
	"time"
)

// Transaction is the PREPARE -> READY -> APPLY -> VERIFY -> COMMIT state machine
// that guards a single radio reconfiguration. Nothing is actuated until a
// rollback record is persisted, and a poll-driven watchdog (Tick) reverts the
// change if COMMIT does not arrive before the deadline. Retuning the only
// backhaul radio is refused unless a watchdog deadline and a writable store are
// present and a complete inverse change can be built, so a failed change can
// never strand a node.
//
// The machine is pure: all side effects go through the Actuator, Store and
// Clock interfaces, so it is unit-tested with mocks and needs no device.

// State is the transaction state.
type State uint8

const (
	StateIdle       State = iota
	StatePrepared         // rollback persisted, nothing actuated yet
	StateApplied          // change actuated, not yet verified
	StateVerified         // change observed in effect, awaiting COMMIT
	StateCommitted        // committed; rollback cleared
	StateRolledBack       // reverted (verify failed, or watchdog fired)
	StateFailed           // apply failed and could not be reverted
)

func (s State) String() string {
	switch s {
	case StatePrepared:
		return "prepared"
	case StateApplied:
		return "applied"
	case StateVerified:
		return "verified"
	case StateCommitted:
		return "committed"
	case StateRolledBack:
		return "rolled-back"
	case StateFailed:
		return "failed"
	default:
		return "idle"
	}
}

// Actuator performs and observes the device-level operations. The real
// implementation (actuator_linux.go) drives hostapd_cli CSA and nl80211
// SET_WIPHY; tests supply a mock. Apply and Verify both take a Change so the
// same Actuator drives a change and its inverse, and a test can assert the
// mechanism each one routed to.
type Actuator interface {
	// Apply actuates the change. It must route a channel change through CSA and
	// a tx-power change through nl80211, never a raw SET_CHANNEL.
	Apply(c Change) error
	// Verify reports nil if the change is observed in effect on the device.
	Verify(c Change) error
}

// transaction state errors.
var (
	ErrState = errors.New("radio: operation not valid in current transaction state")
)

// Transaction holds one in-flight reconfiguration.
type Transaction struct {
	caps  *Capabilities
	act   Actuator
	store Store
	clock Clock

	state  State
	change Change
	mech   Mechanism
	rec    RollbackRecord
	sole   bool

	hasDeadline bool
	deadline    time.Time
}

// NewTransaction builds a transaction bound to a capability snapshot and the
// three effect interfaces. store may be nil for a change that is not durable
// (never a sole-backhaul retune; Prepare enforces that).
func NewTransaction(caps *Capabilities, act Actuator, store Store, clock Clock) *Transaction {
	if clock == nil {
		clock = RealClock{}
	}
	return &Transaction{caps: caps, act: act, store: store, clock: clock}
}

// State returns the current state.
func (t *Transaction) State() State { return t.state }

// buildInverse computes the change that undoes c from the radio's last-known
// state. It refuses (ErrNoRollback) when the state needed to revert is unknown,
// because reverting to a guessed value is worse than refusing the change.
func buildInverse(caps *Capabilities, c Change, mech Mechanism) (RollbackRecord, error) {
	r := caps.radio(c.RadioIndex)
	if r == nil {
		return RollbackRecord{}, ErrNoRadio
	}
	rec := RollbackRecord{RadioIndex: c.RadioIndex, Mech: mech}
	switch mech {
	case MechHostapdCSA:
		if r.Known&KnownChannel == 0 || r.Known&KnownWidth == 0 {
			return RollbackRecord{}, ErrNoRollback
		}
		if r.Netdev == "" {
			// CSA needs an interface for hostapd_cli -i; without it the inverse
			// cannot be actuated, so there is no usable rollback.
			return RollbackRecord{}, ErrNoRollback
		}
		rec.Netdev = r.Netdev
		rec.Inverse = Change{
			RadioIndex: c.RadioIndex,
			SetChannel: true,
			Channel:    r.CurChannel,
			Width:      r.CurWidth,
		}
	case MechNL80211Power:
		if r.Known&KnownTxPower == 0 || r.Known&KnownRange == 0 {
			return RollbackRecord{}, ErrNoRollback
		}
		rec.Netdev = r.Netdev
		rec.Inverse = Change{
			RadioIndex: c.RadioIndex,
			SetTxPower: true,
			TxPower:    r.CurTxPower,
		}
	default:
		return RollbackRecord{}, ErrNoRollback
	}
	return rec, nil
}

// Prepare validates the change, builds and persists a rollback record, and arms
// the watchdog. timeout <= 0 means no watchdog (allowed only for a non-sole
// radio). It performs no actuation. A sole-backhaul retune requires timeout > 0
// and a writable store, and a successful Save; otherwise it refuses.
func (t *Transaction) Prepare(c Change, timeout time.Duration) error {
	if t.state != StateIdle {
		return ErrState
	}
	mech, err := Validate(t.caps, c)
	if err != nil {
		return err
	}
	rec, err := buildInverse(t.caps, c, mech)
	if err != nil {
		return err
	}
	sole := t.caps.soleBackhaul(c.RadioIndex)
	if sole {
		if timeout <= 0 || t.store == nil {
			return ErrNeedWatchdog
		}
	}
	if t.store != nil {
		if err := t.store.Save(rec.marshal()); err != nil {
			// A sole-backhaul retune must not proceed without a durable
			// rollback; refuse rather than force.
			return err
		}
	}
	t.change = c
	t.mech = mech
	t.rec = rec
	t.sole = sole
	t.hasDeadline = timeout > 0
	if t.hasDeadline {
		t.deadline = t.clock.Now().Add(timeout)
	}
	t.state = StatePrepared
	return nil
}

// Apply actuates the prepared change. On failure it attempts the rollback
// immediately; if the rollback also fails the transaction is left Failed and
// the stored record is kept so a later Recover can retry.
func (t *Transaction) Apply() error {
	if t.state != StatePrepared {
		return ErrState
	}
	if err := t.act.Apply(t.change); err != nil {
		if rbErr := t.act.Apply(t.rec.Inverse); rbErr != nil {
			t.state = StateFailed
			return err
		}
		t.clearStore()
		t.state = StateRolledBack
		return err
	}
	t.state = StateApplied
	return nil
}

// Verify checks the change took effect. On a verify failure it reverts and the
// transaction ends RolledBack. On success it moves to Verified, still awaiting
// COMMIT (the watchdog keeps protecting until Commit).
func (t *Transaction) Verify() error {
	if t.state != StateApplied {
		return ErrState
	}
	if err := t.act.Verify(t.change); err != nil {
		if rbErr := t.act.Apply(t.rec.Inverse); rbErr != nil {
			t.state = StateFailed
			return err
		}
		t.clearStore()
		t.state = StateRolledBack
		return err
	}
	t.state = StateVerified
	return nil
}

// Commit finalizes the change: the watchdog is disarmed and the rollback record
// is cleared. It is valid from Verified (the normal path) or Applied (a caller
// that trusts Apply without a separate verify step).
func (t *Transaction) Commit() error {
	if t.state != StateVerified && t.state != StateApplied {
		return ErrState
	}
	t.clearStore()
	t.hasDeadline = false
	t.state = StateCommitted
	return nil
}

// Tick drives the watchdog. If the transaction is still open (actuated but not
// committed) and the deadline has passed, it reverts the change and moves to
// RolledBack. It returns true if it fired. The daemon event loop calls Tick on
// its cadence; there is no goroutine or timer inside the library.
func (t *Transaction) Tick(now time.Time) bool {
	if !t.hasDeadline {
		return false
	}
	switch t.state {
	case StateApplied, StateVerified:
		// open and actuated
	default:
		return false
	}
	if now.Before(t.deadline) {
		return false
	}
	if err := t.act.Apply(t.rec.Inverse); err != nil {
		t.state = StateFailed
		return true
	}
	t.clearStore()
	t.state = StateRolledBack
	return true
}

func (t *Transaction) clearStore() {
	if t.store != nil {
		_ = t.store.Clear()
	}
}

// Recover reverts a rollback record left by a crashed or abandoned transaction.
// A daemon calls it at startup: if the store holds a record, the inverse change
// is re-applied (so the radio returns to its pre-change state) and the record
// cleared. It returns whether a revert was performed. A malformed record is
// cleared and reported, not re-applied.
func Recover(store Store, act Actuator) (bool, error) {
	if store == nil {
		return false, nil
	}
	b, ok, err := store.Load()
	if err != nil {
		return false, err
	}
	if !ok {
		return false, nil
	}
	rec, err := unmarshalRollback(b)
	if err != nil {
		_ = store.Clear()
		return false, err
	}
	if err := act.Apply(rec.Inverse); err != nil {
		return false, err
	}
	if err := store.Clear(); err != nil {
		return true, err
	}
	return true, nil
}
