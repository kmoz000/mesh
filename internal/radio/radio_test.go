package radio

import (
	"os"
	"path/filepath"
	"testing"
	"time"

	"wantasticmesh/internal/wmproto"
)

func mkFile(root, rel, content string) error {
	full := filepath.Join(root, rel)
	if err := os.MkdirAll(filepath.Dir(full), 0o755); err != nil {
		return err
	}
	return os.WriteFile(full, []byte(content), 0o644)
}

func mkFileRaw(t *testing.T, root, rel, content string) {
	t.Helper()
	if err := mkFile(root, rel, content); err != nil {
		t.Fatal(err)
	}
}

// All tests here run against mocks only: no live device, no SSH, no real
// hostapd_cli, no real FIFO, and no real netlink socket. The device-touching
// implementations (actuator_linux.go, genlsock_linux.go, vendoripc_linux.go,
// family_linux.go, probe_linux.go) are exercised only for compilation, never
// executed; device execution is PENDING.

// --- test doubles -----------------------------------------------------------

type mockActuator struct {
	applied  []Change
	verified []Change
	// applyFn, if set, decides the result of the n-th Apply (0-based). nil fn or
	// nil result means success.
	applyFn  func(c Change, n int) error
	verifyFn func(c Change) error
}

func (m *mockActuator) Apply(c Change) error {
	n := len(m.applied)
	m.applied = append(m.applied, c)
	if m.applyFn != nil {
		return m.applyFn(c, n)
	}
	return nil
}

func (m *mockActuator) Verify(c Change) error {
	m.verified = append(m.verified, c)
	if m.verifyFn != nil {
		return m.verifyFn(c)
	}
	return nil
}

type mockClock struct{ t time.Time }

func (m *mockClock) Now() time.Time { return m.t }

type mockPipe struct {
	lines []pipeLine
	err   error
}
type pipeLine struct {
	p    Pipe
	line string
}

func (m *mockPipe) WriteVerb(p Pipe, line string) error {
	if m.err != nil {
		return m.err
	}
	m.lines = append(m.lines, pipeLine{p, line})
	return nil
}

type mockResolver struct {
	ids   map[string]FamilyInfo
	calls int
}

func (m *mockResolver) Resolve(name string) (FamilyInfo, error) {
	m.calls++
	if fi, ok := m.ids[name]; ok {
		return fi, nil
	}
	return FamilyInfo{}, ErrFamilyNotFound
}

// testCaps returns a single fully-known 5 GHz radio.
func testCaps() *Capabilities {
	r := Radio{
		Index: 0, Phy: "phy0", Netdev: "wlan0", Band: Band5,
		Channels: []Channel{
			{Num: 36, Freq: 5180, MaxTxPower: 23},
			{Num: 40, Freq: 5200, MaxTxPower: 23},
			{Num: 44, Freq: 5220, MaxTxPower: 23},
			{Num: 48, Freq: 5240, MaxTxPower: 23},
			{Num: 149, Freq: 5745, MaxTxPower: 30},
			{Num: 153, Freq: 5765, MaxTxPower: 30},
			{Num: 157, Freq: 5785, MaxTxPower: 30},
			{Num: 161, Freq: 5805, MaxTxPower: 30},
		},
		Widths:     []uint16{20, 40, 80},
		TxPowerMin: 0, TxPowerMax: 30,
		CurChannel: 36, CurWidth: 80, CurTxPower: 20, Utilization: 40,
		MLDLink: MLDLinkNone,
		Known:   KnownBand | KnownChannel | KnownWidth | KnownTxPower | KnownRange | KnownUtil,
	}
	return &Capabilities{Radios: []Radio{r}}
}

// attrsFor builds a netlink message body with build and returns its attribute
// region, exercising the real builder and parser so tests ride the wire layout.
func attrsFor(t *testing.T, build func(w *builder)) []byte {
	t.Helper()
	buf := make([]byte, 8192)
	w := newBuilder(buf)
	w.putHeader(0x10, nlmFRequest, 1, 0, 1, 1)
	build(w)
	msg, err := w.finalize()
	if err != nil {
		t.Fatalf("finalize: %v", err)
	}
	m, _, err := parseMessage(msg)
	if err != nil {
		t.Fatalf("parseMessage: %v", err)
	}
	return m.attrs
}

