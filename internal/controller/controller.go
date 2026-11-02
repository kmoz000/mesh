// Package controller implements the WantasticMesh controller role: it issues
// authenticated query challenges to its configured agents, accepts their
// reports through wmproto's replay-protected exchange state, and assembles a
// bounded last-known-good adjacency view. It performs no I/O; the caller wires
// it to a transport. It is not safe for concurrent use (it reuses one HMAC
// state per agent codec and a single exchange table).
package controller

import (
	"crypto/rand"
	"errors"
	"io"
	"time"

	"wantasticmesh/internal/wmproto"
)

// MaxAgents bounds both the configured-agent list and the node view.
const MaxAgents = wmproto.MaxAgents

// DefaultStaleTTL is how long a node's last report is retained as last-known-good
// before it is flagged stale. The entry is kept (for recovery), not dropped.
const DefaultStaleTTL = 5 * time.Minute

var (
	// ErrCapacity is returned when more than MaxAgents agents are configured.
	ErrCapacity = errors.New("controller: agent capacity exceeded")
	// ErrDuplicate is returned when the same agent id is configured twice.
	ErrDuplicate = errors.New("controller: duplicate agent id")
	// ErrUnknownAgent is returned by BuildQuery for an id that is not configured.
	ErrUnknownAgent = errors.New("controller: unknown agent id")
	// ErrIdentity is returned for an empty self id or network.
	ErrIdentity = errors.New("controller: empty identity")
)

type agentCfg struct {
	used   bool
	id     wmproto.NodeID
	keyID  uint16
	codec  *wmproto.Codec
	outSeq uint64 // controller's own monotonic sequence for Query/Ack to this agent
}

// Node is a decoded, read-only view of one agent's latest accepted report.
type Node struct {
	ID          wmproto.NodeID
	Boot        wmproto.BootID
	Sequence    uint64
	Radios      [wmproto.MaxRadios]wmproto.RadioRecord
	NumRadios   int
	Links       [wmproto.MaxLinks]wmproto.LinkRecord
	NumLinks    int
	Position    wmproto.PositionRecord
	HasPosition bool
	LastSeen    time.Time
	Stale       bool
}

type nodeRecord struct {
	used bool
	Node
}

// Controller holds configured agents and the assembled view.
type Controller struct {
	self     wmproto.NodeID
	network  wmproto.NetworkID
	boot     wmproto.BootID
	rng      io.Reader
	staleTTL time.Duration

	ex      wmproto.Controller
	agents  [MaxAgents]agentCfg
	nAgents int
	nodes   [MaxAgents]nodeRecord
}

// New builds a controller for one network with the given self id. rng supplies
// unpredictable challenge bytes; pass nil to use crypto/rand.
func New(network wmproto.NetworkID, self wmproto.NodeID, rng io.Reader) (*Controller, error) {
	if network == (wmproto.NetworkID{}) || self == (wmproto.NodeID{}) {
		return nil, ErrIdentity
	}
	if rng == nil {
		rng = rand.Reader
	}
	// A fresh random boot id per run is required for GCM nonce safety: the
	// controller's Query/Ack subkey is bound to (direction, self, boot), so a
	// restart must change boot or a reset sequence counter would reuse a nonce.
	var boot wmproto.BootID
	if _, err := io.ReadFull(rng, boot[:]); err != nil {
		return nil, err
	}
	if boot == (wmproto.BootID{}) {
		boot[0] = 1
	}
	return &Controller{self: self, network: network, boot: boot, rng: rng, staleTTL: DefaultStaleTTL}, nil
}

// SetStaleTTL overrides how long a node is retained before being flagged stale.
func (c *Controller) SetStaleTTL(d time.Duration) {
	if d > 0 {
		c.staleTTL = d
	}
}

