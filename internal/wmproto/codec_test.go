package wmproto

import (
	"bytes"
	"testing"
)

var (
	testNet     = NetworkID{1, 2, 3, 4, 5, 6, 7, 8}
	testSender  = NodeID{0xaa, 0xbb, 0xcc, 0xdd, 0xee, 0xff, 1, 2, 3, 4, 5, 6, 7, 8, 9, 10}
	testBoot    = BootID{9, 9, 9, 9, 9, 9, 9, 9}
	testChal    = ExchangeID{0xde, 0xad, 0xbe, 0xef, 1, 2, 3, 4, 5, 6, 7, 8, 9, 10, 11, 12}
	testKey     = bytes.Repeat([]byte{0x5a}, KeySize)
	testPeerMAC = [6]byte{0x02, 0xaa, 0xbb, 0xcc, 0xdd, 0xee}
)

func newTestCodec(t *testing.T) *Codec {
	t.Helper()
	c, err := NewCodec(testNet, 7, testKey)
	if err != nil {
		t.Fatalf("NewCodec: %v", err)
	}
	return c
}

func TestNewCodecValidation(t *testing.T) {
	if _, err := NewCodec(testNet, 0, testKey[:10]); err != ErrKey {
		t.Fatalf("short key: got %v want ErrKey", err)
	}
	if _, err := NewCodec(NetworkID{}, 0, testKey); err != ErrIdentity {
		t.Fatalf("zero network: got %v want ErrIdentity", err)
	}
}

func TestAnnounceRoundTrip(t *testing.T) {
	c := newTestCodec(t)
	h := Header{Type: Announce, Flags: Urgent, Network: testNet, Sender: testSender, Boot: testBoot, KeyID: 7}
	var buf [MaxDatagram]byte
	n, err := c.Encode(buf[:], h, nil)
	if err != nil {
		t.Fatalf("Encode: %v", err)
	}
	got, body, err := c.Decode(buf[:n], testSender)
	if err != nil {
		t.Fatalf("Decode: %v", err)
	}
	if len(body) != 0 || got.Type != Announce || got.Flags != Urgent || got.Sender != testSender {
		t.Fatalf("decoded header mismatch: %+v", got)
	}
}

func TestReportRoundTrip(t *testing.T) {
	c := newTestCodec(t)
	rb := &ReportBody{NumRadios: 2, NumLinks: 1}
	rb.Radios[0] = RadioRecord{Index: 0, Band: 3, Channel: 37, Width: 160, TxPower: 23, Utilization: 40, MLDLink: 0, Flags: 0}
	rb.Radios[1] = RadioRecord{Index: 1, Band: 3, Channel: 100, Width: 160, TxPower: 20, Utilization: 10, MLDLink: 0xff}
	rb.Links[0] = LinkRecord{LocalRadio: 0, Peer: testPeerMAC, RSSI: -55, AgeMs: 1200, Confidence: 200, Flags: LinkBackhaul}

	var body [MaxBody]byte
	encoded, err := AppendReport(body[:0], rb)
	if err != nil {
		t.Fatalf("AppendReport: %v", err)
	}
	h := Header{Type: Report, Flags: CompleteRadios | CompleteLinks, Network: testNet, Sender: testSender, Boot: testBoot, Exchange: testChal, Sequence: 5, LeaseSeconds: ReportLease, KeyID: 7}
	var buf [MaxDatagram]byte
	n, err := c.Encode(buf[:], h, encoded)
	if err != nil {
		t.Fatalf("Encode: %v", err)
	}
	gotH, gotBody, err := c.Decode(buf[:n], testSender)
	if err != nil {
		t.Fatalf("Decode: %v", err)
	}
	if gotH.Sequence != 5 || gotH.LeaseSeconds != ReportLease {
		t.Fatalf("header mismatch: %+v", gotH)
	}
	var out ReportBody
	if err := ParseReport(gotBody, &out); err != nil {
		t.Fatalf("ParseReport: %v", err)
	}
	if out.NumRadios != 2 || out.NumLinks != 1 {
		t.Fatalf("counts: radios=%d links=%d", out.NumRadios, out.NumLinks)
	}
	if out.Radios[0] != rb.Radios[0] || out.Radios[1] != rb.Radios[1] || out.Links[0] != rb.Links[0] {
		t.Fatalf("record round-trip mismatch:\n got %+v\nwant %+v", out, rb)
	}
}

