package coord

import (
	"testing"

	"wantasticmesh/internal/wmproto"
)

func node(b byte) wmproto.NodeID { return wmproto.NodeID{b} }

func hasDirectiveFor(ds []Directive, n wmproto.NodeID, idx uint8) (Directive, bool) {
	for _, d := range ds {
		if d.Node == n && d.Index == idx {
			return d, true
		}
	}
	return Directive{}, false
}

// Three nodes already on the 1/6/11 anchors must produce no directives: a settled
// network does not churn.
func TestPlanStableNoChurn(t *testing.T) {
	radios := []Radio{
		{Node: node(1), Index: 0, Band: Band24, Channel: 1, Width: 20},
		{Node: node(2), Index: 0, Band: Band24, Channel: 6, Width: 20},
		{Node: node(3), Index: 0, Band: Band24, Channel: 11, Width: 20},
	}
	if ds := PlanChannels(radios, AllAdjacent{}, nil); len(ds) != 0 {
		t.Fatalf("expected no directives for a clean plan, got %v", ds)
	}
}

// Two adjacent radios on the same 2.4 GHz channel: exactly one should move to a
// non-overlapping channel, resolving the conflict.
func TestPlanResolvesCoChannel(t *testing.T) {
	radios := []Radio{
		{Node: node(1), Index: 0, Band: Band24, Channel: 1, Width: 20},
		{Node: node(2), Index: 0, Band: Band24, Channel: 1, Width: 20},
	}
	ds := PlanChannels(radios, AllAdjacent{}, nil)
	// Exactly one radio should move (moving either resolves the conflict); the
	// mover goes to a non-overlapping anchor, and the result is conflict-free.
	if len(ds) != 1 {
		t.Fatalf("expected 1 directive, got %d: %v", len(ds), ds)
	}
	d := ds[0]
	if d.From != 1 || (d.To != 6 && d.To != 11) {
		t.Fatalf("unexpected move %+v", d)
	}
	// Apply the move and confirm no overlap remains between the two radios.
	for i := range radios {
		if radios[i].Node == d.Node && radios[i].Index == d.Index {
			radios[i].Channel = d.To
		}
	}
	if overlaps(Band24, radios[0].Channel, radios[0].Width, radios[1].Channel, radios[1].Width) {
		t.Fatalf("conflict not resolved after applying %+v", d)
	}
}

// A backhaul radio on a conflicting channel must never be retuned; the
// non-backhaul radio sharing its channel must move around it.
func TestPlanRespectsBackhaul(t *testing.T) {
	radios := []Radio{
		{Node: node(1), Index: 2, Band: Band6, Channel: 1, Width: 160, Backhaul: true},
		{Node: node(2), Index: 2, Band: Band6, Channel: 1, Width: 160},
	}
	ds := PlanChannels(radios, AllAdjacent{}, nil)
	if _, ok := hasDirectiveFor(ds, node(1), 2); ok {
		t.Fatalf("backhaul radio must never be retuned, got %v", ds)
	}
	if _, ok := hasDirectiveFor(ds, node(2), 2); !ok {
		t.Fatalf("expected the non-backhaul radio to move around the backhaul, got %v", ds)
	}
}

// Two backhaul radios on the same channel: neither may move (both respected),
// even though they conflict — the planner must not touch backhaul at all.
func TestPlanTwoBackhaulUntouched(t *testing.T) {
	radios := []Radio{
		{Node: node(1), Index: 2, Band: Band6, Channel: 1, Width: 160, Backhaul: true},
		{Node: node(2), Index: 2, Band: Band6, Channel: 1, Width: 160, Backhaul: true},
	}
	if ds := PlanChannels(radios, AllAdjacent{}, nil); len(ds) != 0 {
		t.Fatalf("backhaul radios must be untouched, got %v", ds)
	}
}

// Non-adjacent nodes may reuse the same channel without conflict: spatial reuse.
func TestPlanSpatialReuse(t *testing.T) {
	radios := []Radio{
		{Node: node(1), Index: 0, Band: Band24, Channel: 1, Width: 20},
		{Node: node(2), Index: 0, Band: Band24, Channel: 1, Width: 20},
	}
	adj := NewLinkedAdjacency(nil) // no links => not adjacent
	if ds := PlanChannels(radios, adj, nil); len(ds) != 0 {
		t.Fatalf("non-adjacent nodes may share a channel, got %v", ds)
	}
}

// Overlapping wide 6 GHz channels on adjacent nodes should be separated.
func TestPlanWideChannelOverlap(t *testing.T) {
	radios := []Radio{
		{Node: node(1), Index: 2, Band: Band6, Channel: 1, Width: 160},
		{Node: node(2), Index: 2, Band: Band6, Channel: 5, Width: 160}, // overlaps ch1/160
	}
	ds := PlanChannels(radios, AllAdjacent{}, nil)
	if len(ds) == 0 {
		t.Fatalf("expected a move to separate overlapping 160 MHz channels")
	}
}

// overlaps must match the 2.4 GHz reality: 1/6/11 mutually clear, 1/3 conflict.
func TestOverlaps24(t *testing.T) {
	if overlaps(Band24, 1, 20, 6, 20) {
		t.Fatal("ch1 and ch6 (20 MHz) must not overlap")
	}
	if overlaps(Band24, 6, 20, 11, 20) {
		t.Fatal("ch6 and ch11 (20 MHz) must not overlap")
	}
	if !overlaps(Band24, 1, 20, 3, 20) {
		t.Fatal("ch1 and ch3 (20 MHz) must overlap")
	}
	if !overlaps(Band24, 1, 20, 1, 20) {
		t.Fatal("a channel always overlaps itself")
	}
}

// Different bands never conflict even on numerically equal channel numbers.
func TestPlanBandsIndependent(t *testing.T) {
	radios := []Radio{
		{Node: node(1), Index: 0, Band: Band24, Channel: 1, Width: 20},
		{Node: node(2), Index: 1, Band: Band6, Channel: 1, Width: 20},
	}
	if ds := PlanChannels(radios, AllAdjacent{}, nil); len(ds) != 0 {
		t.Fatalf("radios on different bands must not conflict, got %v", ds)
	}
}

// Radios reporting channel 0 (unknown) must not be treated as a co-channel
// conflict, and must never receive a directive (we don't invent a channel).
func TestPlanIgnoresUnknownChannel(t *testing.T) {
	radios := []Radio{
		{Node: node(1), Index: 2, Band: Band6, Channel: 0, Width: 160},
		{Node: node(1), Index: 3, Band: Band6, Channel: 0, Width: 160},
	}
	if ds := PlanChannels(radios, AllAdjacent{}, nil); len(ds) != 0 {
		t.Fatalf("unknown-channel radios must yield no directives, got %v", ds)
	}
}