// AddAgent registers an agent id and its shared key. keyID and key identify the
// per-agent HMAC key; a distinct random 32-byte key per agent is required, as a
// shared group key would let any member forge another's reports.
func (c *Controller) AddAgent(id wmproto.NodeID, keyID uint16, key []byte) error {
	if id == (wmproto.NodeID{}) {
		return ErrIdentity
	}
	codec, err := wmproto.NewCodec(c.network, keyID, key)
	if err != nil {
		return err
	}
	free := -1
	for i := range c.agents {
		if c.agents[i].used {
			if c.agents[i].id == id {
				return ErrDuplicate
			}
		} else if free < 0 {
			free = i
		}
	}
	if free < 0 {
		return ErrCapacity
	}
	c.agents[free] = agentCfg{used: true, id: id, keyID: keyID, codec: codec}
	c.nAgents++
	return nil
}

func (c *Controller) findAgent(id wmproto.NodeID) *agentCfg {
	for i := range c.agents {
		if c.agents[i].used && c.agents[i].id == id {
			return &c.agents[i]
		}
	}
	return nil
}

// Self returns the controller's node id.
func (c *Controller) Self() wmproto.NodeID { return c.self }

// senderOf reads the unauthenticated sender field. It only selects a candidate
// key; Decode then authenticates that the sender is genuine, so a spoofed field
// merely picks a key that will not verify.
func senderOf(data []byte) (wmproto.NodeID, bool) {
	if len(data) < wmproto.HeaderSize+wmproto.TagSize {
		return wmproto.NodeID{}, false
	}
	var id wmproto.NodeID
	copy(id[:], data[16:32])
	return id, true
}

// HandleInbound authenticates one datagram and, for a Report, accepts it into
// the view and returns an Ack datagram to send back to the source. ack is nil
// when the message needs no reply. sender is the authenticated sender when ok
// is true. ackBuf must be at least wmproto.MaxDatagram bytes and backs ack.
func (c *Controller) HandleInbound(data, ackBuf []byte, now time.Time) (sender wmproto.NodeID, ok bool, ack []byte, err error) {
	claimed, good := senderOf(data)
	if !good {
		return wmproto.NodeID{}, false, nil, wmproto.ErrLength
	}
	cfg := c.findAgent(claimed)
	if cfg == nil {
		return wmproto.NodeID{}, false, nil, ErrUnknownAgent
	}
	h, body, decErr := cfg.codec.Decode(data, claimed)
	if decErr != nil {
		return wmproto.NodeID{}, false, nil, decErr
	}
	switch h.Type {
	case wmproto.Report:
		if err := c.ex.AcceptReport(h, now); err != nil {
			return h.Sender, true, nil, err
		}
		if err := c.storeReport(h, body, now); err != nil {
			return h.Sender, true, nil, err
		}
		n, ackErr := c.buildAck(cfg, h.Exchange, ackBuf)
		if ackErr != nil {
			return h.Sender, true, nil, ackErr
		}
		return h.Sender, true, ackBuf[:n], nil
	case wmproto.Announce:
		// Announce carries liveness only; refresh last-seen without a reply.
		if rec := c.find(h.Sender); rec != nil {
			rec.LastSeen = now
			rec.Stale = false
		}
		return h.Sender, true, nil, nil
	default:
		return h.Sender, true, nil, wmproto.ErrType
	}
}

func (c *Controller) buildAck(cfg *agentCfg, challenge wmproto.ExchangeID, dst []byte) (int, error) {
	cfg.outSeq++
	h := wmproto.Header{
		Type:     wmproto.Ack,
		Network:  c.network,
		Sender:   c.self,
		Boot:     c.boot,
		Exchange: challenge,
		Sequence: cfg.outSeq,
		KeyID:    cfg.keyID,
	}
	return cfg.codec.Encode(dst, h, nil)
}

