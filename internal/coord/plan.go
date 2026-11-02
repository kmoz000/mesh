// Package coord is the device-free coordination layer: it turns the controller's
// assembled radio view into a minimal set of per-radio channel directives that
// reduce co-channel interference between neighbouring nodes. It encodes the
// standing goals directly:
//
//   - the backhaul is respected: a radio carrying backhaul is never retuned; it
//     is a fixed occupant that other radios plan around;
//   - links stay stable: a directive is emitted only when the move strictly
//     reduces conflict, so a settled network does not churn;
//   - the controller coordinates but does not do all the work: it computes the
//     plan; each agent applies only its own directives, locally, through the
//     guarded CSA.
//
// It performs no I/O and holds no device coupling, so it is unit-tested directly.
package coord

import (
	"sort"

	"wantasticmesh/internal/wmproto"
)

// Band values mirror wmproto.RadioRecord.Band.
const (
	Band24 uint8 = 1
	Band5  uint8 = 2
	Band6  uint8 = 3
)

// Radio is one radio's planning-relevant state, lifted from a controller Node's
// RadioRecord plus the owning node id.
type Radio struct {
	Node     wmproto.NodeID
	Index    uint8  // phy index within the node
	Band     uint8  // Band24/Band5/Band6
	Channel  uint16 // current primary channel
	Width    uint16 // current width in MHz (0 treated as 20)
	Backhaul bool   // carries backhaul: never retuned
}

// Directive asks one node to move one radio from From to To. The agent that owns
// Node applies it; the controller never actuates.
type Directive struct {
	Node  wmproto.NodeID
	Index uint8
	Band  uint8
	From  uint16
	To    uint16
}

// Adjacency reports whether two nodes share an RF collision domain, i.e. whether
// their radios on the same band can interfere. A controller derives this from the
// link graph; AllAdjacent is the conservative default for a small mesh.
type Adjacency interface {
	Adjacent(a, b wmproto.NodeID) bool
}

// AllAdjacent treats every node pair as interfering (safe, conservative).
type AllAdjacent struct{}

// Adjacent always returns true.
func (AllAdjacent) Adjacent(_, _ wmproto.NodeID) bool { return true }

// LinkedAdjacency marks pairs adjacent iff a link was observed between them (plus
// every node with itself). It is the usual real input: nodes that can hear each
// other are the ones that interfere.
type LinkedAdjacency struct {
	pairs map[[2]wmproto.NodeID]bool
}

// NewLinkedAdjacency builds adjacency from undirected node pairs.
func NewLinkedAdjacency(pairs [][2]wmproto.NodeID) *LinkedAdjacency {
	m := make(map[[2]wmproto.NodeID]bool, len(pairs)*2)
	for _, p := range pairs {
		m[p] = true
		m[[2]wmproto.NodeID{p[1], p[0]}] = true
	}
	return &LinkedAdjacency{pairs: m}
}

// Adjacent reports whether a and b are the same node or a linked pair.
func (l *LinkedAdjacency) Adjacent(a, b wmproto.NodeID) bool {
	if a == b {
		return true
	}
	return l.pairs[[2]wmproto.NodeID{a, b}]
}

// DefaultCandidates lists the channels the planner may move a radio to, per band.
// These are the standard non-overlapping 20 MHz anchors (2.4: 1/6/11; a practical
// non-DFS 5 GHz set; the low 6 GHz 20 MHz grid). A caller may supply its own set
// (e.g. the radio's probed, regulatory-filtered channel list) instead.
var DefaultCandidates = map[uint8][]uint16{
	Band24: {1, 6, 11},
	Band5:  {36, 40, 44, 48, 149, 153, 157, 161},
	Band6:  {1, 5, 9, 13, 17, 21, 25, 29, 33, 37},
}

