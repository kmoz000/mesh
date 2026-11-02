package sim

import (
	"encoding/binary"
	"math/rand"
	"time"

	"wantasticmesh/internal/wmproto"
)

// simNet is the single network all simulated agents share. It is non-zero as
// wmproto requires.
var simNet = wmproto.NetworkID{0x57, 0x4d, 0x53, 0x48, 1, 2, 3, 4}

// keyID is shared by every agent; each agent still holds a distinct key, so a
// datagram only authenticates under its own codec. Mirrors the wmproto tests.
const keyID = 7

// Node is one simulated agent. It owns its identity, its per-agent codec, a
// monotonic per-boot sequence and the set of peers it observes. makeReport and
// the scratch buffers allocate nothing on the modeled hot path: the real
// daemon runs under TinyGo's leaking GC, so reports reuse fixed backing arrays.
type Node struct {
	ID    wmproto.NodeID
	Boot  wmproto.BootID
	peers []wmproto.NodeID // intended neighbors (full topology)

	codec *wmproto.Codec
	seq   uint64
	acked int // count of controller Acks this node has decoded

	bodyScratch  [wmproto.MaxReportBody]byte
	dgramScratch [wmproto.MaxDatagram]byte
}

// Acked reports how many controller Acks this node has successfully decoded.
func (n *Node) Acked() int { return n.acked }

// Reboot restarts the node: a fresh boot id and the sequence back to 0, so the
// next report carries sequence 1. The boot id must differ from the old one; a
// restart that reused the boot id would (correctly) read as a stale sequence.
func (n *Node) Reboot(newBoot wmproto.BootID) {
	n.Boot = newBoot
	n.seq = 0
}

// makeReport builds and encodes one full-snapshot Report echoing challenge. It
// includes only the peers for which include reports true, modelling that a node
// can only observe neighbors it currently has a path to; a nil include reports
// every peer. The returned slice aliases the node's scratch buffer and is valid
// only until the next makeReport; Medium.Send copies it, so that is safe here.
func (n *Node) makeReport(challenge wmproto.ExchangeID, include func(peer wmproto.NodeID) bool) ([]byte, error) {
	n.seq++
	var rb wmproto.ReportBody
	// One synthetic radio keeps the report well-formed; the graph is built
	// from links, not radios.
	rb.NumRadios = 1
	rb.Radios[0] = wmproto.RadioRecord{Index: 0, Band: 3, Channel: 36, Width: 80, MLDLink: 0xff}
	for _, p := range n.peers {
		if include != nil && !include(p) {
			continue
		}
		rb.Links[rb.NumLinks] = wmproto.LinkRecord{LocalRadio: 0, Peer: macOf(p), RSSI: -50, Confidence: 255}
		rb.NumLinks++
	}
	body, err := wmproto.AppendReport(n.bodyScratch[:0], &rb)
	if err != nil {
		return nil, err
	}
	h := wmproto.Header{
		Type:         wmproto.Report,
		Flags:        wmproto.CompleteRadios | wmproto.CompleteLinks,
		Network:      simNet,
		Sender:       n.ID,
		Boot:         n.Boot,
		Exchange:     challenge,
		Sequence:     n.seq,
		LeaseSeconds: wmproto.ReportLease,
		KeyID:        keyID,
	}
	count, err := n.codec.Encode(n.dgramScratch[:], h, body)
	if err != nil {
		return nil, err
	}
	return n.dgramScratch[:count], nil
}

// Graph is the directed adjacency the controller assembles from accepted
// reports: adj[origin] is the set of peers origin reported a link to. Links are
// directional in wmproto, so this graph is directed; a bidirectional neighbor
// pair shows as two entries.
type Graph struct {
	adj map[wmproto.NodeID]map[[6]byte]struct{}
}

func newGraph() *Graph {
	return &Graph{adj: make(map[wmproto.NodeID]map[[6]byte]struct{})}
}

// apply folds one accepted report into the graph. A CompleteLinks report is a
// full snapshot that replaces everything held for the origin (so stale edges
// disappear); otherwise the links merge into what is already known.
func (g *Graph) apply(origin wmproto.NodeID, flags uint16, rb *wmproto.ReportBody) {
	if flags&wmproto.CompleteLinks != 0 || g.adj[origin] == nil {
		g.adj[origin] = make(map[[6]byte]struct{})
	}
	set := g.adj[origin]
	for i := 0; i < rb.NumLinks; i++ {
		set[rb.Links[i].Peer] = struct{}{}
	}
}

// HasEdge reports whether origin has a directed edge to peer.
func (g *Graph) HasEdge(origin, peer wmproto.NodeID) bool {
	_, ok := g.adj[origin][macOf(peer)]
	return ok
}

// EdgeCount returns the total number of directed edges held.
func (g *Graph) EdgeCount() int {
	n := 0
	for _, set := range g.adj {
		n += len(set)
	}
	return n
}

