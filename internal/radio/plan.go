package radio

import "errors"

// Mechanism is the owner of a given reconfiguration. Routing is a pure function
// of the change so a test can assert the change never reaches the wrong owner -
// in particular that a channel change never becomes a raw nl80211 SET_CHANNEL.
//
// There is deliberately no vendor mechanism. Vendor channel-preference and
// backhaul-scan work is not a reconfiguration this layer owns: it is *triggered*
// through the ezmesh control FIFO (see vendoripc.go) and the vendor stack does
// the radio-safe work itself. A FIFO trigger has no prior state to verify or
// roll back, so it is never modelled as a Change / Mechanism.
type Mechanism uint8

const (
	MechNone         Mechanism = iota
	MechHostapdCSA             // channel/width: hostapd_cli chan_switch (CSA, keeps clients)
	MechNL80211Power           // tx power: nl80211 SET_WIPHY
)

func (m Mechanism) String() string {
	switch m {
	case MechHostapdCSA:
		return "hostapd-csa"
	case MechNL80211Power:
		return "nl80211-txpower"
	default:
		return "none"
	}
}

// Change is one requested reconfiguration of a single radio. A channel change
// and a tx-power change are modelled as separate changes so each routes to its
// own owner; a caller that wants both sequences two transactions.
type Change struct {
	RadioIndex uint8

	SetChannel bool
	Channel    uint16
	Width      uint16 // MHz

	SetTxPower bool
	TxPower    int8 // dBm
}

// Validation / planning errors.
var (
	ErrEmptyChange = errors.New("radio: change requests nothing")
	ErrMixedChange = errors.New("radio: a change must target one mechanism")
)

// Mechanism returns the single owner for this change, or an error if the change
// is empty or spans more than one owner.
func (c Change) Mechanism() (Mechanism, error) {
	n := 0
	var m Mechanism
	if c.SetChannel {
		n++
		m = MechHostapdCSA
	}
	if c.SetTxPower {
		n++
		m = MechNL80211Power
	}
	switch {
	case n == 0:
		return MechNone, ErrEmptyChange
	case n > 1:
		return MechNone, ErrMixedChange
	default:
		return m, nil
	}
}

// Validate checks a change against capability before anything is actuated:
//   - resolvable to a single mechanism,
//   - channel+width in the radio's capability set (and channel usable),
//   - tx power within the radio's reported range (refused if range unknown).
//
// It performs no side effect and never relaxes a limit.
func Validate(caps *Capabilities, c Change) (Mechanism, error) {
	m, err := c.Mechanism()
	if err != nil {
		return MechNone, err
	}
	if caps.radio(c.RadioIndex) == nil {
		return MechNone, ErrNoRadio
	}
	switch m {
	case MechHostapdCSA:
		if err := caps.SupportsChannel(c.RadioIndex, c.Channel, c.Width); err != nil {
			return MechNone, err
		}
	case MechNL80211Power:
		if err := caps.CheckTxPower(c.RadioIndex, c.TxPower); err != nil {
			return MechNone, err
		}
	}
	return m, nil
}
