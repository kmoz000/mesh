package radio

import (
	"errors"
	"testing"
	"time"
)

// All guard tests run against mocks only: no live device, no socket, no SSH. The
// real socket-backed reader/actuator (actuator_linux.go, guard_linux.go) is
// exercised only for compilation.

// mockReader returns a scripted before reading on the first call and an after
// reading on every later call, so a test can drive the two reads runGuardedTxPower
// makes (before APPLY, and after APPLY for the returned value).
type mockReader struct {
	before, after     int
	beforeOK, afterOK bool
	err               error
	calls             int
}

func (m *mockReader) ReadTxPowerDBm(iface string) (int, bool, error) {
	m.calls++
	if m.err != nil {
		return 0, false, m.err
	}
	if m.calls == 1 {
		return m.before, m.beforeOK, nil
	}
	return m.after, m.afterOK, nil
}

func guardClock() *mockClock { return &mockClock{t: time.Unix(1000, 0)} }

// capsNoRange returns the testCaps radio with the tx-power range unproven.
func capsNoRange() *Capabilities {
	c := testCaps()
	c.Radios[0].Known &^= KnownRange
	c.Radios[0].TxPowerMax = 0
	return c
}

func TestGuardedTxPowerCommit(t *testing.T) {
	caps := testCaps()
	act := &mockActuator{}
	rd := &mockReader{before: 20, beforeOK: true, after: 23, afterOK: true}

	before, after, err := runGuardedTxPower(caps, "wlan0", 23, false,
		act, rd, &MemStore{}, guardClock(), time.Second)
	if err != nil {
		t.Fatalf("commit path err: %v", err)
	}
	if before != 20 || after != 23 {
		t.Fatalf("before=%d after=%d, want 20,23", before, after)
	}
	// Exactly one apply (the target), no inverse apply on a committed change.
	if len(act.applied) != 1 {
		t.Fatalf("applied %d changes, want 1: %+v", len(act.applied), act.applied)
	}
	if !act.applied[0].SetTxPower || act.applied[0].TxPower != 23 {
		t.Fatalf("applied[0]=%+v, want tx power 23", act.applied[0])
	}
	if len(act.verified) != 1 {
		t.Fatalf("verified %d times, want 1", len(act.verified))
	}
}

func TestGuardedTxPowerVerifyFailureReverts(t *testing.T) {
	caps := testCaps()
	verifyErr := errors.New("read 10 dBm, wanted 23")
	act := &mockActuator{verifyFn: func(c Change) error { return verifyErr }}
	rd := &mockReader{before: 20, beforeOK: true, after: 10, afterOK: true}

	before, after, err := runGuardedTxPower(caps, "wlan0", 23, false,
		act, rd, &MemStore{}, guardClock(), time.Second)
	if err == nil {
		t.Fatal("expected a verify error")
	}
	if !errors.Is(err, ErrTxPowerVerify) {
		t.Fatalf("err = %v, want wrapping ErrTxPowerVerify", err)
	}
	if !errors.Is(err, verifyErr) {
		t.Fatalf("err = %v, want to carry the device detail", err)
	}
	if before != 20 || after != 10 {
		t.Fatalf("before=%d after=%d, want 20,10", before, after)
	}
	// Two applies: the target, then the inverse restoring before.
	if len(act.applied) != 2 {
		t.Fatalf("applied %d changes, want 2: %+v", len(act.applied), act.applied)
	}
	last := act.applied[1]
	if !last.SetTxPower || last.TxPower != 20 {
		t.Fatalf("rollback apply = %+v, want tx power 20", last)
	}
}

func TestGuardedTxPowerRevertPath(t *testing.T) {
	caps := testCaps()
	act := &mockActuator{}
	rd := &mockReader{before: 20, beforeOK: true, after: 23, afterOK: true}

	before, after, err := runGuardedTxPower(caps, "wlan0", 23, true,
		act, rd, &MemStore{}, guardClock(), time.Second)
	if err != nil {
		t.Fatalf("revert path err: %v", err)
	}
	// The probe still reports what the driver actually set before reverting.
	if before != 20 || after != 23 {
		t.Fatalf("before=%d after=%d, want 20,23", before, after)
	}
	// Two applies: the target, then the watchdog-forced inverse restoring before.
	if len(act.applied) != 2 {
		t.Fatalf("applied %d changes, want 2: %+v", len(act.applied), act.applied)
	}
	if act.applied[0].TxPower != 23 {
		t.Fatalf("applied[0]=%+v, want target 23", act.applied[0])
	}
	last := act.applied[1]
	if !last.SetTxPower || last.TxPower != 20 {
		t.Fatalf("revert apply = %+v, want tx power 20", last)
	}
}