// Outcome records how the controller handled one delivered report datagram.
type Outcome struct {
	From wmproto.NodeID
	Err  error // nil on accept; wmproto error (ErrChallenge, ErrReplay, ...) otherwise
}

// Sim ties a controller, a medium and a set of nodes into one deterministic
// exchange harness. Time is an explicit now the caller advances; Sim performs
// no I/O and is not safe for concurrent use.
type Sim struct {
	ctrlID   wmproto.NodeID
	ctrlBoot wmproto.BootID
	ctrl     *wmproto.Controller
	Medium   *Medium
	Graph    *Graph

	nodes  map[wmproto.NodeID]*Node
	codecs map[wmproto.NodeID]*wmproto.Codec
	order  []wmproto.NodeID // stable iteration order for determinism

	chalCtr uint64
	chalRng *rand.Rand

	// queryScratch is the controller's per-query encode buffer. One query is
	// encoded and handed to the medium (which copies) before the next, so a
	// single buffer is safe and keeps the controller path allocation-free.
	queryScratch [wmproto.MaxDatagram]byte
}

// NewSim returns an empty harness seeded for determinism. The medium and the
// challenge generator derive from the same seed, so a whole scenario replays
// identically.
func NewSim(seed int64) *Sim {
	return &Sim{
		ctrlID:   nodeID(0xc0ffee),
		ctrlBoot: wmproto.BootID{0xc0, 0, 0, 0, 0, 0, 0, 1},
		ctrl:     &wmproto.Controller{},
		Medium:   NewMedium(seed),
		Graph:    newGraph(),
		nodes:    make(map[wmproto.NodeID]*Node),
		codecs:   make(map[wmproto.NodeID]*wmproto.Codec),
		chalRng:  rand.New(rand.NewSource(seed + 1)),
	}
}

// AddNode registers an agent with the given id, boot and peer set and returns
// it. The agent's key is derived deterministically from its id.
func (s *Sim) AddNode(id wmproto.NodeID, boot wmproto.BootID, peers []wmproto.NodeID) *Node {
	codec, err := wmproto.NewCodec(simNet, keyID, deriveKey(id))
	if err != nil {
		panic("sim: NewCodec: " + err.Error())
	}
	n := &Node{ID: id, Boot: boot, peers: peers, codec: codec}
	s.nodes[id] = n
	s.codecs[id] = codec
	s.order = append(s.order, id)
	return n
}

// IDs returns the registered node ids in stable registration order.
func (s *Sim) IDs() []wmproto.NodeID { return s.order }

// Count exposes the controller's tracked-agent count (bounded by MaxAgents).
func (s *Sim) Count() int { return s.ctrl.Count() }

// nextChallenge returns a fresh, non-zero, non-repeating challenge. The counter
// guarantees uniqueness and non-zeroness; the rng half just keeps it from being
// trivially predictable, matching how a real controller would mint one.
func (s *Sim) nextChallenge() wmproto.ExchangeID {
	s.chalCtr++
	var e wmproto.ExchangeID
	binary.BigEndian.PutUint64(e[0:8], s.chalCtr)
	binary.BigEndian.PutUint64(e[8:16], s.chalRng.Uint64())
	return e
}

// PollAll polls every registered node once at now.
func (s *Sim) PollAll(now time.Time) []Outcome { return s.Poll(s.order, now) }

// Poll runs one full Query -> Report -> ingest round for ids at time now:
//
//  1. the controller issues a challenge per id and sends a Query through the
//     medium;
//  2. each node that receives its Query answers with a full-snapshot Report
//     back through the medium;
//  3. the controller ingests every delivered Report, authenticating it,
//     validating the exchange and folding accepted reports into the graph.
//
// On each accepted report the controller sends an Ack back to the agent
// (stage 4 delivers it), completing the handshake end to end and exercising the
// codec's Ack header rules. Every stage passes through the medium, so loss,
// duplication, reordering and partitioning all apply. The returned outcomes
// record the per-datagram result at the controller, which is how "exactly one
// accept per node" and "duplicate rejected" are asserted.
func (s *Sim) Poll(ids []wmproto.NodeID, now time.Time) []Outcome {
	// Stage 1: queries out.
	for _, id := range ids {
		n := s.nodes[id]
		if n == nil {
			continue
		}
		chal := s.nextChallenge()
		if err := s.ctrl.IssueChallenge(id, chal, now); err != nil {
			continue
		}
		q, err := s.encodeQuery(id, chal)
		if err != nil {
			continue
		}
		s.Medium.Send(Datagram{From: s.ctrlID, To: id, Bytes: q})
	}

	// Stage 2: deliver queries; each recipient answers with a report. A node
	// reports only peers it currently has a path to, via the medium.
	for _, d := range s.Medium.Drain() {
		n := s.nodes[d.To]
		if n == nil {
			continue
		}
		h, _, err := n.codec.Decode(d.Bytes, s.ctrlID)
		if err != nil {
			continue
		}
		include := func(peer wmproto.NodeID) bool { return !s.Medium.Partitioned(n.ID, peer) }
		r, err := n.makeReport(h.Exchange, include)
		if err != nil {
			continue
		}
		s.Medium.Send(Datagram{From: n.ID, To: s.ctrlID, Bytes: r})
	}

	// Stage 3: controller ingests delivered reports and Acks each acceptance.
	var outcomes []Outcome
	for _, d := range s.Medium.Drain() {
		chal, err := s.ingest(d, now)
		outcomes = append(outcomes, Outcome{From: d.From, Err: err})
		if err != nil {
			continue
		}
		a, aerr := s.encodeAck(d.From, chal)
		if aerr != nil {
			continue
		}
		s.Medium.Send(Datagram{From: s.ctrlID, To: d.From, Bytes: a})
	}

	// Stage 4: deliver Acks back to the agents that were accepted.
	for _, d := range s.Medium.Drain() {
		n := s.nodes[d.To]
		if n == nil {
			continue
		}
		if _, _, err := n.codec.Decode(d.Bytes, s.ctrlID); err == nil {
			n.acked++
		}
	}
	return outcomes
}

