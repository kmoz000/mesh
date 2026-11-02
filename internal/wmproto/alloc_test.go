package wmproto

import "testing"

// TestCodecZeroAlloc guards the allocation discipline the daemon depends on:
// the target runs under TinyGo's leaking collector, so any per-message heap
// allocation in the codec hot path is a permanent leak. Encode and
// Decode+ParseReport must stay at zero allocations per call.
func TestCodecZeroAlloc(t *testing.T) {
	c := newTestCodec(t)
	rb := &ReportBody{NumRadios: 2, NumLinks: 3}
	var body [MaxBody]byte
	encoded, err := AppendReport(body[:0], rb)
	if err != nil {
		t.Fatalf("AppendReport: %v", err)
	}
	h := Header{Type: Report, Flags: CompleteRadios | CompleteLinks, Network: testNet, Sender: testSender, Boot: testBoot, Exchange: testChal, Sequence: 1, LeaseSeconds: ReportLease, KeyID: 7}
	var buf [MaxDatagram]byte

	if got := testing.AllocsPerRun(200, func() {
		if _, err := c.Encode(buf[:], h, encoded); err != nil {
			t.Fatal(err)
		}
	}); got != 0 {
		t.Fatalf("Encode allocs/op = %.1f, want 0", got)
	}

	n, err := c.Encode(buf[:], h, encoded)
	if err != nil {
		t.Fatal(err)
	}
	var out ReportBody
	if got := testing.AllocsPerRun(200, func() {
		_, b, err := c.Decode(buf[:n], testSender)
		if err != nil {
			t.Fatal(err)
		}
		if err := ParseReport(b, &out); err != nil {
			t.Fatal(err)
		}
	}); got != 0 {
		t.Fatalf("Decode+ParseReport allocs/op = %.1f, want 0", got)
	}
}
