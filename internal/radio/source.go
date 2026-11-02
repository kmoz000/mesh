package radio

import (
	"sync"
	"time"

	"wantasticmesh/internal/wmproto"
)

// Source adapts a capability snapshot to the daemon's RadioSource contract. The
// daemon wiring (in roles.go, done by the main session) treats any type with
//
//	Radios(dst []wmproto.RadioRecord, now time.Time) int
//
// as a radio source; Source matches that signature exactly. A refresher updates
// the snapshot with Set; Radios reads it. Both are safe to call from different
// goroutines.
type Source struct {
	mu   sync.Mutex
	caps Capabilities
}

// NewSource returns a Source seeded with caps (which may be the zero value).
func NewSource(caps Capabilities) *Source {
	return &Source{caps: caps}
}

// Set replaces the snapshot. The refresher calls this after each probe cycle.
func (s *Source) Set(caps Capabilities) {
	s.mu.Lock()
	s.caps = caps
	s.mu.Unlock()
}

// Snapshot returns a shallow copy of the current capabilities for planning.
func (s *Source) Snapshot() Capabilities {
	s.mu.Lock()
	defer s.mu.Unlock()
	cp := Capabilities{Radios: make([]Radio, len(s.caps.Radios))}
	copy(cp.Radios, s.caps.Radios)
	return cp
}

// Radios fills dst with the current radio records and returns the count written.
// It writes at most min(len(dst), wmproto.MaxRadios, number of radios). now is
// part of the RadioSource contract; this source reports last-known state and
// does not stamp per-call freshness, so now is unused here. No allocation.
func (s *Source) Radios(dst []wmproto.RadioRecord, now time.Time) int {
	_ = now
	s.mu.Lock()
	defer s.mu.Unlock()
	n := len(s.caps.Radios)
	if n > len(dst) {
		n = len(dst)
	}
	if n > wmproto.MaxRadios {
		n = wmproto.MaxRadios
	}
	for i := 0; i < n; i++ {
		dst[i] = s.caps.Radios[i].toRecord()
	}
	return n
}
