package topology

import (
	"testing"
	"time"

	"wantasticmesh/internal/ieee1905"
)

func TestUpdateInitializesAndFindsPeersAcrossHoles(t *testing.T) {
	var table Table
	now := time.Unix(100, 0)
	if !table.Update(testMessage(1), now) || !table.Update(testMessage(2), now) {
		t.Fatal("first observations must create peer entries")
	}
	table.peers[0] = Peer{} // a free slot must not hide a later matching identity
	if table.Update(testMessage(2), now.Add(time.Second)) {
		t.Fatal("refresh must update the existing peer, not create a duplicate")
	}
	if got := table.Count(now.Add(2 * time.Second)); got != 1 {
		t.Fatalf("peer count=%d, want 1", got)
	}
}

func TestUpdateReusesExpiredEntry(t *testing.T) {
	var table Table
	now := time.Unix(200, 0)
	if !table.Update(testMessage(1), now) {
		t.Fatal("initial observation must create a peer")
	}
	if !table.Update(testMessage(2), now.Add(PeerTTL)) {
		t.Fatal("expired slot should be reused for a new peer")
	}
	if got := table.Count(now.Add(PeerTTL)); got != 1 {
		t.Fatalf("peer count=%d, want 1", got)
	}
}

func TestUpdateEvictsOldestAtCapacity(t *testing.T) {
	var table Table
	now := time.Unix(300, 0)
	for i := 1; i <= MaxPeers; i++ {
		if !table.Update(testMessage(byte(i)), now.Add(time.Duration(i)*time.Millisecond)) {
			t.Fatalf("peer %d should be new", i)
		}
	}
	if !table.Update(testMessage(250), now.Add(time.Second)) {
		t.Fatal("new peer should replace oldest entry")
	}
	if got := table.Count(now.Add(time.Second)); got != MaxPeers {
		t.Fatalf("peer count=%d, want %d", got, MaxPeers)
	}
	for _, p := range table.peers {
		if p.Used && p.AL[5] == 1 {
			t.Fatal("oldest peer was not evicted")
		}
	}
}

func testMessage(last byte) ieee1905.Message {
	return ieee1905.Message{
		ALMAC:   [6]byte{2, 0, 0, 0, 0, last},
		Src:     [6]byte{2, 0, 0, 1, 0, last},
		MsgType: ieee1905.MsgTopologyDiscovery, HasInterface: true,
		InterfaceMAC: [6]byte{2, 0, 0, 2, 0, last},
	}
}