// centerMHz returns a channel's 20 MHz centre frequency for a band, or 0 if the
// band is unknown. (2.4: ch14 is the 2484 special case; 6 GHz: ch2 is 5935.)
func centerMHz(band uint8, ch uint16) int {
	switch band {
	case Band24:
		if ch == 14 {
			return 2484
		}
		return 2407 + 5*int(ch)
	case Band5:
		return 5000 + 5*int(ch)
	case Band6:
		if ch == 2 {
			return 5935
		}
		return 5950 + 5*int(ch)
	default:
		return 0
	}
}

func spanMHz(width uint16) int {
	if width == 0 {
		return 20
	}
	return int(width)
}

// overlaps reports whether two channels on the same band occupy overlapping
// spectrum: their centre frequencies are closer than the sum of their half
// widths. For 2.4 GHz this makes 1/6/11 mutually clear while 1/3 conflict; for
// 5/6 GHz it accounts for 40/80/160 MHz segments sharing spectrum.
func overlaps(band uint8, c1, w1, c2, w2 uint16) bool {
	f1, f2 := centerMHz(band, c1), centerMHz(band, c2)
	if f1 == 0 || f2 == 0 {
		return false
	}
	d := f1 - f2
	if d < 0 {
		d = -d
	}
	return d < (spanMHz(w1)+spanMHz(w2))/2
}

// PlanChannels computes the directives that reduce co-channel interference.
// candidates maps a band to the channels a radio may move to; nil uses
// DefaultCandidates. adj decides which nodes interfere; nil means AllAdjacent.
//
// Backhaul radios are never moved but are counted as fixed occupants, so other
// radios plan around them. A radio is moved only to a candidate with strictly
// fewer conflicts than its current channel, so a conflict-free network yields no
// directives.
func PlanChannels(radios []Radio, adj Adjacency, candidates map[uint8][]uint16) []Directive {
	if adj == nil {
		adj = AllAdjacent{}
	}
	if candidates == nil {
		candidates = DefaultCandidates
	}

	// planned[i] is radio i's current-or-chosen channel; it evolves as we assign.
	planned := make([]uint16, len(radios))
	for i := range radios {
		planned[i] = radios[i].Channel
	}

	// Deterministic order: by node id, then phy index. A stable order makes the
	// plan reproducible and testable.
	order := make([]int, len(radios))
	for i := range order {
		order[i] = i
	}
	sort.SliceStable(order, func(a, b int) bool {
		ra, rb := radios[order[a]], radios[order[b]]
		if ra.Node != rb.Node {
			return less(ra.Node, rb.Node)
		}
		return ra.Index < rb.Index
	})

	conflicts := func(self int, ch uint16) int {
		r := radios[self]
		n := 0
		for j := range radios {
			if j == self {
				continue
			}
			o := radios[j]
			// A radio whose channel is unknown (0) is not a real occupant of any
			// channel, so it cannot conflict — skip it rather than treat 0 as a
			// shared channel.
			if planned[j] == 0 || o.Band != r.Band || !adj.Adjacent(r.Node, o.Node) {
				continue
			}
			if overlaps(r.Band, ch, r.Width, planned[j], o.Width) {
				n++
			}
		}
		return n
	}

	var out []Directive
	for _, i := range order {
		r := radios[i]
		if r.Backhaul {
			continue // respected: fixed occupant, never retuned
		}
		cur := planned[i]
		if cur == 0 {
			continue // channel unknown: don't invent one, nothing to plan
		}
		best, bestConf := cur, conflicts(i, cur)
		if bestConf == 0 {
			continue // already clean; leave it (stability)
		}
		for _, cand := range candidates[r.Band] {
			if cand == cur {
				continue
			}
			if c := conflicts(i, cand); c < bestConf {
				best, bestConf = cand, c
			}
		}
		if best != cur {
			planned[i] = best
			out = append(out, Directive{Node: r.Node, Index: r.Index, Band: r.Band, From: cur, To: best})
		}
	}
	return out
}

// less orders two node ids lexicographically.
func less(a, b wmproto.NodeID) bool {
	for i := range a {
		if a[i] != b[i] {
			return a[i] < b[i]
		}
	}
	return false
}