func TestDecodeRejectsTamperedTag(t *testing.T) {
	c := newTestCodec(t)
	h := Header{Type: Announce, Network: testNet, Sender: testSender, Boot: testBoot, KeyID: 7}
	var buf [MaxDatagram]byte
	n, _ := c.Encode(buf[:], h, nil)
	buf[n-1] ^= 0xff // flip a tag byte
	if _, _, err := c.Decode(buf[:n], testSender); err != ErrAuth {
		t.Fatalf("tampered tag: got %v want ErrAuth", err)
	}
}

func TestDecodeRejectsTamperedBody(t *testing.T) {
	c := newTestCodec(t)
	rb := &ReportBody{NumRadios: 1}
	rb.Radios[0] = RadioRecord{Index: 0, Band: 2, Channel: 36, Width: 80}
	var body [MaxBody]byte
	encoded, _ := AppendReport(body[:0], rb)
	h := Header{Type: Report, Network: testNet, Sender: testSender, Boot: testBoot, Exchange: testChal, Sequence: 1, LeaseSeconds: ReportLease, KeyID: 7}
	var buf [MaxDatagram]byte
	n, _ := c.Encode(buf[:], h, encoded)
	buf[HeaderSize] ^= 0x01 // flip a body byte
	if _, _, err := c.Decode(buf[:n], testSender); err != ErrAuth {
		t.Fatalf("tampered body: got %v want ErrAuth", err)
	}
}

func TestDecodeRejectsWrongSender(t *testing.T) {
	c := newTestCodec(t)
	h := Header{Type: Announce, Network: testNet, Sender: testSender, Boot: testBoot, KeyID: 7}
	var buf [MaxDatagram]byte
	n, _ := c.Encode(buf[:], h, nil)
	other := testSender
	other[0] ^= 0x01
	if _, _, err := c.Decode(buf[:n], other); err != ErrIdentity {
		t.Fatalf("wrong sender: got %v want ErrIdentity", err)
	}
}

func TestDecodeRejectsWrongKeyID(t *testing.T) {
	c := newTestCodec(t)
	other, _ := NewCodec(testNet, 8, testKey) // same key material, different keyID
	h := Header{Type: Announce, Network: testNet, Sender: testSender, Boot: testBoot, KeyID: 8}
	var buf [MaxDatagram]byte
	n, _ := other.Encode(buf[:], h, nil)
	if _, _, err := c.Decode(buf[:n], testSender); err != ErrIdentity {
		t.Fatalf("wrong keyID: got %v want ErrIdentity", err)
	}
}

func TestEncodeRejectsBadFlagsPerType(t *testing.T) {
	c := newTestCodec(t)
	var buf [MaxDatagram]byte
	cases := []struct {
		name string
		h    Header
	}{
		{"announce-bad-flag", Header{Type: Announce, Flags: CompleteRadios, Network: testNet, Sender: testSender, Boot: testBoot, KeyID: 7}},
		{"query-nonzero-flag", Header{Type: Query, Flags: Urgent, Network: testNet, Sender: testSender, Boot: testBoot, Exchange: testChal, KeyID: 7}},
		{"report-bad-flag", Header{Type: Report, Flags: Urgent, Network: testNet, Sender: testSender, Boot: testBoot, Exchange: testChal, LeaseSeconds: ReportLease, KeyID: 7}},
	}
	for _, tc := range cases {
		if _, err := c.Encode(buf[:], tc.h, nil); err != ErrFlags {
			t.Fatalf("%s: got %v want ErrFlags", tc.name, err)
		}
	}
}

func TestEncodeRejectsBadHeaderPerType(t *testing.T) {
	c := newTestCodec(t)
	var buf [MaxDatagram]byte
	// Query without a challenge.
	if _, err := c.Encode(buf[:], Header{Type: Query, Network: testNet, Sender: testSender, Boot: testBoot, KeyID: 7}, nil); err != ErrHeader {
		t.Fatalf("query without challenge: got %v want ErrHeader", err)
	}
	// Report with wrong lease.
	h := Header{Type: Report, Network: testNet, Sender: testSender, Boot: testBoot, Exchange: testChal, LeaseSeconds: ReportLease + 1, KeyID: 7}
	if _, err := c.Encode(buf[:], h, nil); err != ErrHeader {
		t.Fatalf("report wrong lease: got %v want ErrHeader", err)
	}
	// Announce with a body.
	if _, err := c.Encode(buf[:], Header{Type: Announce, Network: testNet, Sender: testSender, Boot: testBoot, KeyID: 7}, []byte{1}); err != ErrHeader {
		t.Fatalf("announce with body: got %v want ErrHeader", err)
	}
}

