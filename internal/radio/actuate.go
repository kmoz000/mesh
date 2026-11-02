package radio

import (
	"errors"
	"strconv"
)

// Pure actuation builders: the exact hostapd_cli chan_switch argument vector and
// the nl80211 SET_WIPHY tx-power message. These contain no I/O so they are
// tested on crafted inputs; actuator_linux.go feeds their output to os/exec and
// the netlink socket. Getting the CSA center frequency right is the delicate
// part, so it is computed from the standard 802.11 segment tables (5 GHz) and
// the regular 6 GHz channel grid, and a width/channel combination that cannot be
// placed is refused rather than guessed.

var (
	ErrCSAUnsupported = errors.New("radio: cannot compute a CSA center frequency for this channel/width")
	ErrCSANoNetdev    = errors.New("radio: CSA requires a known AP netdev")
	ErrNotChannelCh   = errors.New("radio: change is not a channel change")
	ErrNotPowerCh     = errors.New("radio: change is not a tx-power change")
)

// chanToFreq converts a channel number to its 20 MHz center frequency (MHz) for
// a band. It is the inverse of channelForFreq.
func chanToFreq(b Band, ch uint16) uint16 {
	switch b {
	case Band24:
		if ch == 14 {
			return 2484
		}
		return 2407 + 5*ch
	case Band5:
		return 5000 + 5*ch
	case Band6:
		if ch == 2 {
			return 5935
		}
		return 5950 + 5*ch
	default:
		return 0
	}
}

// n20For returns the number of 20 MHz subchannels in a width, or 0 for a width
// this layer does not place.
func n20For(width uint16) uint16 {
	switch width {
	case 20:
		return 1
	case 40:
		return 2
	case 80:
		return 4
	case 160:
		return 8
	case 320:
		return 16
	default:
		return 0
	}
}

// 5 GHz segment tables: each inner slice is the ordered set of 20 MHz channels
// in one segment of the given width. The segment center channel is
// (first+last)/2. These are the standard 802.11 blocks (the 5 GHz grid is not
// uniformly aligned — UNII-3 starts at 149 — so it cannot be computed and is
// tabulated). Verifiable against 802.11 operating-class tables.
var (
	band5seg40 = [][]uint16{
		{36, 40}, {44, 48}, {52, 56}, {60, 64},
		{100, 104}, {108, 112}, {116, 120}, {124, 128},
		{132, 136}, {140, 144}, {149, 153}, {157, 161},
		{165, 169}, {173, 177},
	}
	band5seg80 = [][]uint16{
		{36, 40, 44, 48}, {52, 56, 60, 64},
		{100, 104, 108, 112}, {116, 120, 124, 128},
		{132, 136, 140, 144}, {149, 153, 157, 161},
		{165, 169, 173, 177},
	}
	band5seg160 = [][]uint16{
		{36, 40, 44, 48, 52, 56, 60, 64},
		{100, 104, 108, 112, 116, 120, 124, 128},
		{149, 153, 157, 161, 165, 169, 173, 177},
	}
)

func seg5For(width uint16) [][]uint16 {
	switch width {
	case 40:
		return band5seg40
	case 80:
		return band5seg80
	case 160:
		return band5seg160
	default:
		return nil
	}
}

// centerChan5 returns the center channel and the segment for a 5 GHz primary
// channel at the given width, or ok=false if the primary is not in any segment.
func centerChan5(primary, width uint16) (center uint16, seg []uint16, ok bool) {
	for _, s := range seg5For(width) {
		for _, ch := range s {
			if ch == primary {
				return (s[0] + s[len(s)-1]) / 2, s, true
			}
		}
	}
	return 0, nil, false
}

// centerChan6 computes the center channel for a 6 GHz primary at a width. The
// 6 GHz grid is regular (channels 1,5,9,... step 4), so the segment is aligned
// by arithmetic.
func centerChan6(primary, width uint16) (center uint16, lowest uint16, ok bool) {
	n20 := n20For(width)
	if n20 == 0 || primary < 1 {
		return 0, 0, false
	}
	span := 4 * n20             // channel-number span of the segment
	idx := (primary - 1) / span // which aligned block
	lowest = 1 + idx*span
	center = lowest + 2*(n20-1)
	return center, lowest, true
}

// csaPlan is the computed CSA geometry for a channel change.
type csaPlan struct {
	primaryFreq uint16
	centerFreq1 uint16
	bandwidth   uint16
	secOffset   int // -1, 0, +1 (HT sec channel offset, only meaningful at 40 MHz)
}