// --- plan / mechanism routing -----------------------------------------------

func TestMechanismRouting(t *testing.T) {
	cases := []struct {
		name string
		c    Change
		want Mechanism
		err  bool
	}{
		{"channel->csa", Change{SetChannel: true, Channel: 36, Width: 80}, MechHostapdCSA, false},
		{"power->nl80211", Change{SetTxPower: true, TxPower: 10}, MechNL80211Power, false},
		{"empty", Change{}, MechNone, true},
		{"mixed", Change{SetChannel: true, SetTxPower: true}, MechNone, true},
	}
	for _, tc := range cases {
		got, err := tc.c.Mechanism()
		if (err != nil) != tc.err {
			t.Errorf("%s: err=%v want err=%v", tc.name, err, tc.err)
		}
		if got != tc.want {
			t.Errorf("%s: mech=%v want %v", tc.name, got, tc.want)
		}
	}
}

func TestValidateAgainstCaps(t *testing.T) {
	caps := testCaps()
	if _, err := Validate(caps, Change{SetChannel: true, Channel: 36, Width: 80}); err != nil {
		t.Fatalf("valid channel change refused: %v", err)
	}
	if _, err := Validate(caps, Change{SetChannel: true, Channel: 999, Width: 80}); err == nil {
		t.Fatal("unlisted channel accepted")
	}
	if _, err := Validate(caps, Change{SetChannel: true, Channel: 36, Width: 160}); err == nil {
		t.Fatal("unsupported width accepted")
	}
	if _, err := Validate(caps, Change{SetTxPower: true, TxPower: 100}); err == nil {
		t.Fatal("over-range tx power accepted")
	}
	if _, err := Validate(caps, Change{SetTxPower: true, TxPower: 10}); err != nil {
		t.Fatalf("in-range tx power refused: %v", err)
	}
}

func TestCheckTxPowerUnknownRangeRefused(t *testing.T) {
	caps := &Capabilities{Radios: []Radio{{Index: 0}}} // no KnownRange
	if err := caps.CheckTxPower(0, 10); err != ErrTxPowerUnkn {
		t.Fatalf("want ErrTxPowerUnkn, got %v", err)
	}
}

// --- transaction lifecycle --------------------------------------------------

func TestTransactionHappyPath(t *testing.T) {
	caps := testCaps()
	act := &mockActuator{}
	store := &MemStore{}
	clk := &mockClock{t: time.Unix(1000, 0)}
	tx := NewTransaction(caps, act, store, clk)

	if err := tx.Prepare(Change{RadioIndex: 0, SetChannel: true, Channel: 149, Width: 80}, time.Second); err != nil {
		t.Fatalf("prepare: %v", err)
	}
	if _, ok, _ := store.Load(); !ok {
		t.Fatal("rollback record not persisted at prepare")
	}
	if err := tx.Apply(); err != nil {
		t.Fatalf("apply: %v", err)
	}
	if err := tx.Verify(); err != nil {
		t.Fatalf("verify: %v", err)
	}
	if err := tx.Commit(); err != nil {
		t.Fatalf("commit: %v", err)
	}
	if tx.State() != StateCommitted {
		t.Fatalf("state=%v", tx.State())
	}
	if _, ok, _ := store.Load(); ok {
		t.Fatal("rollback record not cleared after commit")
	}
	if len(act.applied) != 1 || !act.applied[0].SetChannel {
		t.Fatalf("unexpected applies: %+v", act.applied)
	}
}

func TestTransactionRefusesUnknownRollback(t *testing.T) {
	// channel known but width unknown -> cannot build a complete CSA inverse.
	caps := &Capabilities{Radios: []Radio{{
		Index: 0, Netdev: "wlan0", Band: Band5,
		Channels:   []Channel{{Num: 36, Freq: 5180}},
		Widths:     []uint16{80},
		CurChannel: 36,
		Known:      KnownChannel, // no KnownWidth
	}}}
	tx := NewTransaction(caps, &mockActuator{}, &MemStore{}, &mockClock{})
	if err := tx.Prepare(Change{RadioIndex: 0, SetChannel: true, Channel: 36, Width: 80}, time.Second); err != ErrNoRollback {
		t.Fatalf("want ErrNoRollback, got %v", err)
	}
}