func TestGuardedTxPowerOutOfRangeRefused(t *testing.T) {
	caps := testCaps() // range is 0..30 dBm
	act := &mockActuator{}
	rd := &mockReader{before: 20, beforeOK: true, after: 40, afterOK: true}

	before, _, err := runGuardedTxPower(caps, "wlan0", 40, false,
		act, rd, &MemStore{}, guardClock(), time.Second)
	if !errors.Is(err, ErrTxPowerRange) {
		t.Fatalf("err = %v, want ErrTxPowerRange", err)
	}
	if before != 20 {
		t.Fatalf("before=%d, want 20", before)
	}
	if len(act.applied) != 0 {
		t.Fatalf("out-of-range change must not actuate, applied %+v", act.applied)
	}
}

func TestGuardedTxPowerUnknownRangeRefused(t *testing.T) {
	caps := capsNoRange()
	act := &mockActuator{}
	rd := &mockReader{before: 20, beforeOK: true, after: 23, afterOK: true}

	_, _, err := runGuardedTxPower(caps, "wlan0", 23, false,
		act, rd, &MemStore{}, guardClock(), time.Second)
	if !errors.Is(err, ErrTxPowerUnkn) {
		t.Fatalf("err = %v, want ErrTxPowerUnkn", err)
	}
	if len(act.applied) != 0 {
		t.Fatalf("unknown-range change must not actuate, applied %+v", act.applied)
	}
}

func TestGuardedTxPowerReadbackUnavailable(t *testing.T) {
	caps := testCaps()
	act := &mockActuator{}
	rd := &mockReader{beforeOK: false} // driver exposes no level

	_, _, err := runGuardedTxPower(caps, "wlan0", 23, false,
		act, rd, &MemStore{}, guardClock(), time.Second)
	if !errors.Is(err, ErrTxPowerReadback) {
		t.Fatalf("err = %v, want ErrTxPowerReadback", err)
	}
	if len(act.applied) != 0 {
		t.Fatalf("no-readback change must not actuate, applied %+v", act.applied)
	}
}

func TestGuardedTxPowerNoNetdev(t *testing.T) {
	caps := testCaps()
	act := &mockActuator{}
	rd := &mockReader{before: 20, beforeOK: true}

	_, _, err := runGuardedTxPower(caps, "wlan9", 23, false,
		act, rd, &MemStore{}, guardClock(), time.Second)
	if !errors.Is(err, ErrNoNetdev) {
		t.Fatalf("err = %v, want ErrNoNetdev", err)
	}
	if rd.calls != 0 {
		t.Fatalf("unresolved iface must not read power, calls=%d", rd.calls)
	}
}

func TestTxPowerWithinTolerance(t *testing.T) {
	cases := []struct {
		read, target, tol int
		want              bool
	}{
		{23, 23, 1, true},
		{22, 23, 1, true},
		{24, 23, 1, true},
		{21, 23, 1, false},
		{25, 23, 1, false},
		{29, 30, 1, true},
	}
	for _, c := range cases {
		if got := txPowerWithinTolerance(c.read, c.target, c.tol); got != c.want {
			t.Errorf("within(%d,%d,%d)=%v want %v", c.read, c.target, c.tol, got, c.want)
		}
	}
}

func TestParseTxPowerLevelDBm(t *testing.T) {
	// 2900 mBm -> 29 dBm, read from the same attribute GET_INTERFACE returns.
	attrs := attrsFor(t, func(w *builder) {
		w.putU32(nl80211AttrIfIndex, 7)
		w.putU32(nl80211AttrWiphyTxPowerLevel, 2900)
	})
	dBm, ok := parseTxPowerLevelDBm(attrs)
	if !ok || dBm != 29 {
		t.Fatalf("parse = %d,%v, want 29,true", dBm, ok)
	}

	absent := attrsFor(t, func(w *builder) {
		w.putU32(nl80211AttrIfIndex, 7)
	})
	if _, ok := parseTxPowerLevelDBm(absent); ok {
		t.Fatal("expected ok=false when the level attr is absent")
	}
}
