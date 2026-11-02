package controller

import (
	"bytes"
	"testing"
	"time"

	"wantasticmesh/internal/wmproto"
)

var (
	ctlNet   = wmproto.NetworkID{1, 2, 3, 4, 5, 6, 7, 8}
	ctlSelf  = wmproto.NodeID{0xc0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 1}
	ctlAgent = wmproto.NodeID{0xa0, 1, 2, 3, 4, 5, 6, 7, 8, 9, 10, 11, 12, 13, 14, 15}
	ctlKey   = bytes.Repeat([]byte{0x7e}, wmproto.KeySize)
)

// seqReader yields distinct non-zero bytes so each challenge differs.
type seqReader struct{ n byte }

func (r *seqReader) Read(p []byte) (int, error) {
	for i := range p {
		r.n++
		p[i] = r.n
	}
	return len(p), nil
}

func TestNewValidation(t *testing.T) {
	if _, err := New(wmproto.NetworkID{}, ctlSelf, nil); err != ErrIdentity {
		t.Fatalf("empty network: got %v want ErrIdentity", err)
	}
	if _, err := New(ctlNet, wmproto.NodeID{}, nil); err != ErrIdentity {
		t.Fatalf("empty self: got %v want ErrIdentity", err)
	}
}

func TestAddAgentDuplicateAndCapacity(t *testing.T) {
	c, _ := New(ctlNet, ctlSelf, &seqReader{})
	if err := c.AddAgent(ctlAgent, 1, ctlKey); err != nil {
		t.Fatalf("first add: %v", err)
	}
	if err := c.AddAgent(ctlAgent, 1, ctlKey); err != ErrDuplicate {
		t.Fatalf("duplicate: got %v want ErrDuplicate", err)
	}
	// Fill to capacity with distinct ids.
	c2, _ := New(ctlNet, ctlSelf, &seqReader{})
	for i := 0; i < MaxAgents; i++ {
		id := ctlAgent
		id[0] = byte(i)
		id[1] = byte(i >> 8)
		if err := c2.AddAgent(id, 1, ctlKey); err != nil {
			t.Fatalf("fill %d: %v", i, err)
		}
	}
	over := ctlAgent
	over[0], over[2] = 0xff, 0xff
	if err := c2.AddAgent(over, 1, ctlKey); err != ErrCapacity {
		t.Fatalf("over capacity: got %v want ErrCapacity", err)
	}
}

func TestBuildQueryUnknownAgent(t *testing.T) {
	c, _ := New(ctlNet, ctlSelf, &seqReader{})
	var buf [wmproto.MaxDatagram]byte
	if _, err := c.BuildQuery(ctlAgent, buf[:], time.Unix(1, 0)); err != ErrUnknownAgent {
		t.Fatalf("unknown agent: got %v want ErrUnknownAgent", err)
	}
}

func TestHandleInboundUnknownAgent(t *testing.T) {
	c, _ := New(ctlNet, ctlSelf, &seqReader{})
	// A well-formed-length datagram whose sender is not configured.
	data := make([]byte, wmproto.HeaderSize+wmproto.TagSize)
	copy(data[16:32], ctlAgent[:])
	var ack [wmproto.MaxDatagram]byte
	if _, _, _, err := c.HandleInbound(data, ack[:], time.Unix(1, 0)); err != ErrUnknownAgent {
		t.Fatalf("unknown sender: got %v want ErrUnknownAgent", err)
	}
}

func TestHandleInboundShortDatagram(t *testing.T) {
	c, _ := New(ctlNet, ctlSelf, &seqReader{})
	var ack [wmproto.MaxDatagram]byte
	if _, _, _, err := c.HandleInbound(make([]byte, 10), ack[:], time.Unix(1, 0)); err != wmproto.ErrLength {
		t.Fatalf("short datagram: got %v want ErrLength", err)
	}
}

