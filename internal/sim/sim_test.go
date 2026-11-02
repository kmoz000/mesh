package sim

import (
	"testing"
	"time"

	"wantasticmesh/internal/wmproto"
)

// --- topology builders -------------------------------------------------------

type adjacency map[wmproto.NodeID]map[wmproto.NodeID]struct{}

// build registers n nodes with sim and returns the intended directed adjacency,
// where neighbors(i) lists the indices node i has links to. Edges are the union
// of each node's reported out-edges; wmproto links are directional.
func build(s *Sim, n int, neighbors func(i int) []int) adjacency {
	ids := make([]wmproto.NodeID, n)
	for i := 0; i < n; i++ {
		ids[i] = nodeID(i)
	}
	exp := make(adjacency)
	for i := 0; i < n; i++ {
		set := make(map[wmproto.NodeID]struct{})
		var peers []wmproto.NodeID
		for _, j := range neighbors(i) {
			peers = append(peers, ids[j])
			set[ids[j]] = struct{}{}
		}
		s.AddNode(ids[i], bootID(i), peers)
		exp[ids[i]] = set
	}
	return exp
}

func chainNeighbors(n int) func(int) []int {
	return func(i int) []int {
		var out []int
		if i > 0 {
			out = append(out, i-1)
		}
		if i < n-1 {
			out = append(out, i+1)
		}
		return out
	}
}

func ringNeighbors(n int) func(int) []int {
	return func(i int) []int { return []int{(i - 1 + n) % n, (i + 1) % n} }
}

// diamond: 0 top, 1 left, 2 right, 3 bottom; edges 0-1, 0-2, 1-3, 2-3.
func diamondNeighbors() func(int) []int {
	adj := [][]int{{1, 2}, {0, 3}, {0, 3}, {1, 2}}
	return func(i int) []int { return adj[i] }
}

// dense: a clique. Capped at 8 nodes so a node's 7 peers stay under MaxLinks
// (32); a larger clique would legitimately hit wmproto's ErrCapacity.
func cliqueNeighbors(n int) func(int) []int {
	return func(i int) []int {
		var out []int
		for j := 0; j < n; j++ {
			if j != i {
				out = append(out, j)
			}
		}
		return out
	}
}

// --- assertions --------------------------------------------------------------

func totalEdges(exp adjacency) int {
	n := 0
	for _, set := range exp {
		n += len(set)
	}
	return n
}

// assertGraph checks the assembled graph is exactly exp: equal edge counts plus
// every expected edge present implies no extras.
func assertGraph(t *testing.T, g *Graph, exp adjacency) {
	t.Helper()
	if got, want := g.EdgeCount(), totalEdges(exp); got != want {
		t.Fatalf("edge count=%d want %d", got, want)
	}
	for origin, set := range exp {
		for peer := range set {
			if !g.HasEdge(origin, peer) {
				t.Fatalf("missing edge %x -> %x", origin[:4], peer[:4])
			}
		}
	}
}

// assertOneAcceptEach asserts every id accepted exactly once and that no
// non-nil outcome is anything other than the allowed duplicate/replay rejection
// errors. This, not the graph, is what proves reports are never double-counted.
func assertOneAcceptEach(t *testing.T, outcomes []Outcome, ids []wmproto.NodeID) {
	t.Helper()
	accepts := make(map[wmproto.NodeID]int)
	for _, o := range outcomes {
		if o.Err == nil {
			accepts[o.From]++
			continue
		}
		if o.Err != wmproto.ErrChallenge && o.Err != wmproto.ErrReplay {
			t.Fatalf("unexpected outcome error for %x: %v", o.From[:4], o.Err)
		}
	}
	for _, id := range ids {
		if accepts[id] != 1 {
			t.Fatalf("node %x accepted %d times, want exactly 1", id[:4], accepts[id])
		}
	}
}

var epoch = time.Unix(1_000_000, 0)

// --- test 1: topology convergence -------------------------------------------

