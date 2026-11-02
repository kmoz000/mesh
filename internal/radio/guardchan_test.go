package radio

import (
	"errors"
	"net"
	"os"
	"path/filepath"
	"testing"
	"time"
)

// All channel-switch guard tests run against mocks only: no live device, no
// socket, no SSH. The real socket/exec-backed reader/actuator
// (actuator_linux.go, guardchan_linux.go) is exercised only for compilation.

// mockOperReader returns a scripted before channel on the first call and an after
// channel on every later call, mirroring mockReader for tx power: the guard reads
// the channel twice (before APPLY, and after APPLY for the returned value).
type mockOperReader struct {
	before, after     int
	width             int
	beforeOK, afterOK bool
	err               error
	calls             int
}

func (m *mockOperReader) ReadOperChannel(iface string) (int, int, bool, error) {
	m.calls++
	if m.err != nil {
		return 0, 0, false, m.err
	}
	if m.calls == 1 {
		return m.before, m.width, m.beforeOK, nil
	}
	return m.after, m.width, m.afterOK, nil
}

// caps24 returns a single 2.4 GHz radio supporting channels 1..13 at 20/40 MHz.
func caps24() *Capabilities {
	chans := make([]Channel, 0, 13)
	for n := uint16(1); n <= 13; n++ {
		chans = append(chans, Channel{Num: n, Freq: 2407 + 5*n, MaxTxPower: 20})
	}
	r := Radio{
		Index: 0, Phy: "phy0", Netdev: "wlan0", Band: Band24,
		Channels:   chans,
		Widths:     []uint16{20, 40},
		TxPowerMin: 0, TxPowerMax: 20,
		CurChannel: 1, CurWidth: 20, CurTxPower: 20,
		MLDLink: MLDLinkNone,
		Known:   KnownBand | KnownChannel | KnownWidth | KnownTxPower | KnownRange,
	}
	return &Capabilities{Radios: []Radio{r}}
}

func TestGuardedChannelSwitchCommit(t *testing.T) {
	caps := testCaps() // 5 GHz, oper 36@80, supports 149@80
	act := &mockActuator{}
	rd := &mockOperReader{before: 36, width: 80, beforeOK: true, after: 149, afterOK: true}

	before, after, err := runGuardedChannelSwitch(caps, "wlan0", 149, false,
		act, rd, &MemStore{}, guardClock(), nil, time.Second)
	if err != nil {
		t.Fatalf("commit path err: %v", err)
	}
	if before != 36 || after != 149 {
		t.Fatalf("before=%d after=%d, want 36,149", before, after)
	}
	// Exactly one apply (the target), no inverse apply on a committed change.
	if len(act.applied) != 1 {
		t.Fatalf("applied %d changes, want 1: %+v", len(act.applied), act.applied)
	}
	if !act.applied[0].SetChannel || act.applied[0].Channel != 149 || act.applied[0].Width != 80 {
		t.Fatalf("applied[0]=%+v, want CSA to 149@80", act.applied[0])
	}
	if len(act.verified) != 1 {
		t.Fatalf("verified %d times, want 1", len(act.verified))
	}
}

func TestGuardedChannelSwitchVerifyFailureReverts(t *testing.T) {
	caps := testCaps()
	verifyErr := errors.New("channel is 36, wanted 149")
	act := &mockActuator{verifyFn: func(c Change) error { return verifyErr }}
	rd := &mockOperReader{before: 36, width: 80, beforeOK: true, after: 36, afterOK: true}

	before, after, err := runGuardedChannelSwitch(caps, "wlan0", 149, false,
		act, rd, &MemStore{}, guardClock(), nil, time.Second)
	if err == nil {
		t.Fatal("expected a verify error")
	}
	if !errors.Is(err, ErrChanVerify) {
		t.Fatalf("err = %v, want wrapping ErrChanVerify", err)
	}
	if !errors.Is(err, verifyErr) {
		t.Fatalf("err = %v, want to carry the device detail", err)
	}
	if before != 36 || after != 36 {
		t.Fatalf("before=%d after=%d, want 36,36", before, after)
	}
	// Two applies: the target, then the inverse restoring before via CSA.
	if len(act.applied) != 2 {
		t.Fatalf("applied %d changes, want 2: %+v", len(act.applied), act.applied)
	}
	last := act.applied[1]
	if !last.SetChannel || last.Channel != 36 || last.Width != 80 {
		t.Fatalf("rollback apply = %+v, want CSA to 36@80", last)
	}
}

func TestGuardedChannelSwitchRevertPath(t *testing.T) {
	caps := testCaps()
	act := &mockActuator{}
	rd := &mockOperReader{before: 36, width: 80, beforeOK: true, after: 149, afterOK: true}

	before, after, err := runGuardedChannelSwitch(caps, "wlan0", 149, true,
		act, rd, &MemStore{}, guardClock(), nil, time.Second)
	if err != nil {
		t.Fatalf("revert path err: %v", err)
	}
	// The probe still reports what the driver actually switched to before reverting.
	if before != 36 || after != 149 {
		t.Fatalf("before=%d after=%d, want 36,149", before, after)
	}
	// Two applies: the target, then the watchdog-forced inverse restoring before.
	if len(act.applied) != 2 {
		t.Fatalf("applied %d changes, want 2: %+v", len(act.applied), act.applied)
	}
	if act.applied[0].Channel != 149 {
		t.Fatalf("applied[0]=%+v, want target 149", act.applied[0])
	}
	last := act.applied[1]
	if !last.SetChannel || last.Channel != 36 || last.Width != 80 {
		t.Fatalf("revert apply = %+v, want CSA to 36@80", last)
	}
}