func TestQueryThenAcceptReport(t *testing.T) {
	c, _ := New(ctlNet, ctlSelf, &seqReader{})
	if err := c.AddAgent(ctlAgent, 5, ctlKey); err != nil {
		t.Fatal(err)
	}
	t0 := time.Unix(1000, 0)
	if !c.NeedsQuery(ctlAgent, t0) {
		t.Fatal("fresh agent should need a query")
	}
	var qbuf [wmproto.MaxDatagram]byte
	qn, err := c.BuildQuery(ctlAgent, qbuf[:], t0)
	if err != nil {
		t.Fatalf("BuildQuery: %v", err)
	}
	if c.NeedsQuery(ctlAgent, t0) {
		t.Fatal("agent should not need a query while one is pending")
	}

	// Build a matching report with the agent's codec (same key/keyid).
	codec, _ := wmproto.NewCodec(ctlNet, 5, ctlKey)
	qh, _, err := codec.Decode(qbuf[:qn], ctlSelf)
	if err != nil {
		t.Fatalf("agent decode query: %v", err)
	}
	rb := &wmproto.ReportBody{NumLinks: 1}
	rb.Links[0] = wmproto.LinkRecord{Peer: [6]byte{0x02, 1, 2, 3, 4, 5}, Confidence: 10}
	var body [wmproto.MaxBody]byte
	enc, _ := wmproto.AppendReport(body[:0], rb)
	rh := wmproto.Header{Type: wmproto.Report, Flags: wmproto.CompleteLinks, Network: ctlNet, Sender: ctlAgent, Boot: wmproto.BootID{1}, Exchange: qh.Exchange, Sequence: 1, LeaseSeconds: wmproto.ReportLease, KeyID: 5}
	var rbuf [wmproto.MaxDatagram]byte
	rn, err := codec.Encode(rbuf[:], rh, enc)
	if err != nil {
		t.Fatalf("encode report: %v", err)
	}

	var ack [wmproto.MaxDatagram]byte
	sender, ok, ackOut, err := c.HandleInbound(rbuf[:rn], ack[:], t0)
	if err != nil || !ok || sender != ctlAgent || ackOut == nil {
		t.Fatalf("accept report: err=%v ok=%v sender=%x ack=%v", err, ok, sender, ackOut != nil)
	}
	if c.NodeCount() != 1 {
		t.Fatalf("NodeCount=%d want 1", c.NodeCount())
	}
	var nodes [MaxAgents]Node
	if n := c.Nodes(nodes[:]); n != 1 || nodes[0].NumLinks != 1 {
		t.Fatalf("Nodes n=%d links=%d", n, nodes[0].NumLinks)
	}

	// The Ack must authenticate under the agent's codec as from the controller.
	if _, _, err := codec.Decode(ackOut, ctlSelf); err != nil {
		t.Fatalf("decode ack: %v", err)
	}

	// Replaying the same report must fail: the challenge is consumed.
	if _, _, _, err := c.HandleInbound(rbuf[:rn], ack[:], t0); err != wmproto.ErrChallenge {
		t.Fatalf("replay: got %v want ErrChallenge", err)
	}
}

func TestMaintainFlagsStale(t *testing.T) {
	c, _ := New(ctlNet, ctlSelf, &seqReader{})
	c.SetStaleTTL(time.Minute)
	_ = c.AddAgent(ctlAgent, 1, ctlKey)
	// Inject a node by accepting a report.
	t0 := time.Unix(2000, 0)
	var qbuf [wmproto.MaxDatagram]byte
	qn, _ := c.BuildQuery(ctlAgent, qbuf[:], t0)
	codec, _ := wmproto.NewCodec(ctlNet, 1, ctlKey)
	qh, _, _ := codec.Decode(qbuf[:qn], ctlSelf)
	rh := wmproto.Header{Type: wmproto.Report, Network: ctlNet, Sender: ctlAgent, Boot: wmproto.BootID{1}, Exchange: qh.Exchange, Sequence: 1, LeaseSeconds: wmproto.ReportLease, KeyID: 1}
	var rbody [wmproto.MaxBody]byte
	emptyReport, _ := wmproto.AppendReport(rbody[:0], &wmproto.ReportBody{}) // a report with zero records is the 2 count bytes, not an empty body
	var rbuf [wmproto.MaxDatagram]byte
	rn, _ := codec.Encode(rbuf[:], rh, emptyReport)
	var ack [wmproto.MaxDatagram]byte
	if _, _, _, err := c.HandleInbound(rbuf[:rn], ack[:], t0); err != nil {
		t.Fatalf("seed report: %v", err)
	}
	c.Maintain(t0.Add(2 * time.Minute))
	var nodes [MaxAgents]Node
	c.Nodes(nodes[:])
	if !nodes[0].Stale {
		t.Fatal("node past TTL should be flagged stale but retained")
	}
	if c.NodeCount() != 1 {
		t.Fatal("stale node must be retained as last-known-good")
	}
}
