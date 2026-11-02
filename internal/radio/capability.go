package radio

import (
	"errors"

	"wantasticmesh/internal/wmproto"
)

// Band values match wmproto.RadioRecord.Band (0 unknown, 1=2.4, 2=5, 3=6).
type Band uint8

const (
	BandUnknown Band = 0
	Band24      Band = 1
	Band5       Band = 2
	Band6       Band = 3
)

// MLDLinkNone is the RadioRecord.MLDLink sentinel for "not part of an MLD".
const MLDLinkNone uint8 = 0xff

// Known-field bits for Radio. The capability enumerator fills what it can prove
// and leaves the rest zero; a consumer must consult Known before trusting a
// zero value, because zero is also a legal channel/power reading.
const (
	KnownBand    uint32 = 1 << 0
	KnownChannel uint32 = 1 << 1
	KnownWidth   uint32 = 1 << 2
	KnownTxPower uint32 = 1 << 3
	KnownUtil    uint32 = 1 << 4
	KnownRange   uint32 = 1 << 5 // tx-power range is known
)

// Channel is one supported channel with the regulatory flags that gate use.
type Channel struct {
	Num        uint16 // channel number
	Freq       uint16 // center frequency, MHz
	MaxTxPower int8   // regulatory cap, dBm (from FREQUENCY_ATTR_MAX_TX_POWER)
	Disabled   bool   // not usable at all
	NoIR       bool   // no initiating radiation (passive) -> cannot start a BSS
	Radar      bool   // DFS channel; CAC owned by hostapd, never by this layer
}

// Radio is one physical radio's capability plus last-known operating state.
// Capability fields (Band, Channels, Widths, TxPower range) describe what the
// radio can do; the Cur* fields and Utilization are telemetry and are only
// meaningful when the matching Known bit is set.
type Radio struct {
	Index    uint8  // phy index (from sysfs)
	Phy      string // phy name, e.g. "phy0"
	Netdev   string // associated AP netdev, e.g. "wlan0" (empty if unknown)
	Band     Band
	Channels []Channel
	Widths   []uint16 // supported bandwidths, MHz (20,40,80,160,320)

	TxPowerMin int8 // dBm
	TxPowerMax int8 // dBm

	MLDLink  uint8 // 0xff when not in an MLD
	Backhaul bool  // radio carries a mesh backhaul link
	MLO      bool  // radio participates in an MLD

	CurChannel  uint16
	CurWidth    uint16
	CurTxPower  int8
	Utilization uint8
	Known       uint32
}

func newRadio() Radio { return Radio{MLDLink: MLDLinkNone} }

// hasWidth reports whether w (MHz) is a supported bandwidth. The radio's
// current operating width counts as supported even when the capability decode
// (HT/VHT/HE/EHT) has not populated Widths: a width the radio is already using
// is self-evidently available, so preserving it across a channel switch is safe.
func (r *Radio) hasWidth(w uint16) bool {
	if w != 0 && w == r.CurWidth {
		return true
	}
	for _, x := range r.Widths {
		if x == w {
			return true
		}
	}
	return false
}

// channel returns the capability entry for channel num, or nil.
func (r *Radio) channel(num uint16) *Channel {
	for i := range r.Channels {
		if r.Channels[i].Num == num {
			return &r.Channels[i]
		}
	}
	return nil
}

// usableChannel reports whether num may be operated: it must be a listed
// channel, not disabled, and not NoIR (which forbids starting a BSS). Radar
// channels are allowed here because hostapd owns CAC; the transaction layer
// still routes them through hostapd only.
func (r *Radio) usableChannel(num uint16) bool {
	c := r.channel(num)
	if c == nil {
		return false
	}
	return !c.Disabled && !c.NoIR
}

// Capabilities is a snapshot of every radio a node reports. It is bounded by
// wmproto.MaxRadios on emit. A nil or empty snapshot means "nothing probed yet".
type Capabilities struct {
	Radios []Radio
}

// radio returns the radio with phy index idx, or nil.
func (c *Capabilities) radio(idx uint8) *Radio {
	for i := range c.Radios {
		if c.Radios[i].Index == idx {
			return &c.Radios[i]
		}
	}
	return nil
}

// backhaulCount returns how many radios are flagged as backhaul.
func (c *Capabilities) backhaulCount() int {
	n := 0
	for i := range c.Radios {
		if c.Radios[i].Backhaul {
			n++
		}
	}
	return n
}

