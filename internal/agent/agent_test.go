package agent_test

import (
	"bytes"
	"testing"
	"time"

	"wantasticmesh/internal/agent"
	"wantasticmesh/internal/controller"
	"wantasticmesh/internal/wmproto"
)

var (
	netID   = wmproto.NetworkID{9, 8, 7, 6, 5, 4, 3, 2}
	ctrlID  = wmproto.NodeID{0xc0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 2}
	agentID = wmproto.NodeID{0xa1, 1, 2, 3, 4, 5, 6, 7, 8, 9, 10, 11, 12, 13, 14, 15}
	key     = bytes.Repeat([]byte{0x33}, wmproto.KeySize)
)

type seqReader struct{ n byte }

func (r *seqReader) Read(p []byte) (int, error) {
	for i := range p {
		r.n++
		p[i] = r.n
	}
	return len(p), nil
}

type fakeNeighbors struct{ n int }

func (f fakeNeighbors) Links(dst []wmproto.LinkRecord, now time.Time) int {
	for i := 0; i < f.n && i < len(dst); i++ {
		dst[i] = wmproto.LinkRecord{LocalRadio: 0, Peer: [6]byte{0x02, byte(i + 1)}, Confidence: 20, Flags: 1}
	}
	return f.n
}

func newAgent(t *testing.T, neigh agent.NeighborSource) *agent.Agent {
	t.Helper()
	ag, err := agent.New(agent.Config{
		Network:    netID,
		Self:       agentID,
		Controller: ctrlID,
		KeyID:      3,
		Key:        key,
		Neighbors:  neigh,
		RNG:        &seqReader{n: 100},
	})
	if err != nil {
		t.Fatalf("agent.New: %v", err)
	}
	return ag
}

func newController(t *testing.T) *controller.Controller {
	t.Helper()
	c, err := controller.New(netID, ctrlID, &seqReader{})
	if err != nil {
		t.Fatalf("controller.New: %v", err)
	}
	if err := c.AddAgent(agentID, 3, key); err != nil {
		t.Fatalf("AddAgent: %v", err)
	}
	return c
}

func TestAgentNewValidation(t *testing.T) {
	if _, err := agent.New(agent.Config{Network: netID, Self: agentID, Neighbors: fakeNeighbors{}}); err != agent.ErrIdentity {
		t.Fatalf("missing controller: got %v want ErrIdentity", err)
	}
	if _, err := agent.New(agent.Config{Network: netID, Self: agentID, Controller: ctrlID, Key: key}); err == nil {
		t.Fatal("nil neighbor source should fail")
	}
}

// TestFullExchange runs the whole query -> report -> ack cycle between a real
// agent and a real controller with no sockets, then checks replay and reporting.
func TestFullExchange(t *testing.T) {
	c := newController(t)
	ag := newAgent(t, fakeNeighbors{n: 3})
	t0 := time.Unix(5000, 0)

	var qbuf [wmproto.MaxDatagram]byte
	qn, err := c.BuildQuery(agentID, qbuf[:], t0)
	if err != nil {
		t.Fatalf("BuildQuery: %v", err)
	}

	var rbuf [wmproto.MaxDatagram]byte
	report, err := ag.HandleInbound(qbuf[:qn], rbuf[:], t0)
	if err != nil || report == nil {
		t.Fatalf("agent handle query: err=%v report=%v", err, report != nil)
	}
	if !ag.ControllerAlive() {
		t.Fatal("agent should mark controller alive after a query")
	}

	var ackbuf [wmproto.MaxDatagram]byte
	sender, ok, ack, err := c.HandleInbound(report, ackbuf[:], t0)
	if err != nil || !ok || sender != agentID || ack == nil {
		t.Fatalf("controller accept: err=%v ok=%v sender=%x ack=%v", err, ok, sender, ack != nil)
	}
	var nodes [controller.MaxAgents]controller.Node
	if n := c.Nodes(nodes[:]); n != 1 || nodes[0].NumLinks != 3 {
		t.Fatalf("graph: n=%d links=%d want 1/3", n, nodes[0].NumLinks)
	}

	// Agent consumes the Ack.
	var tmp [wmproto.MaxDatagram]byte
	out, err := ag.HandleInbound(ack, tmp[:], t0)
	if err != nil || out != nil {
		t.Fatalf("agent handle ack: err=%v out=%v", err, out != nil)
	}

	// Replaying the captured report fails after the challenge is consumed.
	if _, _, _, err := c.HandleInbound(report, ackbuf[:], t0); err != wmproto.ErrChallenge {
		t.Fatalf("replay: got %v want ErrChallenge", err)
	}

	// A fresh query increments the report sequence and is accepted again.
	qn2, _ := c.BuildQuery(agentID, qbuf[:], t0.Add(time.Second))
	report2, err := ag.HandleInbound(qbuf[:qn2], rbuf[:], t0.Add(time.Second))
	if err != nil {
		t.Fatalf("second report: %v", err)
	}
	if _, _, _, err := c.HandleInbound(report2, ackbuf[:], t0.Add(time.Second)); err != nil {
		t.Fatalf("accept second report: %v", err)
	}
	c.Nodes(nodes[:])
	if nodes[0].Sequence != 2 {
		t.Fatalf("report sequence=%d want 2 after a second report", nodes[0].Sequence)
	}
}

func TestAgentRejectsWrongSender(t *testing.T) {
	c := newController(t)
	ag := newAgent(t, fakeNeighbors{n: 1})
	t0 := time.Unix(6000, 0)

	// Build a query, then corrupt the sender field so it is not the controller.
	var qbuf [wmproto.MaxDatagram]byte
	qn, _ := c.BuildQuery(agentID, qbuf[:], t0)
	qbuf[16] ^= 0xff // mangle sender; also breaks the tag
	var rbuf [wmproto.MaxDatagram]byte
	if _, err := ag.HandleInbound(qbuf[:qn], rbuf[:], t0); err == nil {
		t.Fatal("agent accepted a datagram with a tampered sender")
	}
}

func TestAnnounceRoundTrip(t *testing.T) {
	c := newController(t)
	ag := newAgent(t, fakeNeighbors{n: 0})
	t0 := time.Unix(7000, 0)
	var abuf [wmproto.MaxDatagram]byte
	an, err := ag.Announce(abuf[:], t0)
	if err != nil {
		t.Fatalf("Announce: %v", err)
	}
	var ack [wmproto.MaxDatagram]byte
	sender, ok, out, err := c.HandleInbound(abuf[:an], ack[:], t0)
	if err != nil || !ok || sender != agentID || out != nil {
		t.Fatalf("controller handle announce: err=%v ok=%v sender=%x out=%v", err, ok, sender, out != nil)
	}
}

func TestAgentBoot(t *testing.T) {
	ag := newAgent(t, fakeNeighbors{n: 1})
	if ag.Boot() == (wmproto.BootID{}) {
		t.Fatal("boot id must be non-zero")
	}
}