func TestTopologyConvergence(t *testing.T) {
	cases := []struct {
		name string
		n    int
		nbr  func(int) []int
	}{
		{"chain", 6, chainNeighbors(6)},
		{"ring", 8, ringNeighbors(8)},
		{"diamond", 4, diamondNeighbors()},
		{"dense", 6, cliqueNeighbors(6)},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			s := NewSim(1)
			exp := build(s, tc.n, tc.nbr)
			out := s.PollAll(epoch)
			assertOneAcceptEach(t, out, s.IDs())
			assertGraph(t, s.Graph, exp)
			if s.Count() != tc.n {
				t.Fatalf("Count=%d want %d", s.Count(), tc.n)
			}
			// The handshake completes: every node received exactly one Ack.
			for _, id := range s.IDs() {
				if got := s.nodes[id].Acked(); got != 1 {
					t.Fatalf("node %x acked %d times, want 1", id[:4], got)
				}
			}
		})
	}
}

// --- test 2: reordering + duplication ---------------------------------------

func TestReorderAndDuplication(t *testing.T) {
	s := NewSim(42)
	n := 8
	exp := build(s, n, ringNeighbors(n))
	// Always duplicate, and shuffle every delivered batch.
	s.Medium.Reorder = true
	s.Medium.DupProb = 1.0

	out := s.PollAll(epoch)

	// Each node is still accepted exactly once; every duplicate is rejected by
	// the one-shot challenge, so the graph cannot be corrupted or double-counted.
	assertOneAcceptEach(t, out, s.IDs())
	assertGraph(t, s.Graph, exp)
	if s.Count() != n {
		t.Fatalf("Count=%d want %d", s.Count(), n)
	}
	if s.Medium.Dups == 0 {
		t.Fatal("expected the medium to have duplicated datagrams")
	}
	// Medium-induced duplicates always hit the consumed challenge first, so the
	// rejection is ErrChallenge, never ErrReplay.
	var rejects int
	for _, o := range out {
		if o.Err == wmproto.ErrChallenge {
			rejects++
		} else if o.Err == wmproto.ErrReplay {
			t.Fatal("duplicate was rejected as ErrReplay; expected ErrChallenge")
		}
	}
	if rejects == 0 {
		t.Fatal("expected at least one duplicate rejected with ErrChallenge")
	}
	// Acks are duplicated too, but each node is Acked at least once.
	for _, id := range s.IDs() {
		if s.nodes[id].Acked() < 1 {
			t.Fatalf("node %x was never acked", id[:4])
		}
	}
}

// --- test 3: replay ----------------------------------------------------------

func TestReplayRejectedAfterChallengeConsumed(t *testing.T) {
	s := NewSim(7)
	id := nodeID(1)
	n := s.AddNode(id, bootID(1), []wmproto.NodeID{nodeID(2)})

	now := epoch
	chal := s.nextChallenge()
	if err := s.ctrl.IssueChallenge(id, chal, now); err != nil {
		t.Fatalf("IssueChallenge: %v", err)
	}
	rep, err := n.makeReport(chal, nil)
	if err != nil {
		t.Fatalf("makeReport: %v", err)
	}
	// makeReport returns a slice into node scratch; copy before reuse/replay.
	captured := append([]byte(nil), rep...)

	if _, err := s.ingest(Datagram{From: id, To: s.ctrlID, Bytes: captured}, now); err != nil {
		t.Fatalf("first ingest: %v", err)
	}
	// Redeliver the exact same datagram: the challenge is consumed.
	if _, err := s.ingest(Datagram{From: id, To: s.ctrlID, Bytes: captured}, now.Add(time.Second)); err != wmproto.ErrChallenge {
		t.Fatalf("replay: got %v want ErrChallenge", err)
	}
	// Even after a brand-new challenge is outstanding, the stale datagram echoes
	// the old challenge and is still rejected.
	chal2 := s.nextChallenge()
	if err := s.ctrl.IssueChallenge(id, chal2, now.Add(2*time.Second)); err != nil {
		t.Fatalf("re-challenge: %v", err)
	}
	if _, err := s.ingest(Datagram{From: id, To: s.ctrlID, Bytes: captured}, now.Add(3*time.Second)); err != wmproto.ErrChallenge {
		t.Fatalf("replay under new challenge: got %v want ErrChallenge", err)
	}
}

// --- test 4: boot reset ------------------------------------------------------