func TestSoleBackhaulRequiresWatchdog(t *testing.T) {
	caps := testCaps()
	caps.Radios[0].Backhaul = true // sole backhaul
	// no deadline -> refuse
	tx := NewTransaction(caps, &mockActuator{}, &MemStore{}, &mockClock{})
	if err := tx.Prepare(Change{RadioIndex: 0, SetChannel: true, Channel: 149, Width: 80}, 0); err != ErrNeedWatchdog {
		t.Fatalf("want ErrNeedWatchdog (no deadline), got %v", err)
	}
	// nil store -> refuse
	tx2 := NewTransaction(caps, &mockActuator{}, nil, &mockClock{})
	if err := tx2.Prepare(Change{RadioIndex: 0, SetChannel: true, Channel: 149, Width: 80}, time.Second); err != ErrNeedWatchdog {
		t.Fatalf("want ErrNeedWatchdog (nil store), got %v", err)
	}
	// deadline + store -> ok
	tx3 := NewTransaction(caps, &mockActuator{}, &MemStore{}, &mockClock{})
	if err := tx3.Prepare(Change{RadioIndex: 0, SetChannel: true, Channel: 149, Width: 80}, time.Second); err != nil {
		t.Fatalf("sole backhaul with watchdog refused: %v", err)
	}
}

func TestWatchdogRevertsOnMissedCommit(t *testing.T) {
	caps := testCaps()
	caps.Radios[0].Backhaul = true
	act := &mockActuator{}
	store := &MemStore{}
	clk := &mockClock{t: time.Unix(1000, 0)}
	tx := NewTransaction(caps, act, store, clk)

	if err := tx.Prepare(Change{RadioIndex: 0, SetChannel: true, Channel: 149, Width: 80}, time.Second); err != nil {
		t.Fatalf("prepare: %v", err)
	}
	if err := tx.Apply(); err != nil {
		t.Fatalf("apply: %v", err)
	}
	// Before deadline: no revert.
	if tx.Tick(clk.t.Add(500 * time.Millisecond)) {
		t.Fatal("watchdog fired before deadline")
	}
	// After deadline, COMMIT never arrived: revert.
	if !tx.Tick(clk.t.Add(2 * time.Second)) {
		t.Fatal("watchdog did not fire after deadline")
	}
	if tx.State() != StateRolledBack {
		t.Fatalf("state=%v, want rolled-back", tx.State())
	}
	// The revert must itself be a channel change (CSA), never a raw op, and must
	// restore the prior channel/width.
	if len(act.applied) != 2 {
		t.Fatalf("applies=%d, want 2 (forward+revert)", len(act.applied))
	}
	rev := act.applied[1]
	if !rev.SetChannel || rev.Channel != 36 || rev.Width != 80 {
		t.Fatalf("revert not a CSA to prior state: %+v", rev)
	}
	if _, ok, _ := store.Load(); ok {
		t.Fatal("rollback record not cleared after revert")
	}
}

func TestVerifyFailureReverts(t *testing.T) {
	caps := testCaps()
	act := &mockActuator{verifyFn: func(c Change) error { return ErrState }}
	tx := NewTransaction(caps, act, &MemStore{}, &mockClock{})
	_ = tx.Prepare(Change{RadioIndex: 0, SetChannel: true, Channel: 149, Width: 80}, time.Second)
	_ = tx.Apply()
	if err := tx.Verify(); err == nil {
		t.Fatal("verify should have failed")
	}
	if tx.State() != StateRolledBack {
		t.Fatalf("state=%v, want rolled-back", tx.State())
	}
	if len(act.applied) != 2 || !act.applied[1].SetChannel || act.applied[1].Channel != 36 {
		t.Fatalf("verify failure did not revert via CSA: %+v", act.applied)
	}
}

func TestApplyFailureReverts(t *testing.T) {
	caps := testCaps()
	act := &mockActuator{applyFn: func(c Change, n int) error {
		if n == 0 {
			return ErrState // forward apply fails
		}
		return nil // inverse succeeds
	}}
	tx := NewTransaction(caps, act, &MemStore{}, &mockClock{})
	_ = tx.Prepare(Change{RadioIndex: 0, SetChannel: true, Channel: 149, Width: 80}, time.Second)
	if err := tx.Apply(); err == nil {
		t.Fatal("apply should have failed")
	}
	if tx.State() != StateRolledBack {
		t.Fatalf("state=%v, want rolled-back", tx.State())
	}
}