func TestEncodeOverlappingDstBody(t *testing.T) {
	c := newTestCodec(t)
	rb := &ReportBody{NumRadios: 1}
	rb.Radios[0] = RadioRecord{Index: 0, Band: 3, Channel: 37, Width: 160}
	// Place the source body inside the destination buffer at the region the
	// header will overwrite, to exercise the copy-body-first path.
	var buf [MaxDatagram]byte
	src, _ := AppendReport(buf[:0], rb)
	bodyLen := len(src)
	h := Header{Type: Report, Network: testNet, Sender: testSender, Boot: testBoot, Exchange: testChal, Sequence: 1, LeaseSeconds: ReportLease, KeyID: 7}
	n, err := c.Encode(buf[:], h, buf[:bodyLen])
	if err != nil {
		t.Fatalf("overlapping Encode: %v", err)
	}
	_, gotBody, err := c.Decode(buf[:n], testSender)
	if err != nil {
		t.Fatalf("Decode after overlap: %v", err)
	}
	var out ReportBody
	if err := ParseReport(gotBody, &out); err != nil || out.NumRadios != 1 || out.Radios[0] != rb.Radios[0] {
		t.Fatalf("overlap corrupted body: err=%v out=%+v", err, out)
	}
}

func TestEncodeRejectsShortDst(t *testing.T) {
	c := newTestCodec(t)
	h := Header{Type: Announce, Network: testNet, Sender: testSender, Boot: testBoot, KeyID: 7}
	small := make([]byte, HeaderSize+TagSize-1)
	if _, err := c.Encode(small, h, nil); err != ErrLength {
		t.Fatalf("short dst: got %v want ErrLength", err)
	}
}

func TestDecodeRejectsBadLength(t *testing.T) {
	c := newTestCodec(t)
	if _, _, err := c.Decode(make([]byte, HeaderSize+TagSize-1), testSender); err != ErrLength {
		t.Fatalf("too short: got %v want ErrLength", err)
	}
	if _, _, err := c.Decode(make([]byte, MaxDatagram+1), testSender); err != ErrLength {
		t.Fatalf("too long: got %v want ErrLength", err)
	}
}

func TestReportBodyRejects(t *testing.T) {
	// A well-formed one-radio body for the trailing-byte case.
	oneRadio := make([]byte, countsSize+radioRecSize)
	oneRadio[0] = 1
	cases := []struct {
		name string
		body []byte
		want error
	}{
		{"missing-counts", []byte{0}, ErrBody},                                       // shorter than the three count bytes
		{"radio-count-over-cap", []byte{MaxRadios + 1, 0, 0}, ErrCapacity},            // count byte exceeds cap
		{"link-count-over-cap", []byte{0, MaxLinks + 1, 0}, ErrCapacity},
		{"pos-count-over-cap", []byte{0, 0, MaxPositions + 1}, ErrCapacity},
		{"short-for-counts", []byte{1, 0, 0}, ErrBody},     // claims 1 radio but no record bytes
		{"trailing-byte", append(oneRadio, 0x00), ErrBody}, // one extra byte past the records
	}
	for _, tc := range cases {
		if err := scanReportBody(tc.body); err != tc.want {
			t.Fatalf("%s: scan got %v want %v", tc.name, err, tc.want)
		}
		var out ReportBody
		if err := ParseReport(tc.body, &out); err != tc.want {
			t.Fatalf("%s: parse got %v want %v", tc.name, err, tc.want)
		}
	}
}

func TestReportBodyCaps(t *testing.T) {
	// A count byte above the cap must be rejected by both scan and parse.
	body := []byte{MaxRadios + 1, 0, 0}
	if err := scanReportBody(body); err != ErrCapacity {
		t.Fatalf("scan over-cap: got %v want ErrCapacity", err)
	}
	var out ReportBody
	if err := ParseReport(body, &out); err != ErrCapacity {
		t.Fatalf("parse over-cap: got %v want ErrCapacity", err)
	}
}

func TestAppendReportCapacity(t *testing.T) {
	rb := &ReportBody{NumRadios: MaxRadios + 1}
	if _, err := AppendReport(nil, rb); err != ErrCapacity {
		t.Fatalf("over-cap append: got %v want ErrCapacity", err)
	}
}

func TestMaxReportBodyFitsDatagram(t *testing.T) {
	if MaxReportBody > MaxBody {
		t.Fatalf("MaxReportBody=%d exceeds MaxBody=%d", MaxReportBody, MaxBody)
	}
}