func TestBootResetReaccepted(t *testing.T) {
	s := NewSim(3)
	exp := build(s, 4, ringNeighbors(4))
	if out := s.PollAll(epoch); len(out) != 4 {
		t.Fatalf("initial poll delivered %d reports", len(out))
	}
	assertGraph(t, s.Graph, exp)
	count0 := s.Count()

	// Reboot one node: new boot id, sequence restarts at 1.
	target := nodeID(0)
	s.nodes[target].Reboot(bootID(999))

	out := s.PollAll(epoch.Add(time.Second))
	assertOneAcceptEach(t, out, s.IDs())
	assertGraph(t, s.Graph, exp)
	if s.Count() != count0 {
		t.Fatalf("Count changed across reboot: %d -> %d", count0, s.Count())
	}
}

// A reused boot id with a rewound sequence is caught by the per-boot sequence
// guard. This is only reachable from a misbehaving agent, not from any network
// reordering/duplication/replay (those hit the one-shot challenge first); the
// sim exercises it by driving the exchange directly.
func TestSequenceRewindWithinBootRejected(t *testing.T) {
	s := NewSim(4)
	id := nodeID(1)
	n := s.AddNode(id, bootID(1), []wmproto.NodeID{nodeID(2)})

	now := epoch
	chal := s.nextChallenge()
	_ = s.ctrl.IssueChallenge(id, chal, now)
	rep, _ := n.makeReport(chal, nil) // seq 1, accepted
	if _, err := s.ingest(Datagram{From: id, To: s.ctrlID, Bytes: append([]byte(nil), rep...)}, now); err != nil {
		t.Fatalf("accept seq 1: %v", err)
	}

	// Same boot, rewind the sequence below the last accepted value.
	now = now.Add(time.Second)
	chal2 := s.nextChallenge()
	_ = s.ctrl.IssueChallenge(id, chal2, now)
	n.seq = 0                           // next makeReport produces seq 1 again
	rep2, _ := n.makeReport(chal2, nil) // seq 1 <= last accepted, same boot
	if _, err := s.ingest(Datagram{From: id, To: s.ctrlID, Bytes: append([]byte(nil), rep2...)}, now); err != wmproto.ErrReplay {
		t.Fatalf("rewound sequence: got %v want ErrReplay", err)
	}
}

// --- test 5: partition + merge ----------------------------------------------

func TestPartitionThenMergeReconverges(t *testing.T) {
	s := NewSim(11)
	n := 6
	exp := build(s, n, ringNeighbors(n)) // ring 0-1-2-3-4-5-0

	// Split the ring: {0,1,2} with the controller (partition 0), {3,4,5} on the
	// far side (partition 1). Boundary links 2-3 and 5-0 cross the cut.
	for i := 3; i <= 5; i++ {
		s.Medium.SetPartition(nodeID(i), 1)
	}

	// Partitioned poll: queries to the far side are dropped, and near-side nodes
	// can only observe peers they still have a path to.
	out := s.PollAll(epoch)
	if s.Medium.PartitionDrops == 0 {
		t.Fatal("expected cross-partition datagrams to be dropped")
	}
	// Far side never reports.
	for i := 3; i <= 5; i++ {
		if _, seen := accepted(out, nodeID(i)); seen {
			t.Fatalf("node %d reported across a partition", i)
		}
	}
	// Far side received no Ack either: the handshake never started.
	for i := 3; i <= 5; i++ {
		if got := s.nodes[nodeID(i)].Acked(); got != 0 {
			t.Fatalf("far-side node %d acked %d times during partition", i, got)
		}
	}
	// The near side reports exactly its interior edges: 0->{1}, 1->{0,2}, 2->{1}.
	// Boundary edges (0-5, 2-3) are truncated and the far side is wholly absent.
	if got := s.Graph.EdgeCount(); got != 4 {
		t.Fatalf("partitioned graph edge count=%d want 4", got)
	}
	if s.Graph.HasEdge(nodeID(0), nodeID(5)) || s.Graph.HasEdge(nodeID(2), nodeID(3)) {
		t.Fatal("boundary edge present during partition")
	}
	if !s.Graph.HasEdge(nodeID(1), nodeID(0)) || !s.Graph.HasEdge(nodeID(1), nodeID(2)) {
		t.Fatal("near-side interior edges missing during partition")
	}

	// Heal: a full-snapshot report from every node reconverges the whole graph,
	// restoring the boundary edges and the far side.
	s.Medium.Heal()
	out = s.PollAll(epoch.Add(time.Second))
	assertOneAcceptEach(t, out, s.IDs())
	assertGraph(t, s.Graph, exp)
}