func (c *Controller) storeReport(h wmproto.Header, body []byte, now time.Time) error {
	var rb wmproto.ReportBody
	if err := wmproto.ParseReport(body, &rb); err != nil {
		return err
	}
	rec := c.slotFor(h.Sender, now)
	if rec == nil {
		return ErrCapacity
	}
	rec.used = true
	rec.ID = h.Sender
	rec.Boot = h.Boot
	rec.Sequence = h.Sequence
	rec.Radios = rb.Radios
	rec.NumRadios = rb.NumRadios
	rec.Links = rb.Links
	rec.NumLinks = rb.NumLinks
	if rb.NumPositions > 0 {
		rec.Position = rb.Positions[0]
		rec.HasPosition = true
	} else {
		rec.Position = wmproto.PositionRecord{}
		rec.HasPosition = false
	}
	rec.LastSeen = now
	rec.Stale = false
	return nil
}

func (c *Controller) find(id wmproto.NodeID) *Node {
	for i := range c.nodes {
		if c.nodes[i].used && c.nodes[i].ID == id {
			return &c.nodes[i].Node
		}
	}
	return nil
}

// slotFor returns the record for id, a free record, or the oldest to evict.
func (c *Controller) slotFor(id wmproto.NodeID, now time.Time) *nodeRecord {
	free, oldest := -1, -1
	for i := range c.nodes {
		r := &c.nodes[i]
		if !r.used {
			if free < 0 {
				free = i
			}
			continue
		}
		if r.ID == id {
			return r
		}
		if oldest < 0 || r.LastSeen.Before(c.nodes[oldest].LastSeen) {
			oldest = i
		}
	}
	if free >= 0 {
		c.nodes[free] = nodeRecord{}
		return &c.nodes[free]
	}
	if oldest >= 0 {
		c.nodes[oldest] = nodeRecord{}
		return &c.nodes[oldest]
	}
	return nil
}

// NeedsQuery reports whether agent id has no outstanding unexpired challenge.
func (c *Controller) NeedsQuery(id wmproto.NodeID, now time.Time) bool {
	return !c.ex.Pending(id, now)
}

// BuildQuery issues a fresh challenge for id and encodes a Query datagram into
// dst, returning its length. dst must be at least wmproto.HeaderSize+TagSize
// bytes. It fails for an unknown id or if the challenge cannot be issued.
func (c *Controller) BuildQuery(id wmproto.NodeID, dst []byte, now time.Time) (int, error) {
	cfg := c.findAgent(id)
	if cfg == nil {
		return 0, ErrUnknownAgent
	}
	var chal wmproto.ExchangeID
	if _, err := io.ReadFull(c.rng, chal[:]); err != nil {
		return 0, err
	}
	if chal == (wmproto.ExchangeID{}) {
		chal[0] = 1 // never issue the zero challenge
	}
	if err := c.ex.IssueChallenge(id, chal, now); err != nil {
		return 0, err
	}
	cfg.outSeq++
	h := wmproto.Header{
		Type:     wmproto.Query,
		Network:  c.network,
		Sender:   c.self,
		Boot:     c.boot,
		Exchange: chal,
		Sequence: cfg.outSeq,
		KeyID:    cfg.keyID,
	}
	return cfg.codec.Encode(dst, h, nil)
}

// Maintain flags nodes not seen within the stale TTL as stale, keeping their
// last-known-good state for recovery rather than deleting it.
func (c *Controller) Maintain(now time.Time) {
	for i := range c.nodes {
		r := &c.nodes[i]
		if r.used && !r.Stale && now.Sub(r.LastSeen) > c.staleTTL {
			r.Stale = true
		}
	}
}

// Agents returns the configured agent ids into dst and the count written.
func (c *Controller) Agents(dst []wmproto.NodeID) int {
	n := 0
	for i := range c.agents {
		if c.agents[i].used && n < len(dst) {
			dst[n] = c.agents[i].id
			n++
		}
	}
	return n
}

// Nodes copies the current node view into dst and returns the count written.
func (c *Controller) Nodes(dst []Node) int {
	n := 0
	for i := range c.nodes {
		if c.nodes[i].used && n < len(dst) {
			dst[n] = c.nodes[i].Node
			n++
		}
	}
	return n
}

// NodeCount returns the number of nodes currently in the view.
func (c *Controller) NodeCount() int {
	n := 0
	for i := range c.nodes {
		if c.nodes[i].used {
			n++
		}
	}
	return n
}