func TestRecoverRevertsPersistedRecord(t *testing.T) {
	store := &MemStore{}
	rec := RollbackRecord{
		RadioIndex: 0, Mech: MechHostapdCSA, Netdev: "wlan0",
		Inverse: Change{RadioIndex: 0, SetChannel: true, Channel: 36, Width: 80},
	}
	_ = store.Save(rec.marshal())
	act := &mockActuator{}
	did, err := Recover(store, act)
	if err != nil || !did {
		t.Fatalf("recover did=%v err=%v", did, err)
	}
	if len(act.applied) != 1 || act.applied[0].Channel != 36 {
		t.Fatalf("recover did not re-apply inverse: %+v", act.applied)
	}
	if _, ok, _ := store.Load(); ok {
		t.Fatal("recover did not clear record")
	}
	// No record -> nothing to do.
	did, err = Recover(store, act)
	if did || err != nil {
		t.Fatalf("recover on empty store did=%v err=%v", did, err)
	}
}

// --- rollback marshalling ---------------------------------------------------

func TestRollbackRoundTrip(t *testing.T) {
	rec := RollbackRecord{
		RadioIndex: 3, Mech: MechNL80211Power, Netdev: "wlan1",
		Inverse: Change{RadioIndex: 3, SetTxPower: true, TxPower: -5},
	}
	got, err := unmarshalRollback(rec.marshal())
	if err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if got.RadioIndex != 3 || got.Mech != MechNL80211Power || got.Netdev != "wlan1" ||
		!got.Inverse.SetTxPower || got.Inverse.TxPower != -5 {
		t.Fatalf("round trip mismatch: %+v", got)
	}
	if _, err := unmarshalRollback([]byte{9, 9, 9}); err != ErrBadRollback {
		t.Fatalf("want ErrBadRollback, got %v", err)
	}
}

// --- vendor IPC -------------------------------------------------------------

func TestVendorVerbLines(t *testing.T) {
	if l, _ := buildVerbLine(VerbBkScan); l != "bkscan" {
		t.Fatalf("bkscan line=%q", l)
	}
	if l, _ := buildVerbLine(VerbPeerBSSInfo, "aa:bb:cc:dd:ee:ff"); l != "peer_bss_info aa:bb:cc:dd:ee:ff" {
		t.Fatalf("peer_bss_info line=%q", l)
	}
	if _, err := buildVerbLine(VerbBkScan, "x"); err != ErrVendorArgs {
		t.Fatalf("args on no-arg verb: %v", err)
	}
	if _, err := buildVerbLine(VerbPeerBSSInfo, "bad arg"); err != ErrVendorArgs {
		t.Fatalf("whitespace arg accepted: %v", err)
	}
}

func TestVendorPipeMapping(t *testing.T) {
	if VerbBkScan.Pipe != PipeAgent {
		t.Fatal("bkscan must be on agent pipe")
	}
	if VerbChanPrefQuery.Pipe != PipeCtrl || VerbTopOpt.Pipe != PipeCtrl || VerbPeerBSSInfo.Pipe != PipeCtrl {
		t.Fatal("ctrl verbs mis-mapped")
	}
	if PipeAgent.Path() != "/var/run/ezmesh-agent-cmd.fifo" || PipeCtrl.Path() != "/var/run/ezmesh-ctrl-cmd.fifo" {
		t.Fatal("pipe paths wrong")
	}
}

func TestVendorTriggerGatedByBusy(t *testing.T) {
	pw := &mockPipe{}
	busy := true
	tr := &VendorTrigger{W: pw, Busy: func() bool { return busy }}
	if err := tr.BackhaulScan(); err != ErrVendorBusy {
		t.Fatalf("trigger not refused while busy: %v", err)
	}
	busy = false
	if err := tr.ChanPrefQuery(); err != nil {
		t.Fatalf("trigger refused while idle: %v", err)
	}
	if len(pw.lines) != 1 || pw.lines[0].p != PipeCtrl || pw.lines[0].line != "chanprefquery" {
		t.Fatalf("unexpected pipe writes: %+v", pw.lines)
	}
}

// --- family resolution ------------------------------------------------------