func accepted(outcomes []Outcome, id wmproto.NodeID) (int, bool) {
	seen := false
	n := 0
	for _, o := range outcomes {
		if o.From == id {
			seen = true
			if o.Err == nil {
				n++
			}
		}
	}
	return n, seen
}

// --- test 6: simultaneous reboot --------------------------------------------

func TestSimultaneousRebootNoStaleState(t *testing.T) {
	s := NewSim(5)
	n := 6
	build(s, n, ringNeighbors(n))
	s.PollAll(epoch)
	count0 := s.Count()

	// Every node reboots at once AND the topology shrinks from a ring to a
	// chain. New boot ids let the restarted sequences be accepted; the
	// full-snapshot semantics must drop the now-stale ring-closing edges.
	chain := chainNeighbors(n)
	ids := s.IDs()
	for i := 0; i < n; i++ {
		node := s.nodes[ids[i]]
		node.Reboot(bootID(1000 + i))
		var peers []wmproto.NodeID
		for _, j := range chain(i) {
			peers = append(peers, ids[j])
		}
		node.peers = peers
	}

	out := s.PollAll(epoch.Add(time.Second))
	assertOneAcceptEach(t, out, ids) // no ErrReplay: new boots reset the sequence

	// Rebuild the expected chain adjacency over the same ids.
	exp := make(adjacency)
	for i := 0; i < n; i++ {
		set := make(map[wmproto.NodeID]struct{})
		for _, j := range chain(i) {
			set[ids[j]] = struct{}{}
		}
		exp[ids[i]] = set
	}
	assertGraph(t, s.Graph, exp) // stale ring edges (0-5) are gone

	if s.Count() != count0 {
		t.Fatalf("Count changed across simultaneous reboot: %d -> %d", count0, s.Count())
	}
	if s.Count() > wmproto.MaxAgents {
		t.Fatalf("Count=%d exceeds MaxAgents=%d", s.Count(), wmproto.MaxAgents)
	}
}

// --- test 7: scale smoke ----------------------------------------------------

func TestScale128Nodes(t *testing.T) {
	s := NewSim(99)
	n := wmproto.MaxAgents // 128
	exp := build(s, n, ringNeighbors(n))

	out := s.PollAll(epoch)

	assertOneAcceptEach(t, out, s.IDs())
	assertGraph(t, s.Graph, exp)
	if s.Count() != n || s.Count() > wmproto.MaxAgents {
		t.Fatalf("Count=%d want %d (<= MaxAgents %d)", s.Count(), n, wmproto.MaxAgents)
	}
	// A ring contributes exactly two directed edges per node.
	if got := s.Graph.EdgeCount(); got != 2*n {
		t.Fatalf("edge count=%d want %d", got, 2*n)
	}
}

// --- bonus: convergence under loss ------------------------------------------

func TestConvergenceUnderLoss(t *testing.T) {
	s := NewSim(2024)
	n := 8
	exp := build(s, n, ringNeighbors(n))
	s.Medium.LossProb = 0.3 // drops queries and reports alike

	now := epoch
	const maxRounds = 100
	var rounds int
	for rounds = 1; rounds <= maxRounds; rounds++ {
		s.PollAll(now)
		now = now.Add(time.Second)
		if s.Graph.EdgeCount() == totalEdges(exp) {
			break
		}
	}
	if rounds > maxRounds {
		t.Fatalf("did not converge within %d rounds under loss", maxRounds)
	}
	assertGraph(t, s.Graph, exp)
	if s.Count() != n {
		t.Fatalf("Count=%d want %d", s.Count(), n)
	}
	if s.Medium.LossDrops == 0 {
		t.Fatal("expected the medium to have dropped datagrams")
	}
}
