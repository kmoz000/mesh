// Package agent implements the WantasticMesh agent role: it answers a
// controller's authenticated query with a bounded report of its locally
// observed links, keeps a last-known-good neighbor view, and keeps serving
// when the controller is unreachable. It performs no I/O; the caller wires it
// to a transport. It is not safe for concurrent use.
//
// Link inference here is a documented approximation: the current neighbor
// source is the IEEE 1905 topology observer, which sees advertised AL MACs,
// not verified bidirectional radio links. Reports therefore carry low
// confidence until real per-radio telemetry (nl80211/vendor) is wired in.
package agent

import (
	"crypto/rand"
	"errors"
	"io"
	"time"

	"wantasticmesh/internal/wmproto"
)

var (
	// ErrIdentity is returned for an empty self or controller id, or network.
	ErrIdentity = errors.New("agent: empty identity")
	// ErrNotForUs is returned when an inbound datagram is not from the
	// configured controller.
	ErrNotForUs = errors.New("agent: datagram not from controller")
)

// NeighborSource yields the agent's currently observed neighbor links. It fills
// dst (bounded by the caller to wmproto.MaxLinks) and returns the count. It must
// not allocate and must be cheap; it is called once per report.
type NeighborSource interface {
	Links(dst []wmproto.LinkRecord, now time.Time) int
}

// RadioSource yields the agent's radio records, or 0 when no radio telemetry is
// available yet. Returning 0 is honest: the daemon makes no radio claims until
// a verified telemetry adapter exists.
type RadioSource interface {
	Radios(dst []wmproto.RadioRecord, now time.Time) int
}

// PositionSource yields the agent's current GPS position record. ok is false
// when GPS reporting is disabled (no record is sent); when enabled it returns a
// record each report, with PositionValid set only on an actual fix, so the
// controller distinguishes "no GPS" from "GPS present, no lock".
type PositionSource interface {
	Position(now time.Time) (wmproto.PositionRecord, bool)
}

// Agent holds one agent's identity, codec, and liveness state.
type Agent struct {
	self       wmproto.NodeID
	controller wmproto.NodeID
	network    wmproto.NetworkID
	codec      *wmproto.Codec
	keyID      uint16
	boot       wmproto.BootID
	seq        uint64

	neighbors NeighborSource
	radios    RadioSource
	position  PositionSource

	controllerAlive bool
	lastContact     time.Time

	// Inbound replay guard for controller-originated Query/Ack, scoped to the
	// controller's boot: a datagram at or below the last accepted sequence for
	// the current boot is a replay. A new boot resets the window.
	ctrlBoot wmproto.BootID
	ctrlSeq  uint64
	haveCtrl bool
}

// Config carries the agent's identity and shared key with its controller.
type Config struct {
	Network    wmproto.NetworkID
	Self       wmproto.NodeID
	Controller wmproto.NodeID
	KeyID      uint16
	Key        []byte // 32 bytes
	Neighbors  NeighborSource
	Radios     RadioSource    // optional; nil means no radio records
	Position   PositionSource // optional; nil means GPS reporting disabled
	RNG        io.Reader      // nil uses crypto/rand for the boot id
}

// New builds an agent. It draws a random per-process boot id so a restart is
// distinguishable from a replay and resets the report sequence.
func New(cfg Config) (*Agent, error) {
	if cfg.Network == (wmproto.NetworkID{}) || cfg.Self == (wmproto.NodeID{}) || cfg.Controller == (wmproto.NodeID{}) {
		return nil, ErrIdentity
	}
	if cfg.Neighbors == nil {
		return nil, errors.New("agent: nil neighbor source")
	}
	codec, err := wmproto.NewCodec(cfg.Network, cfg.KeyID, cfg.Key)
	if err != nil {
		return nil, err
	}
	rng := cfg.RNG
	if rng == nil {
		rng = rand.Reader
	}
	var boot wmproto.BootID
	if _, err := io.ReadFull(rng, boot[:]); err != nil {
		return nil, err
	}
	if boot == (wmproto.BootID{}) {
		boot[0] = 1 // boot id must be non-zero
	}
	return &Agent{
		self:       cfg.Self,
		controller: cfg.Controller,
		network:    cfg.Network,
		codec:      codec,
		keyID:      cfg.KeyID,
		boot:       boot,
		neighbors:  cfg.Neighbors,
		radios:     cfg.Radios,
		position:   cfg.Position,
	}, nil
}