func TestGetFamilyRoundTrip(t *testing.T) {
	// Build a GETFAMILY reply as the kernel would and parse it back.
	attrs := attrsFor(t, func(w *builder) {
		w.putU16(ctrlAttrFamilyID, 0x2a)
		w.putString(ctrlAttrFamilyName, "hyfi")
		w.beginNested(ctrlAttrMcastGroups)
		w.beginNested(1)
		w.putString(ctrlAttrMcastGrpName, "events")
		w.putU32(ctrlAttrMcastGrpID, 7)
		w.endNested()
		w.endNested()
	})
	fi, err := parseGetFamilyReply(attrs)
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	if fi.ID != 0x2a {
		t.Fatalf("family id=%#x", fi.ID)
	}
	if fi.Groups["events"] != 7 {
		t.Fatalf("mcast groups=%+v", fi.Groups)
	}
	// A reply without a family id is malformed.
	bad := attrsFor(t, func(w *builder) { w.putString(ctrlAttrFamilyName, "x") })
	if _, err := parseGetFamilyReply(bad); err != ErrFamilyReply {
		t.Fatalf("want ErrFamilyReply, got %v", err)
	}
}

func TestBuildGetFamilyShape(t *testing.T) {
	buf := make([]byte, 256)
	msg, err := buildGetFamily(buf, "emesh", 5, 0)
	if err != nil {
		t.Fatalf("build: %v", err)
	}
	m, _, err := parseMessage(msg)
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	if m.msgType != genlIDCtrl || m.cmd != ctrlCmdGetFamily {
		t.Fatalf("header: type=%#x cmd=%d", m.msgType, m.cmd)
	}
	name, ok := "", false
	it := newAttrIter(m.attrs)
	for {
		at, p, more := it.next()
		if !more {
			break
		}
		if at == ctrlAttrFamilyName {
			name, ok = cstr(p), true
		}
	}
	if !ok || name != "emesh" {
		t.Fatalf("family name attr=%q ok=%v", name, ok)
	}
}

func TestCachedResolverMemoizes(t *testing.T) {
	mr := &mockResolver{ids: map[string]FamilyInfo{"hyfi": {ID: 9}}}
	cr := &CachedResolver{R: mr}
	for i := 0; i < 3; i++ {
		fi, err := cr.Resolve("hyfi")
		if err != nil || fi.ID != 9 {
			t.Fatalf("resolve: fi=%+v err=%v", fi, err)
		}
	}
	// miss is also cached.
	for i := 0; i < 2; i++ {
		if _, err := cr.Resolve("absent"); err != ErrFamilyNotFound {
			t.Fatalf("want not found, got %v", err)
		}
	}
	if mr.calls != 2 {
		t.Fatalf("resolver called %d times, want 2 (memoized)", mr.calls)
	}
}

// --- CSA / tx power builders ------------------------------------------------

func TestPlanCSACenterFreqs(t *testing.T) {
	r5 := &testCaps().Radios[0]
	cases := []struct {
		ch, w    uint16
		wantPrim uint16
		wantCF1  uint16
		wantOff  int
	}{
		{36, 80, 5180, 5210, 0},  // block 36-48 center ch42
		{149, 80, 5745, 5775, 0}, // block 149-161 center ch155
		{36, 20, 5180, 5180, 0},
		{36, 40, 5180, 5190, 1},  // 36/40 -> center ch38 (5190), sec +1
		{40, 40, 5200, 5190, -1}, // upper of pair -> sec -1
	}
	for _, tc := range cases {
		p, err := planCSA(r5, tc.ch, tc.w)
		if err != nil {
			t.Fatalf("planCSA(%d,%d): %v", tc.ch, tc.w, err)
		}
		if p.primaryFreq != tc.wantPrim || p.centerFreq1 != tc.wantCF1 || p.secOffset != tc.wantOff {
			t.Errorf("planCSA(%d,%d)=%+v want prim=%d cf1=%d off=%d",
				tc.ch, tc.w, p, tc.wantPrim, tc.wantCF1, tc.wantOff)
		}
	}
	// 6 GHz regular grid.
	r6 := &Radio{Band: Band6, Channels: []Channel{{Num: 37, Freq: 6135}}}
	p, err := planCSA(r6, 37, 80)
	if err != nil {
		t.Fatalf("6ghz planCSA: %v", err)
	}
	// block lowest 33, center ch39 -> 5950+5*39 = 6145.
	if p.centerFreq1 != 6145 {
		t.Fatalf("6ghz cf1=%d want 6145", p.centerFreq1)
	}
	// A channel that is in no segment for the width is refused, not guessed.
	if _, err := planCSA(r5, 34, 80); err != ErrCSAUnsupported {
		t.Fatalf("channel 34 (no 80MHz segment) should be unsupported, got %v", err)
	}
}

