package coord

import (
	"testing"

	"wantasticmesh/internal/wmproto"
)

func healFor(hs []Heal, n wmproto.NodeID) (Heal, bool) {
	for _, h := range hs {
		if h.Node == n {
			return h, true
		}
	}
	return Heal{}, false
}

// A stale node must get a rejoin; a reporting node with a backhaul radio but no
// link must get reform-backhaul; a healthy node gets nothing.
func TestEngineSelfHeal(t *testing.T) {
	nodes := []NodeState{
		{ID: node(1), Stale: true, HasBackhaulRadio: true},
		{ID: node(2), HasBackhaulRadio: true, HasBackhaulLink: false},
		{ID: node(3), HasBackhaulRadio: true, HasBackhaulLink: true, BackhaulDBm: -55},
	}
	p := Engine{WeakBackhaulDBm: -75}.Tick(nodes, AllAdjacent{})
	if h, ok := healFor(p.Heals, node(1)); !ok || h.Kind != HealRejoin {
		t.Fatalf("node1 should rejoin, got %+v", p.Heals)
	}
	if h, ok := healFor(p.Heals, node(2)); !ok || h.Kind != HealReformBackhaul {
		t.Fatalf("node2 should reform-backhaul, got %+v", p.Heals)
	}
	if _, ok := healFor(p.Heals, node(3)); ok {
		t.Fatalf("healthy node3 should need no heal, got %+v", p.Heals)
	}
}

// A weak backhaul link triggers relocate; a strong one does not.
func TestEngineRelocateOnWeak(t *testing.T) {
	nodes := []NodeState{
		{ID: node(1), HasBackhaulRadio: true, HasBackhaulLink: true, BackhaulDBm: -82},
		{ID: node(2), HasBackhaulRadio: true, HasBackhaulLink: true, BackhaulDBm: -60},
	}
	p := Engine{WeakBackhaulDBm: -75}.Tick(nodes, AllAdjacent{})
	if h, ok := healFor(p.Heals, node(1)); !ok || h.Kind != HealRelocate {
		t.Fatalf("weak node1 should relocate, got %+v", p.Heals)
	}
	if _, ok := healFor(p.Heals, node(2)); ok {
		t.Fatalf("strong node2 should not relocate, got %+v", p.Heals)
	}
}

// Stale outranks a weak/missing uplink: only one action per node, the most urgent.
func TestEngineStaleOutranks(t *testing.T) {
	nodes := []NodeState{
		{ID: node(1), Stale: true, HasBackhaulRadio: true, HasBackhaulLink: false, BackhaulDBm: -90},
	}
	p := Engine{WeakBackhaulDBm: -75}.Tick(nodes, AllAdjacent{})
	if len(p.Heals) != 1 || p.Heals[0].Kind != HealRejoin {
		t.Fatalf("stale must yield exactly one rejoin, got %+v", p.Heals)
	}
}

// The engine also plans channels: two adjacent co-channel radios get separated,
// while a backhaul radio is never moved.
func TestEngineChannelsAndBackhaul(t *testing.T) {
	nodes := []NodeState{
		{ID: node(1), Radios: []Radio{{Node: node(1), Index: 0, Band: Band24, Channel: 1, Width: 20}}},
		{ID: node(2), Radios: []Radio{
			{Node: node(2), Index: 0, Band: Band24, Channel: 1, Width: 20},
			{Node: node(2), Index: 2, Band: Band6, Channel: 1, Width: 160, Backhaul: true},
		}, HasBackhaulRadio: true, HasBackhaulLink: true, BackhaulDBm: -50},
	}
	p := Engine{WeakBackhaulDBm: -75}.Tick(nodes, AllAdjacent{})
	if len(p.Channels) == 0 {
		t.Fatal("expected a channel directive to separate the co-channel 2.4 radios")
	}
	for _, d := range p.Channels {
		if d.Band == Band6 {
			t.Fatalf("backhaul 6 GHz radio must never be retuned, got %+v", d)
		}
	}
	if len(p.Heals) != 0 {
		t.Fatalf("both nodes healthy; no heals expected, got %+v", p.Heals)
	}
}

// A settled, healthy, conflict-free mesh produces an empty plan (no churn).
func TestEngineQuiescent(t *testing.T) {
	nodes := []NodeState{
		{ID: node(1), Radios: []Radio{{Node: node(1), Index: 0, Band: Band24, Channel: 1, Width: 20}},
			HasBackhaulRadio: true, HasBackhaulLink: true, BackhaulDBm: -50},
		{ID: node(2), Radios: []Radio{{Node: node(2), Index: 0, Band: Band24, Channel: 6, Width: 20}},
			HasBackhaulRadio: true, HasBackhaulLink: true, BackhaulDBm: -55},
	}
	p := Engine{WeakBackhaulDBm: -75}.Tick(nodes, AllAdjacent{})
	if len(p.Channels) != 0 || len(p.Heals) != 0 {
		t.Fatalf("quiescent mesh must produce an empty plan, got %+v", p)
	}
}
