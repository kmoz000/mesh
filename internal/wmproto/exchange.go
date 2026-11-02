package wmproto

import "time"

// Controller holds the authenticated exchange state for the agents that report
// to one controller. It is the server side of the query/report/ack handshake:
//
//	controller --Query(challenge)--> agent
//	agent      --Report(challenge)--> controller   (echoes the challenge)
//	controller --Ack(challenge)----> agent
//
// The challenge is the primary replay defence: IssueChallenge stores a fresh,
// caller-supplied unpredictable ExchangeID per agent, and AcceptReport consumes
// it exactly once. A replayed Report therefore fails because its challenge has
// already been cleared. A per-(agent, boot) strictly increasing sequence orders
// reports within one boot, and a monotonic clock guard rejects time moving
// backwards. Controller performs no I/O and is not safe for concurrent use.
//
// MaxAgents bounds memory the same way topology.Table bounds peer state: a
// fixed array with hole-safe lookup and oldest-entry eviction at capacity.
const MaxAgents = 128

type agentState struct {
	used        bool
	id          NodeID
	boot        BootID
	haveBoot    bool
	seq         uint64
	haveSeq     bool
	challenge   ExchangeID
	pending     bool
	challengeAt time.Time
	lastSeen    time.Time
}

// Controller tracks pending challenges and accepted sequences per agent.
type Controller struct {
	agents [MaxAgents]agentState
	last   time.Time // most recent now observed, for the monotonic guard
}

// IssueChallenge records a fresh challenge for id and returns the slot it used.
// challenge must be unpredictable and non-repeating; Controller does not
// generate randomness. now is the issue time used for expiry. A second
// IssueChallenge for the same agent before a report replaces the pending
// challenge, so only the latest outstanding query can be answered.
func (c *Controller) IssueChallenge(id NodeID, challenge ExchangeID, now time.Time) error {
	if id == (NodeID{}) || challenge == (ExchangeID{}) {
		return ErrIdentity
	}
	if now.Before(c.last) {
		return ErrClock
	}
	c.last = now
	s := c.slotFor(id, now)
	if s == nil {
		return ErrCapacity
	}
	s.used = true
	s.id = id
	s.challenge = challenge
	s.pending = true
	s.challengeAt = now
	s.lastSeen = now
	return nil
}

// AcceptReport validates a decoded Report header against the agent's pending
// challenge and sequence state, then consumes the challenge. The caller must
// have already authenticated the datagram with Codec.Decode. On success the
// report may be processed; on error it must be dropped. Errors: ErrType if h is
// not a Report, ErrIdentity for an empty sender, ErrChallenge if no matching
// unexpired challenge is pending, ErrClock if now precedes the last observed
// time, and ErrReplay for a stale or repeated sequence within the same boot.
func (c *Controller) AcceptReport(h Header, now time.Time) error {
	if h.Type != Report {
		return ErrType
	}
	if h.Sender == (NodeID{}) {
		return ErrIdentity
	}
	if now.Before(c.last) {
		return ErrClock
	}
	c.last = now
	s := c.find(h.Sender)
	if s == nil || !s.pending || s.challenge != h.Exchange {
		return ErrChallenge
	}
	if now.Sub(s.challengeAt) > time.Duration(ReportLease)*time.Second {
		s.pending = false
		return ErrChallenge
	}
	// Sequence ordering is scoped to the boot. A new boot id is a legitimate
	// restart and resets the sequence; within a boot the sequence must strictly
	// increase. The one-shot challenge above already blocks cross-boot replay.
	if s.haveBoot && s.boot == h.Boot {
		if s.haveSeq && h.Sequence <= s.seq {
			return ErrReplay
		}
	}
	s.boot = h.Boot
	s.haveBoot = true
	s.seq = h.Sequence
	s.haveSeq = true
	s.pending = false
	s.lastSeen = now
	return nil
}

// Pending reports whether an unexpired challenge is outstanding for id.
func (c *Controller) Pending(id NodeID, now time.Time) bool {
	s := c.find(id)
	if s == nil || !s.pending {
		return false
	}
	return now.Sub(s.challengeAt) <= time.Duration(ReportLease)*time.Second
}

// Count returns the number of agents currently tracked.
func (c *Controller) Count() int {
	n := 0
	for i := range c.agents {
		if c.agents[i].used {
			n++
		}
	}
	return n
}

func (c *Controller) find(id NodeID) *agentState {
	for i := range c.agents {
		if c.agents[i].used && c.agents[i].id == id {
			return &c.agents[i]
		}
	}
	return nil
}

// slotFor returns the slot holding id, else a free slot, else the oldest entry.
func (c *Controller) slotFor(id NodeID, now time.Time) *agentState {
	free, oldest := -1, -1
	for i := range c.agents {
		s := &c.agents[i]
		if !s.used {
			if free < 0 {
				free = i
			}
			continue
		}
		if s.id == id {
			return s
		}
		if oldest < 0 || s.lastSeen.Before(c.agents[oldest].lastSeen) {
			oldest = i
		}
	}
	if free >= 0 {
		s := &c.agents[free]
		*s = agentState{}
		return s
	}
	if oldest >= 0 {
		s := &c.agents[oldest]
		*s = agentState{}
		return s
	}
	return nil
}
