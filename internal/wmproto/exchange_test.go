package wmproto

import (
	"testing"
	"time"
)

func reportHeader(seq uint64, boot BootID, chal ExchangeID) Header {
	return Header{Type: Report, Network: testNet, Sender: testSender, Boot: boot, Exchange: chal, Sequence: seq, LeaseSeconds: ReportLease, KeyID: 7}
}

func TestExchangeHappyPath(t *testing.T) {
	var c Controller
	t0 := time.Unix(1000, 0)
	if err := c.IssueChallenge(testSender, testChal, t0); err != nil {
		t.Fatalf("IssueChallenge: %v", err)
	}
	if !c.Pending(testSender, t0) {
		t.Fatal("challenge should be pending")
	}
	if err := c.AcceptReport(reportHeader(1, testBoot, testChal), t0.Add(time.Second)); err != nil {
		t.Fatalf("AcceptReport: %v", err)
	}
	if c.Pending(testSender, t0) {
		t.Fatal("challenge should be consumed after accept")
	}
	if c.Count() != 1 {
		t.Fatalf("Count=%d want 1", c.Count())
	}
}

func TestExchangeReplayFailsAfterConsume(t *testing.T) {
	var c Controller
	t0 := time.Unix(2000, 0)
	_ = c.IssueChallenge(testSender, testChal, t0)
	h := reportHeader(1, testBoot, testChal)
	if err := c.AcceptReport(h, t0); err != nil {
		t.Fatalf("first accept: %v", err)
	}
	if err := c.AcceptReport(h, t0.Add(time.Second)); err != ErrChallenge {
		t.Fatalf("replayed report: got %v want ErrChallenge", err)
	}
}

func TestExchangeWrongChallenge(t *testing.T) {
	var c Controller
	t0 := time.Unix(3000, 0)
	_ = c.IssueChallenge(testSender, testChal, t0)
	bad := testChal
	bad[0] ^= 0xff
	if err := c.AcceptReport(reportHeader(1, testBoot, bad), t0); err != ErrChallenge {
		t.Fatalf("wrong challenge: got %v want ErrChallenge", err)
	}
}

func TestExchangeNoPendingChallenge(t *testing.T) {
	var c Controller
	t0 := time.Unix(3500, 0)
	if err := c.AcceptReport(reportHeader(1, testBoot, testChal), t0); err != ErrChallenge {
		t.Fatalf("no challenge issued: got %v want ErrChallenge", err)
	}
}

func TestExchangeChallengeExpiry(t *testing.T) {
	var c Controller
	t0 := time.Unix(4000, 0)
	_ = c.IssueChallenge(testSender, testChal, t0)
	late := t0.Add(time.Duration(ReportLease)*time.Second + time.Second)
	if err := c.AcceptReport(reportHeader(1, testBoot, testChal), late); err != ErrChallenge {
		t.Fatalf("expired challenge: got %v want ErrChallenge", err)
	}
	if c.Pending(testSender, late) {
		t.Fatal("expired challenge should not read as pending")
	}
}

func TestExchangeSequenceWithinBoot(t *testing.T) {
	var c Controller
	t0 := time.Unix(5000, 0)
	// Accept seq 5.
	_ = c.IssueChallenge(testSender, testChal, t0)
	if err := c.AcceptReport(reportHeader(5, testBoot, testChal), t0); err != nil {
		t.Fatalf("accept seq 5: %v", err)
	}
	// Re-challenge, then a lower sequence in the same boot must be ErrReplay.
	chal2 := testChal
	chal2[0] = 0x11
	_ = c.IssueChallenge(testSender, chal2, t0.Add(time.Second))
	if err := c.AcceptReport(reportHeader(4, testBoot, chal2), t0.Add(2*time.Second)); err != ErrReplay {
		t.Fatalf("stale sequence: got %v want ErrReplay", err)
	}
	// Same sequence is also a replay.
	_ = c.IssueChallenge(testSender, chal2, t0.Add(3*time.Second))
	if err := c.AcceptReport(reportHeader(5, testBoot, chal2), t0.Add(4*time.Second)); err != ErrReplay {
		t.Fatalf("equal sequence: got %v want ErrReplay", err)
	}
}

func TestExchangeNewBootResetsSequence(t *testing.T) {
	var c Controller
	t0 := time.Unix(6000, 0)
	_ = c.IssueChallenge(testSender, testChal, t0)
	if err := c.AcceptReport(reportHeader(9, testBoot, testChal), t0); err != nil {
		t.Fatalf("accept seq 9: %v", err)
	}
	// A new boot id legitimately restarts from a low sequence.
	newBoot := testBoot
	newBoot[0] = 0x42
	chal2 := testChal
	chal2[0] = 0x22
	_ = c.IssueChallenge(testSender, chal2, t0.Add(time.Second))
	if err := c.AcceptReport(reportHeader(1, newBoot, chal2), t0.Add(2*time.Second)); err != nil {
		t.Fatalf("new boot seq 1: %v", err)
	}
}

func TestExchangeClockGuard(t *testing.T) {
	var c Controller
	t0 := time.Unix(7000, 0)
	_ = c.IssueChallenge(testSender, testChal, t0)
	if err := c.IssueChallenge(testSender, testChal, t0.Add(-time.Second)); err != ErrClock {
		t.Fatalf("backwards issue: got %v want ErrClock", err)
	}
	if err := c.AcceptReport(reportHeader(1, testBoot, testChal), t0.Add(-time.Second)); err != ErrClock {
		t.Fatalf("backwards accept: got %v want ErrClock", err)
	}
}

func TestExchangeRejectsNonReport(t *testing.T) {
	var c Controller
	if err := c.AcceptReport(Header{Type: Announce, Sender: testSender}, time.Unix(8000, 0)); err != ErrType {
		t.Fatalf("non-report: got %v want ErrType", err)
	}
}

func TestExchangeCapacityEviction(t *testing.T) {
	var c Controller
	base := time.Unix(9000, 0)
	// Fill all slots with distinct agents.
	for i := 0; i < MaxAgents; i++ {
		id := testSender
		id[0] = byte(i)
		id[1] = byte(i >> 8)
		if err := c.IssueChallenge(id, testChal, base.Add(time.Duration(i)*time.Second)); err != nil {
			t.Fatalf("fill %d: %v", i, err)
		}
	}
	if c.Count() != MaxAgents {
		t.Fatalf("Count=%d want %d", c.Count(), MaxAgents)
	}
	// One more agent must evict the oldest, keeping the table bounded.
	extra := testSender
	extra[0] = 0xff
	extra[2] = 0xff
	if err := c.IssueChallenge(extra, testChal, base.Add(time.Duration(MaxAgents)*time.Second)); err != nil {
		t.Fatalf("evicting issue: %v", err)
	}
	if c.Count() != MaxAgents {
		t.Fatalf("after eviction Count=%d want %d", c.Count(), MaxAgents)
	}
}