// soleBackhaul reports whether idx is the only backhaul radio. Retuning it must
// carry a watchdog so a failed change cannot strand the node.
func (c *Capabilities) soleBackhaul(idx uint8) bool {
	r := c.radio(idx)
	if r == nil || !r.Backhaul {
		return false
	}
	return c.backhaulCount() == 1
}

// Capability/validation errors.
var (
	ErrNoRadio      = errors.New("radio: no such radio index")
	ErrChannel      = errors.New("radio: channel not in capability set")
	ErrWidth        = errors.New("radio: width not supported by radio")
	ErrTxPowerRange = errors.New("radio: tx power outside radio range")
	ErrTxPowerUnkn  = errors.New("radio: tx power range unknown, refusing change")
)

// SupportsChannel validates a channel+width request against a radio's reported
// capability. It never consults regulatory policy beyond the per-channel flags
// already captured at enumeration time.
func (c *Capabilities) SupportsChannel(idx uint8, ch, width uint16) error {
	r := c.radio(idx)
	if r == nil {
		return ErrNoRadio
	}
	if !r.usableChannel(ch) {
		return ErrChannel
	}
	if !r.hasWidth(width) {
		return ErrWidth
	}
	return nil
}

// CheckTxPower validates a tx-power request (dBm) against a radio's range. If
// the range was never probed it refuses rather than guess.
func (c *Capabilities) CheckTxPower(idx uint8, dBm int8) error {
	r := c.radio(idx)
	if r == nil {
		return ErrNoRadio
	}
	if r.Known&KnownRange == 0 {
		return ErrTxPowerUnkn
	}
	if dBm < r.TxPowerMin || dBm > r.TxPowerMax {
		return ErrTxPowerRange
	}
	return nil
}

// bandForFreq classifies a center frequency (MHz) into a band.
func bandForFreq(freq uint16) Band {
	switch {
	case freq >= 2400 && freq <= 2500:
		return Band24
	case freq >= 4900 && freq < 5950:
		return Band5
	case freq >= 5950 && freq <= 7125:
		return Band6
	default:
		return BandUnknown
	}
}

// channelForFreq converts a center frequency (MHz) to a channel number. It
// returns 0 for a frequency it does not recognise.
func channelForFreq(freq uint16) uint16 {
	switch {
	case freq == 2484:
		return 14
	case freq >= 2412 && freq <= 2472:
		return (freq - 2407) / 5
	case freq == 5935:
		return 2 // 6 GHz channel 2 special case
	case freq >= 5955 && freq <= 7115:
		return (freq - 5950) / 5
	case freq >= 5000 && freq < 5950:
		return (freq - 5000) / 5
	default:
		return 0
	}
}

// chanWidthToMHz maps an nl80211_chan_width value to a bandwidth in MHz, or 0
// for a value this layer does not model.
func chanWidthToMHz(w uint32) uint16 {
	switch w {
	case chanWidth20NoHT, chanWidth20:
		return 20
	case chanWidth40:
		return 40
	case chanWidth80:
		return 80
	case chanWidth80P80, chanWidth160:
		return 160
	case chanWidth320:
		return 320
	default:
		return 0
	}
}

// toRecord fills a wmproto.RadioRecord from a radio's last-known state. Unknown
// fields are emitted as zero (RadioRecord has no approximate bit; zero is the
// honest value and the Known bitmask is internal to this package).
func (r *Radio) toRecord() wmproto.RadioRecord {
	var rec wmproto.RadioRecord
	rec.Index = r.Index
	if r.Known&KnownBand != 0 {
		rec.Band = uint8(r.Band)
	}
	if r.Known&KnownChannel != 0 {
		rec.Channel = r.CurChannel
	}
	if r.Known&KnownWidth != 0 {
		rec.Width = r.CurWidth
	}
	if r.Known&KnownTxPower != 0 {
		rec.TxPower = r.CurTxPower
	}
	if r.Known&KnownUtil != 0 {
		rec.Utilization = r.Utilization
	}
	rec.MLDLink = r.MLDLink
	if r.Backhaul {
		rec.Flags |= wmproto.RadioBackhaul
	}
	if r.MLO {
		rec.Flags |= wmproto.RadioMLO
	}
	return rec
}