func TestGuardedChannelSwitchBadTargetRefused(t *testing.T) {
	caps := testCaps() // supports 36,40,44,48,149,153,157,161
	act := &mockActuator{}
	rd := &mockOperReader{before: 36, width: 80, beforeOK: true, after: 999, afterOK: true}

	before, _, err := runGuardedChannelSwitch(caps, "wlan0", 999, false,
		act, rd, &MemStore{}, guardClock(), nil, time.Second)
	if !errors.Is(err, ErrChannel) {
		t.Fatalf("err = %v, want ErrChannel", err)
	}
	if before != 36 {
		t.Fatalf("before=%d, want 36", before)
	}
	if len(act.applied) != 0 {
		t.Fatalf("bad-target change must not actuate, applied %+v", act.applied)
	}
}

func TestGuardedChannelSwitch24ChannelRestricted(t *testing.T) {
	caps := caps24() // 2.4 GHz, 1..13
	act := &mockActuator{}
	rd := &mockOperReader{before: 1, width: 20, beforeOK: true, after: 14, afterOK: true}

	// Channel 14 is outside the allowed 1..13 set for 2.4 GHz.
	before, _, err := runGuardedChannelSwitch(caps, "wlan0", 14, false,
		act, rd, &MemStore{}, guardClock(), nil, time.Second)
	if !errors.Is(err, ErrChannel) {
		t.Fatalf("err = %v, want ErrChannel for 2.4 GHz ch 14", err)
	}
	if before != 1 {
		t.Fatalf("before=%d, want 1", before)
	}
	if len(act.applied) != 0 {
		t.Fatalf("restricted 2.4 GHz target must not actuate, applied %+v", act.applied)
	}
}

func TestGuardedChannelSwitchWidthUnknownRefused(t *testing.T) {
	caps := testCaps()
	act := &mockActuator{}
	// Channel read back, but width unknown (0): cannot preserve width or roll back.
	rd := &mockOperReader{before: 36, width: 0, beforeOK: true, after: 149, afterOK: true}

	_, _, err := runGuardedChannelSwitch(caps, "wlan0", 149, false,
		act, rd, &MemStore{}, guardClock(), nil, time.Second)
	if !errors.Is(err, ErrChanWidthUnknown) {
		t.Fatalf("err = %v, want ErrChanWidthUnknown", err)
	}
	if len(act.applied) != 0 {
		t.Fatalf("unknown-width change must not actuate, applied %+v", act.applied)
	}
}

func TestGuardedChannelSwitchReadbackUnavailable(t *testing.T) {
	caps := testCaps()
	act := &mockActuator{}
	rd := &mockOperReader{beforeOK: false} // driver exposes no channel

	_, _, err := runGuardedChannelSwitch(caps, "wlan0", 149, false,
		act, rd, &MemStore{}, guardClock(), nil, time.Second)
	if !errors.Is(err, ErrChanReadback) {
		t.Fatalf("err = %v, want ErrChanReadback", err)
	}
	if len(act.applied) != 0 {
		t.Fatalf("no-readback change must not actuate, applied %+v", act.applied)
	}
}

func TestGuardedChannelSwitchNoNetdev(t *testing.T) {
	caps := testCaps()
	act := &mockActuator{}
	rd := &mockOperReader{before: 36, width: 80, beforeOK: true}

	_, _, err := runGuardedChannelSwitch(caps, "wlan9", 149, false,
		act, rd, &MemStore{}, guardClock(), nil, time.Second)
	if !errors.Is(err, ErrNoNetdev) {
		t.Fatalf("err = %v, want ErrNoNetdev", err)
	}
	if rd.calls != 0 {
		t.Fatalf("unresolved iface must not read channel, calls=%d", rd.calls)
	}
}

func TestHostapdCtrlDirMissingRefused(t *testing.T) {
	// An empty root (no /var/run at all) must refuse.
	if _, err := hostapdCtrlDir(t.TempDir(), "ath0"); !errors.Is(err, ErrNoHostapdCtrl) {
		t.Fatalf("empty root: err = %v, want ErrNoHostapdCtrl", err)
	}

	// A hostapd dir that holds a socket for a *different* iface must still refuse
	// for ours.
	root := t.TempDir()
	dir := filepath.Join(root, "var", "run", "hostapd-wifi0")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	// A regular file named like our iface must not count as a control socket.
	if err := os.WriteFile(filepath.Join(dir, "ath0"), nil, 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := hostapdCtrlDir(root, "ath0"); !errors.Is(err, ErrNoHostapdCtrl) {
		t.Fatalf("regular file (not a socket): err = %v, want ErrNoHostapdCtrl", err)
	}
}

func TestHostapdCtrlDirFound(t *testing.T) {
	// Create a real UNIX socket named exactly after the iface inside a hostapd-*
	// dir, and confirm discovery returns that dir. Keep the path short: macOS
	// caps sun_path at ~104 bytes, and t.TempDir() paths are long.
	base, err := os.MkdirTemp(os.TempDir(), "csa")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.RemoveAll(base) })
	dir := filepath.Join(base, "var", "run", "hostapd-w0")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	sock := filepath.Join(dir, "ath0")
	l, err := net.Listen("unix", sock)
	if err != nil {
		t.Skipf("cannot create unix socket (path limit?): %v", err)
	}
	defer l.Close()

	got, err := hostapdCtrlDir(base, "ath0")
	if err != nil {
		t.Fatalf("discovery err: %v", err)
	}
	if got != dir {
		t.Fatalf("ctrl dir = %q, want %q", got, dir)
	}
}
