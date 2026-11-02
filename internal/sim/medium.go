// Package sim is a deterministic, in-memory simulation harness for the
// WantasticMesh v0 exchange and topology layer. It carries wmproto datagrams
// between simulated nodes and a controller with configurable, seeded loss,
// reordering, duplication and partitioning, so adverse-condition behaviour can
// be validated without sockets, goroutines or real time. All timing is driven
// by an explicit now that the caller advances; nothing here performs I/O.
//
// This package is test scaffolding for validation gate 4 of
// research/PROTOCOL-DESIGN.md ("Emulate 128 nodes across chains, rings,
// diamonds, dense neighborhoods, partitions, reordering, loss, replay and
// simultaneous reboot"). It models the wmproto message layer only: there is no
// radio, hardware, device or credential logic of any kind.
package sim

import (
	"math/rand"

	"wantasticmesh/internal/wmproto"
)

// Datagram is one authenticated wmproto datagram in flight from From to To.
// Bytes is a complete, encoded datagram as produced by wmproto.Codec.Encode.
type Datagram struct {
	From  wmproto.NodeID
	To    wmproto.NodeID
	Bytes []byte
}

// Medium carries datagrams between nodes with deterministic impairments. A
// single seeded *rand.Rand drives every probabilistic choice, so a given seed
// and call sequence always produce the same outcome. Medium performs no real
// timing: callers Send datagrams, then Drain the delivered set when they are
// ready to process it. Medium is not safe for concurrent use.
type Medium struct {
	rng *rand.Rand

	// LossProb drops a datagram outright; DupProb delivers a second copy;
	// Reorder shuffles each drained batch. All are applied deterministically.
	LossProb float64
	DupProb  float64
	Reorder  bool

	part     map[wmproto.NodeID]int // partition id per node; absent == 0
	inflight []Datagram

	// Observability for assertions.
	PartitionDrops int
	LossDrops      int
	Dups           int
}

// NewMedium returns a clean Medium seeded for determinism.
func NewMedium(seed int64) *Medium {
	return &Medium{
		rng:  rand.New(rand.NewSource(seed)),
		part: make(map[wmproto.NodeID]int),
	}
}

// SetPartition places id in partition p. Datagrams between nodes in different
// partitions are dropped, modelling "no path". The zero value is partition 0,
// so an unset node (including the controller) is in partition 0.
func (m *Medium) SetPartition(id wmproto.NodeID, p int) { m.part[id] = p }

// Heal removes all partitioning, restoring a single connected medium.
func (m *Medium) Heal() { m.part = make(map[wmproto.NodeID]int) }

// Partitioned reports whether a and b are on opposite sides of a partition.
func (m *Medium) Partitioned(a, b wmproto.NodeID) bool {
	return m.part[a] != m.part[b]
}

// Send offers one datagram to the medium. It returns false if the datagram was
// dropped (cross-partition or lost); otherwise it is buffered for the next
// Drain, possibly with a duplicate. The bytes are copied, so the caller may
// reuse its encode buffer immediately.
func (m *Medium) Send(d Datagram) bool {
	if m.Partitioned(d.From, d.To) {
		m.PartitionDrops++
		return false
	}
	if m.LossProb > 0 && m.rng.Float64() < m.LossProb {
		m.LossDrops++
		return false
	}
	m.inflight = append(m.inflight, copyDatagram(d))
	if m.DupProb > 0 && m.rng.Float64() < m.DupProb {
		m.inflight = append(m.inflight, copyDatagram(d))
		m.Dups++
	}
	return true
}

// Drain returns and clears the buffered datagrams. When Reorder is set the
// batch is shuffled with the seeded rng, so delivery order is adverse but
// reproducible.
func (m *Medium) Drain() []Datagram {
	out := m.inflight
	m.inflight = nil
	if m.Reorder && len(out) > 1 {
		m.rng.Shuffle(len(out), func(i, j int) { out[i], out[j] = out[j], out[i] })
	}
	return out
}

func copyDatagram(d Datagram) Datagram {
	b := make([]byte, len(d.Bytes))
	copy(b, d.Bytes)
	return Datagram{From: d.From, To: d.To, Bytes: b}
}