func TestHostapdChanSwitchArgs(t *testing.T) {
	r := &testCaps().Radios[0]
	iface, args, err := hostapdChanSwitchArgs(r, Change{SetChannel: true, Channel: 36, Width: 80}, 5, "")
	if err != nil {
		t.Fatalf("args: %v", err)
	}
	if iface != "wlan0" {
		t.Fatalf("iface=%q", iface)
	}
	want := []string{"chan_switch", "5", "5180", "center_freq1=5210", "bandwidth=80", "vht"}
	if len(args) != len(want) {
		t.Fatalf("args=%v want %v", args, want)
	}
	for i := range want {
		if args[i] != want[i] {
			t.Fatalf("args[%d]=%q want %q (full %v)", i, args[i], want[i], args)
		}
	}
	// No netdev -> refuse.
	r2 := &Radio{Band: Band5, Channels: []Channel{{Num: 36, Freq: 5180}}}
	if _, _, err := hostapdChanSwitchArgs(r2, Change{SetChannel: true, Channel: 36, Width: 80}, 5, ""); err != ErrCSANoNetdev {
		t.Fatalf("missing netdev not refused: %v", err)
	}
}

func TestBuildSetWiphyTxPower(t *testing.T) {
	buf := make([]byte, 256)
	msg, err := buildSetWiphyTxPower(buf, 0x1b, 7, 20, 3, 0)
	if err != nil {
		t.Fatalf("build: %v", err)
	}
	m, _, err := parseMessage(msg)
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	if m.msgType != 0x1b || m.cmd != nl80211CmdSetWiphy {
		t.Fatalf("header type=%#x cmd=%d", m.msgType, m.cmd)
	}
	var ifindex, setting, level uint32
	it := newAttrIter(m.attrs)
	for {
		at, p, ok := it.next()
		if !ok {
			break
		}
		switch at {
		case nl80211AttrIfIndex:
			ifindex, _ = attrU32(p)
		case nl80211AttrWiphyTxPowerSetng:
			setting, _ = attrU32(p)
		case nl80211AttrWiphyTxPowerLevel:
			level, _ = attrU32(p)
		}
	}
	// tx power is set LIMITED and scoped to the interface (ifindex), the way
	// `iw dev <iface> set txpower limit` does it; the driver clamps dBm to the
	// nearest regulatory step.
	if ifindex != 7 || setting != txPowerLimited || level != 2000 {
		t.Fatalf("ifindex=%d setting=%d level=%d (want 7,%d,2000)", ifindex, setting, txPowerLimited, level)
	}
}

// --- probe parsers ----------------------------------------------------------

func TestApplyWiphyAttrs(t *testing.T) {
	attrs := attrsFor(t, func(w *builder) {
		w.putU32(nl80211AttrWiphy, 0)
		w.beginNested(nl80211AttrWiphyBands)
		w.beginNested(0) // band 0
		w.beginNested(nl80211BandAttrFreqs)
		w.beginNested(0) // freq entry 0
		w.putU32(nl80211FreqAttrFreq, 5180)
		w.putU32(nl80211FreqAttrMaxTxPower, 2300)
		w.endNested()
		w.beginNested(1) // freq entry 1, disabled
		w.putU32(nl80211FreqAttrFreq, 5200)
		w.putFlag(nl80211FreqAttrDisabled)
		w.endNested()
		w.endNested()
		w.endNested()
		w.endNested()
	})
	var r Radio
	if err := applyWiphyAttrs(attrs, &r); err != nil {
		t.Fatalf("apply: %v", err)
	}
	if len(r.Channels) != 2 {
		t.Fatalf("channels=%d want 2", len(r.Channels))
	}
	if r.Channels[0].Num != 36 || r.Channels[0].MaxTxPower != 23 {
		t.Fatalf("ch0=%+v", r.Channels[0])
	}
	if !r.Channels[1].Disabled {
		t.Fatal("ch1 should be disabled")
	}
	if r.Band != Band5 || r.Known&KnownBand == 0 {
		t.Fatal("band not derived")
	}
	if r.Known&KnownRange == 0 || r.TxPowerMax != 23 {
		t.Fatalf("tx range not set: max=%d known=%#x", r.TxPowerMax, r.Known)
	}
}

