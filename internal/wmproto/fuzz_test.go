package wmproto

import (
	"bytes"
	"testing"
)

// FuzzParseReport hardens the bespoke body parser against untrusted radio
// input. Invariants: ParseReport never panics; a body that parses must
// re-encode byte-identically (canonical form), and scanReportBody must agree
// with ParseReport on acceptance. Run with: go test -run x -fuzz FuzzParseReport
func FuzzParseReport(f *testing.F) {
	// Seeds: empty, a one-radio body, a one-link body, and a full snapshot.
	var scratch [MaxBody]byte
	seed := func(rb *ReportBody) []byte {
		b, err := AppendReport(scratch[:0], rb)
		if err != nil {
			f.Fatalf("seed AppendReport: %v", err)
		}
		return append([]byte(nil), b...)
	}
	f.Add(seed(&ReportBody{}))
	f.Add(seed(&ReportBody{NumRadios: 1}))
	f.Add(seed(&ReportBody{NumLinks: 1}))
	full := &ReportBody{NumRadios: MaxRadios, NumLinks: MaxLinks}
	f.Add(seed(full))
	f.Add([]byte{})
	f.Add([]byte{0xff, 0xff})

	f.Fuzz(func(t *testing.T, body []byte) {
		var out ReportBody
		perr := ParseReport(body, &out)
		serr := scanReportBody(body)
		// scan and parse must agree on whether the body is acceptable.
		if (perr == nil) != (serr == nil) {
			t.Fatalf("scan/parse disagree: parse=%v scan=%v", perr, serr)
		}
		if perr != nil {
			return // rejected inputs need no further checks
		}
		// Accepted bodies must round-trip to the exact same bytes.
		var rebuf [MaxBody]byte
		re, err := AppendReport(rebuf[:0], &out)
		if err != nil {
			t.Fatalf("re-encode of accepted body failed: %v", err)
		}
		if !bytes.Equal(re, body) {
			t.Fatalf("accepted body not canonical:\n in=%x\nout=%x", body, re)
		}
		// And the re-encoded form must parse back to the same struct.
		var out2 ReportBody
		if err := ParseReport(re, &out2); err != nil {
			t.Fatalf("re-parse failed: %v", err)
		}
		if out2.NumRadios != out.NumRadios || out2.NumLinks != out.NumLinks {
			t.Fatalf("counts changed across round-trip")
		}
	})
}

// TestSnapshotSizes documents the wire density the compact schema achieves, so
// a regression that bloats the format is visible. These are the "super
// compressed frames" the design targets.
func TestSnapshotSizes(t *testing.T) {
	var buf [MaxBody]byte

	empty, _ := AppendReport(buf[:0], &ReportBody{})
	full, _ := AppendReport(buf[:0], &ReportBody{NumRadios: MaxRadios, NumLinks: MaxLinks, NumPositions: MaxPositions})

	// A full 8-radio/32-link snapshot must fit a single sub-1200-byte datagram
	// with room for the header and tag, and be far smaller than a tagged
	// encoding of the same data would be.
	onWire := HeaderSize + len(full) + TagSize
	t.Logf("report body: empty=%d full=%d; full datagram on wire=%d/%d",
		len(empty), len(full), onWire, MaxDatagram)
	if len(empty) != countsSize {
		t.Fatalf("empty body = %d, want %d", len(empty), countsSize)
	}
	if len(full) != MaxReportBody {
		t.Fatalf("full body = %d, want %d", len(full), MaxReportBody)
	}
	if onWire > MaxDatagram {
		t.Fatalf("full snapshot datagram %d exceeds MaxDatagram %d", onWire, MaxDatagram)
	}
}
