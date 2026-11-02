// Package topology provides bounded neighbor state for the daemon.
package topology

import (
	"time"

	"wantasticmesh/internal/ieee1905"
)

const (
	MaxPeers = 128
	PeerTTL  = 60 * time.Second
)

type Peer struct {
	Used     bool
	AL       [6]byte
	IfMAC    [6]byte
	Source   [6]byte
	Frames   uint32
	LastType uint16
	LastID   uint16
	LastSeen time.Time
}

// Table stores a fixed number of peers without per-update allocation.
type Table struct {
	peers [MaxPeers]Peer
}

// Update records a validated topology message. It returns true for a new peer.
func (t *Table) Update(msg ieee1905.Message, now time.Time) bool {
	found, free, oldest := -1, -1, -1
	for i := range t.peers {
		p := &t.peers[i]
		if !p.Used {
			if free < 0 {
				free = i
			}
			continue
		}
		if now.Sub(p.LastSeen) >= PeerTTL {
			*p = Peer{}
			if free < 0 {
				free = i
			}
			continue
		}
		if p.AL == msg.ALMAC {
			found = i
		}
		if oldest < 0 || p.LastSeen.Before(t.peers[oldest].LastSeen) {
			oldest = i
		}
	}
	idx := found
	if idx < 0 {
		idx = free
	}
	if idx < 0 {
		idx = oldest
	}
	isNew := found < 0
	if isNew {
		t.peers[idx] = Peer{Used: true, AL: msg.ALMAC, Source: msg.Src}
	}
	p := &t.peers[idx]
	p.Frames++
	p.LastType = msg.MsgType
	p.LastID = msg.MessageID
	p.LastSeen = now
	if msg.HasInterface {
		p.IfMAC = msg.InterfaceMAC
	}
	return isNew
}

// Count expires stale entries and reports the number still retained.
func (t *Table) Count(now time.Time) int {
	n := 0
	for i := range t.peers {
		p := &t.peers[i]
		if p.Used && now.Sub(p.LastSeen) >= PeerTTL {
			*p = Peer{}
		}
		if p.Used {
			n++
		}
	}
	return n
}
