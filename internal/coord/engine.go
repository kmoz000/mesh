package coord

import (
	"sort"

	"wantasticmesh/internal/wmproto"
)

// Engine is the controller-side closed loop: from a snapshot of node state it
// produces the actions that keep the mesh stable and well-organised — channel
// directives (interference), plus self-healing actions (silent nodes, lost or
// weak backhaul). The controller computes; each agent applies only the actions
// addressed to it, so no single node carries the work. The engine is pure and
// unit-tested; a caller drives it on each controller tick with the assembled
// graph and dispatches the result through the authenticated exchange.
type Engine struct {
	// WeakBackhaulDBm: a backhaul link at or below this RSSI is a relocation
	// candidate (too weak for stable max throughput). 0 disables the check.
	WeakBackhaulDBm int8
	// Candidates overrides the per-band channel candidate set (nil = defaults).
	Candidates map[uint8][]uint16
}

// HealKind classifies a recovery action.
type HealKind uint8

const (
	// HealNone is no action.
	HealNone HealKind = iota
	// HealRejoin: the node has gone silent/stale; re-challenge and, if it owns a
	// backhaul radio, have it re-form its uplink (isolated-device recovery).
	HealRejoin
	// HealReformBackhaul: the node is reporting but has no usable backhaul link;
	// instruct it to (re)associate its backhaul STA.
	HealReformBackhaul
	// HealRelocate: the node's backhaul link is up but weak; move it to a better
	// parent/channel so the topology stays high-throughput over time.
	HealRelocate
)

func (k HealKind) String() string {
	switch k {
	case HealRejoin:
		return "rejoin"
	case HealReformBackhaul:
		return "reform-backhaul"
	case HealRelocate:
		return "relocate"
	default:
		return "none"
	}
}

// NodeState is one node's loop-relevant state, lifted from the controller's view.
type NodeState struct {
	ID               wmproto.NodeID
	Radios           []Radio // all radios (fed to the channel planner)
	Stale            bool    // controller saw no fresh report within the TTL
	HasBackhaulRadio bool    // the node declares/flags a backhaul radio
	HasBackhaulLink  bool    // a usable backhaul link is currently present
	BackhaulDBm      int8    // best backhaul link RSSI (0 = unknown)
}

// Heal is a recovery action addressed to one node.
type Heal struct {
	Node   wmproto.NodeID
	Kind   HealKind
	Reason string
}

// Plan is the engine's output for one tick.
type Plan struct {
	Channels []Directive // interference-reducing channel moves
	Heals    []Heal      // self-healing actions, one node at a time
}

// Tick computes the plan for the current snapshot. adj decides which nodes
// interfere (nil = AllAdjacent). It is deterministic and side-effect free.
func (e Engine) Tick(nodes []NodeState, adj Adjacency) Plan {
	// Channel plan over every radio of every node.
	var radios []Radio
	for _, n := range nodes {
		radios = append(radios, n.Radios...)
	}
	plan := Plan{Channels: PlanChannels(radios, adj, e.Candidates)}

	// Self-heal, deterministic order by node id. One action per node, most urgent
	// first: a silent node (rejoin) outranks a missing uplink (reform) outranks a
	// weak uplink (relocate). A healthy node yields nothing.
	order := append([]NodeState(nil), nodes...)
	sort.SliceStable(order, func(a, b int) bool { return less(order[a].ID, order[b].ID) })
	for _, n := range order {
		switch {
		case n.Stale:
			plan.Heals = append(plan.Heals, Heal{n.ID, HealRejoin, "no fresh report within TTL"})
		case n.HasBackhaulRadio && !n.HasBackhaulLink:
			plan.Heals = append(plan.Heals, Heal{n.ID, HealReformBackhaul, "backhaul radio present but no link"})
		case n.HasBackhaulLink && e.WeakBackhaulDBm != 0 && n.BackhaulDBm != 0 && n.BackhaulDBm <= e.WeakBackhaulDBm:
			plan.Heals = append(plan.Heals, Heal{n.ID, HealRelocate, "backhaul link weak"})
		}
	}
	return plan
}