// planCSA computes the CSA frequencies for a radio's change. It uses the
// radio's band and (where present) its capability channel list for the exact
// primary frequency, falling back to the band grid. It refuses a combination it
// cannot place.
func planCSA(r *Radio, ch, width uint16) (csaPlan, error) {
	var p csaPlan
	p.bandwidth = width
	// primary 20 MHz center frequency.
	if cc := r.channel(ch); cc != nil && cc.Freq != 0 {
		p.primaryFreq = cc.Freq
	} else {
		p.primaryFreq = chanToFreq(r.Band, ch)
	}
	if p.primaryFreq == 0 {
		return csaPlan{}, ErrCSAUnsupported
	}
	if width == 20 {
		p.centerFreq1 = p.primaryFreq
		return p, nil
	}
	switch r.Band {
	case Band24:
		if width != 40 {
			return csaPlan{}, ErrCSAUnsupported
		}
		// 2.4 GHz HT40: use HT40+ (secondary above, center +10 MHz) when the
		// secondary channel (ch+4) still fits in the band (ch <= 9, top channel
		// 13); otherwise HT40- (secondary below, center -10 MHz). Emitting +1
		// unconditionally made a switch/revert to a high channel (e.g. 10..13)
		// fail, because the secondary would fall outside the band.
		if ch+4 <= chan24Max {
			p.secOffset = 1
			p.centerFreq1 = p.primaryFreq + 10
		} else {
			p.secOffset = -1
			p.centerFreq1 = p.primaryFreq - 10
		}
		return p, nil
	case Band5:
		center, seg, ok := centerChan5(ch, width)
		if !ok {
			return csaPlan{}, ErrCSAUnsupported
		}
		p.centerFreq1 = chanToFreq(Band5, center)
		if width == 40 {
			if ch == seg[0] {
				p.secOffset = 1
			} else {
				p.secOffset = -1
			}
		}
		return p, nil
	case Band6:
		center, lowest, ok := centerChan6(ch, width)
		if !ok {
			return csaPlan{}, ErrCSAUnsupported
		}
		p.centerFreq1 = chanToFreq(Band6, center)
		if width == 40 {
			if ch == lowest {
				p.secOffset = 1
			} else {
				p.secOffset = -1
			}
		}
		return p, nil
	default:
		return csaPlan{}, ErrCSAUnsupported
	}
}

// hostapdChanSwitchArgs builds the hostapd_cli command vector for a channel
// change, excluding the "-i <iface>" prefix (returned separately). csCount is
// the number of beacons to announce the switch over (CSA count); phyMode is the
// hostapd width keyword (ht/vht/he/eht) and defaults by band/width when empty.
// The hostapd control path and phyMode are PENDING device confirmation.
func hostapdChanSwitchArgs(r *Radio, c Change, csCount int, phyMode string) (iface string, args []string, err error) {
	if !c.SetChannel {
		return "", nil, ErrNotChannelCh
	}
	if r.Netdev == "" {
		return "", nil, ErrCSANoNetdev
	}
	p, err := planCSA(r, c.Channel, c.Width)
	if err != nil {
		return "", nil, err
	}
	args = []string{"chan_switch", strconv.Itoa(csCount), strconv.Itoa(int(p.primaryFreq))}
	if p.secOffset != 0 {
		args = append(args, "sec_channel_offset="+strconv.Itoa(p.secOffset))
	}
	if p.bandwidth > 20 {
		args = append(args, "center_freq1="+strconv.Itoa(int(p.centerFreq1)))
	}
	args = append(args, "bandwidth="+strconv.Itoa(int(p.bandwidth)))
	args = append(args, defaultPhyMode(r.Band, c.Width, phyMode))
	return r.Netdev, args, nil
}

// defaultPhyMode picks a hostapd width keyword. hostapd requires one of
// ht/vht/he/eht for a width above 20 MHz; the real generation a radio supports
// is device-dependent and PENDING, so a caller may override via phyMode.
func defaultPhyMode(b Band, width uint16, phyMode string) string {
	if phyMode != "" {
		return phyMode
	}
	switch {
	case width >= 320:
		return "eht"
	case b == Band6:
		return "he"
	case width >= 80:
		return "vht"
	case width == 40:
		return "ht"
	default:
		return "ht"
	}
}

// buildSetWiphyTxPower builds an NL80211_CMD_SET_WIPHY message that sets the tx
// power to dBm, scoped to the radio's wiphy (NL80211_ATTR_WIPHY), using the
// LIMITED setting: the driver treats dBm as a ceiling it clamps down to the
// nearest regulatory step, which is why the read-back verify tolerates +/-1 dBm
// rather than demanding exact equality. familyID is the resolved nl80211 generic
// family id (never hardcoded). The level attribute is in mBm (dBm * 100).
// buildSetWiphyTxPower builds a SET_WIPHY request that limits tx power on one
// interface. nl80211 tx power is set per-interface (the wdev), the same way
// `iw dev <iface> set txpower limit <mBm>` does it; targeting the wiphy index
// instead is silently ignored by the QCA driver, so the request carries
// NL80211_ATTR_IFINDEX. LIMITED means the level is an upper bound the driver may
// stay under, which is the safe choice for a mesh (never exceed the cap).
func buildSetWiphyTxPower(buf []byte, familyID uint16, ifindex uint32, dBm int8, seq, pid uint32) ([]byte, error) {
	w := newBuilder(buf)
	w.putHeader(familyID, nlmFRequest|nlmFAck, seq, pid, nl80211CmdSetWiphy, 0)
	w.putU32(nl80211AttrIfIndex, ifindex)
	w.putU32(nl80211AttrWiphyTxPowerSetng, txPowerLimited)
	w.putU32(nl80211AttrWiphyTxPowerLevel, uint32(int32(dBm)*100))
	return w.finalize()
}