// Boot returns the agent's boot id for this process.
func (a *Agent) Boot() wmproto.BootID { return a.boot }

// ControllerAlive reports whether the controller has been heard from recently.
func (a *Agent) ControllerAlive() bool { return a.controllerAlive }

// Announce builds an unsolicited announce datagram into dst so the controller
// can learn the agent's address and liveness. It returns the datagram length.
func (a *Agent) Announce(dst []byte, now time.Time) (int, error) {
	a.seq++
	h := wmproto.Header{
		Type:     wmproto.Announce,
		Network:  a.network,
		Sender:   a.self,
		Boot:     a.boot,
		Sequence: a.seq,
		KeyID:    a.keyID,
	}
	return a.codec.Encode(dst, h, nil)
}

// HandleInbound authenticates one datagram from the controller. On a Query it
// builds and returns a Report datagram into dst; on an Ack it records controller
// liveness and returns no datagram. out is nil when there is nothing to send.
// dst must be at least wmproto.MaxDatagram bytes and backs out.
func (a *Agent) HandleInbound(data, dst []byte, now time.Time) (out []byte, err error) {
	h, _, decErr := a.codec.Decode(data, a.controller)
	if decErr != nil {
		return nil, decErr
	}
	// Reject a replayed controller message within the same boot. A different
	// boot id is a legitimate controller restart and resets the window.
	if a.haveCtrl && h.Boot == a.ctrlBoot && h.Sequence <= a.ctrlSeq {
		return nil, wmproto.ErrReplay
	}
	a.ctrlBoot = h.Boot
	a.ctrlSeq = h.Sequence
	a.haveCtrl = true
	switch h.Type {
	case wmproto.Query:
		a.controllerAlive = true
		a.lastContact = now
		n, buildErr := a.buildReport(h.Exchange, dst, now)
		if buildErr != nil {
			return nil, buildErr
		}
		return dst[:n], nil
	case wmproto.Ack:
		a.controllerAlive = true
		a.lastContact = now
		return nil, nil
	default:
		return nil, wmproto.ErrType
	}
}

// buildReport fills a bounded ReportBody from the neighbor and radio sources and
// encodes a Report echoing the controller's challenge. v0 always sends the full
// bounded set (CompleteRadios|CompleteLinks); deltas are a later optimization,
// so a report sent when the controller returns is already a full snapshot.
func (a *Agent) buildReport(challenge wmproto.ExchangeID, dst []byte, now time.Time) (int, error) {
	var rb wmproto.ReportBody
	rb.NumLinks = a.neighbors.Links(rb.Links[:], now)
	if rb.NumLinks > wmproto.MaxLinks {
		rb.NumLinks = wmproto.MaxLinks
	}
	if a.radios != nil {
		rb.NumRadios = a.radios.Radios(rb.Radios[:], now)
		if rb.NumRadios > wmproto.MaxRadios {
			rb.NumRadios = wmproto.MaxRadios
		}
	}
	if a.position != nil {
		if rec, ok := a.position.Position(now); ok {
			rb.Positions[0] = rec
			rb.NumPositions = 1
		}
	}
	var body [wmproto.MaxBody]byte
	encoded, err := wmproto.AppendReport(body[:0], &rb)
	if err != nil {
		return 0, err
	}
	a.seq++
	h := wmproto.Header{
		Type:         wmproto.Report,
		Flags:        wmproto.CompleteRadios | wmproto.CompleteLinks,
		Network:      a.network,
		Sender:       a.self,
		Boot:         a.boot,
		Exchange:     challenge,
		Sequence:     a.seq,
		LeaseSeconds: wmproto.ReportLease,
		KeyID:        a.keyID,
	}
	return a.codec.Encode(dst, h, encoded)
}