func (s *Sim) encodeQuery(id wmproto.NodeID, chal wmproto.ExchangeID) ([]byte, error) {
	h := wmproto.Header{
		Type:     wmproto.Query,
		Network:  simNet,
		Sender:   s.ctrlID,
		Boot:     s.ctrlBoot,
		Exchange: chal,
		KeyID:    keyID,
	}
	count, err := s.codecs[id].Encode(s.queryScratch[:], h, nil)
	if err != nil {
		return nil, err
	}
	return s.queryScratch[:count], nil
}

// encodeAck builds the controller's Ack for id echoing challenge. It reuses the
// query scratch buffer; Medium.Send copies the bytes, so that is safe.
func (s *Sim) encodeAck(id wmproto.NodeID, challenge wmproto.ExchangeID) ([]byte, error) {
	h := wmproto.Header{
		Type:     wmproto.Ack,
		Network:  simNet,
		Sender:   s.ctrlID,
		Boot:     s.ctrlBoot,
		Exchange: challenge,
		KeyID:    keyID,
	}
	count, err := s.codecs[id].Encode(s.queryScratch[:], h, nil)
	if err != nil {
		return nil, err
	}
	return s.queryScratch[:count], nil
}

// ingest authenticates one report, validates the exchange, and only on success
// folds it into the graph. The order matters: a duplicate or replayed report is
// rejected by AcceptReport before it can touch the graph, so it can never
// double-count. On success it returns the accepted challenge, which the
// controller echoes in its Ack.
func (s *Sim) ingest(d Datagram, now time.Time) (wmproto.ExchangeID, error) {
	codec := s.codecs[d.From]
	if codec == nil {
		return wmproto.ExchangeID{}, wmproto.ErrIdentity
	}
	h, body, err := codec.Decode(d.Bytes, d.From)
	if err != nil {
		return wmproto.ExchangeID{}, err
	}
	if err := s.ctrl.AcceptReport(h, now); err != nil {
		return wmproto.ExchangeID{}, err
	}
	var rb wmproto.ReportBody
	if err := wmproto.ParseReport(body, &rb); err != nil {
		return wmproto.ExchangeID{}, err
	}
	s.Graph.apply(h.Sender, h.Flags, &rb)
	return h.Exchange, nil
}

// --- deterministic identity helpers ---

// nodeID maps a small integer to a distinct, non-zero NodeID.
func nodeID(i int) wmproto.NodeID {
	var id wmproto.NodeID
	// Put the index in the leading bytes so macOf (the first six) is distinct
	// per node, matching how the daemon derives a node id from an interface MAC.
	binary.BigEndian.PutUint32(id[0:4], uint32(i)+1)
	id[4] = 0x02 // locally administered unicast; keeps the derived MAC non-zero
	id[15] = 0xA5
	return id
}

// macOf derives a node's 6-byte MAC from the first six bytes of its id, the
// same reduction the daemon applies to an observed AL MAC.
func macOf(id wmproto.NodeID) [6]byte {
	var m [6]byte
	copy(m[:], id[:6])
	return m
}

// bootID maps a small integer to a distinct, non-zero BootID.
func bootID(i int) wmproto.BootID {
	var b wmproto.BootID
	binary.BigEndian.PutUint64(b[0:8], uint64(i)*2654435761+1)
	return b
}

// deriveKey produces a stable 32-byte key from an id. Any non-degenerate key
// works; the sim is not modelling key management.
func deriveKey(id wmproto.NodeID) []byte {
	key := make([]byte, wmproto.KeySize)
	for i := range key {
		key[i] = byte(i)*31 ^ id[i%len(id)] ^ 0x5a
	}
	return key
}