func TestParseInterfaceMLDBothShapes(t *testing.T) {
	// Shape A: top-level MLO_LINK_ID + MLD_ADDR.
	a := attrsFor(t, func(w *builder) {
		w.putU16(nl80211AttrMLOLinkID, 2)
		w.putAttr(nl80211AttrMLDAddr, []byte{1, 2, 3, 4, 5, 6})
	})
	id, addr, hasLink, hasAddr, err := parseInterfaceMLD(a)
	if err != nil || !hasLink || id != 2 || !hasAddr || addr != [6]byte{1, 2, 3, 4, 5, 6} {
		t.Fatalf("shape A: id=%d addr=%v link=%v addr?=%v err=%v", id, addr, hasLink, hasAddr, err)
	}
	// Shape B: nested MLO_LINKS carrying the link id.
	b := attrsFor(t, func(w *builder) {
		w.beginNested(nl80211AttrMLOLinks)
		w.beginNested(0)
		w.putU16(nl80211AttrMLOLinkID, 5)
		w.endNested()
		w.endNested()
	})
	id, _, hasLink, _, err = parseInterfaceMLD(b)
	if err != nil || !hasLink || id != 5 {
		t.Fatalf("shape B: id=%d link=%v err=%v", id, hasLink, err)
	}
}

func TestParseOperChannel(t *testing.T) {
	attrs := attrsFor(t, func(w *builder) {
		w.putU32(nl80211AttrChannelWidth, chanWidth80)
		w.putU32(nl80211AttrWiphyFreq, 5180)
	})
	width, hw, ch, hc, err := parseOperChannel(attrs)
	if err != nil || !hw || width != 80 || !hc || ch != 36 {
		t.Fatalf("width=%d(%v) ch=%d(%v) err=%v", width, hw, ch, hc, err)
	}
}

// --- source / sysfs ---------------------------------------------------------

func TestSourceRadios(t *testing.T) {
	src := NewSource(*testCaps())
	dst := make([]wmproto.RadioRecord, wmproto.MaxRadios)
	n := src.Radios(dst, time.Now())
	if n != 1 {
		t.Fatalf("n=%d want 1", n)
	}
	rec := dst[0]
	if rec.Band != uint8(Band5) || rec.Channel != 36 || rec.Width != 80 || rec.TxPower != 20 || rec.Utilization != 40 {
		t.Fatalf("record=%+v", rec)
	}
	if rec.MLDLink != MLDLinkNone {
		t.Fatalf("MLDLink=%d want none", rec.MLDLink)
	}
}

func TestSourceFlagsFromCapability(t *testing.T) {
	caps := testCaps()
	caps.Radios[0].Backhaul = true
	caps.Radios[0].MLO = true
	caps.Radios[0].MLDLink = 1
	src := NewSource(*caps)
	dst := make([]wmproto.RadioRecord, 1)
	src.Radios(dst, time.Now())
	if dst[0].Flags&wmproto.RadioBackhaul == 0 || dst[0].Flags&wmproto.RadioMLO == 0 {
		t.Fatalf("flags=%#x", dst[0].Flags)
	}
	if dst[0].MLDLink != 1 {
		t.Fatalf("MLDLink=%d", dst[0].MLDLink)
	}
}

func TestEnumerateFakeSysfs(t *testing.T) {
	root := t.TempDir()
	mk := func(p string) {
		if err := mkFile(root, p, "0\n"); err != nil {
			t.Fatal(err)
		}
	}
	mk("sys/class/ieee80211/phy0/index")
	mkFileRaw(t, root, "sys/class/net/wlan0/phy80211/name", "phy0\n")
	en := Enumerator{Root: root}
	radios, err := en.Enumerate()
	if err != nil {
		t.Fatalf("enumerate: %v", err)
	}
	if len(radios) != 1 || radios[0].Phy != "phy0" || radios[0].Netdev != "wlan0" {
		t.Fatalf("radios=%+v", radios)
	}
}
