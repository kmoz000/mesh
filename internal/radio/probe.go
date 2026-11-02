package radio

// Pure nl80211 reply parsers used by the Linux probe (probe_linux via the
// socket) to fill a Radio's capability and operating state. They are pure so
// they are tested on crafted attribute buffers, exactly like genlmsg.go's
// codec; the socket round-trip that produces the bytes lives in the linux file.
//
// Honesty rule: a parser sets a Known bit only for a field it actually read.
// Width capability (HT/VHT/HE/EHT decode) is not parsed yet and is PENDING, so
// Radio.Widths is left for a caller to supply; the validator then refuses a
// width it was not told the radio supports rather than assuming one.

// applyWiphyAttrs fills a radio's band, channel list and tx-power bound from the
// attribute region of an NL80211_CMD_NEW_WIPHY (GET_WIPHY) reply. A split dump
// delivers a wiphy across several messages; calling this for each message
// accumulates channels. It returns the parse error, if any.
func applyWiphyAttrs(attrs []byte, r *Radio) error {
	it := newAttrIter(attrs)
	for {
		atype, payload, ok := it.next()
		if !ok {
			break
		}
		if atype == nl80211AttrWiphyBands {
			if err := applyBands(payload, r); err != nil {
				return err
			}
		}
	}
	return it.err
}

// applyBands walks the nested WIPHY_BANDS array (each entry is one band nest).
func applyBands(payload []byte, r *Radio) error {
	bands := newAttrIter(payload)
	for {
		_, bandAttrs, ok := bands.next()
		if !ok {
			break
		}
		inner := newAttrIter(bandAttrs)
		for {
			atype, p, ok := inner.next()
			if !ok {
				break
			}
			if atype == nl80211BandAttrFreqs {
				if err := applyFreqs(p, r); err != nil {
					return err
				}
			}
		}
		if inner.err != nil {
			return inner.err
		}
	}
	return bands.err
}

// applyFreqs walks the nested FREQS array, adding one Channel per frequency nest
// and tracking band and tx-power bound.
func applyFreqs(payload []byte, r *Radio) error {
	freqs := newAttrIter(payload)
	for {
		_, freqAttrs, ok := freqs.next()
		if !ok {
			break
		}
		var ch Channel
		haveFreq := false
		it := newAttrIter(freqAttrs)
		for {
			atype, p, ok := it.next()
			if !ok {
				break
			}
			switch atype {
			case nl80211FreqAttrFreq:
				if v, ok := attrU32(p); ok {
					ch.Freq = uint16(v)
					ch.Num = channelForFreq(uint16(v))
					haveFreq = true
				}
			case nl80211FreqAttrDisabled:
				ch.Disabled = true
			case nl80211FreqAttrNoIR:
				ch.NoIR = true
			case nl80211FreqAttrRadar:
				ch.Radar = true
			case nl80211FreqAttrMaxTxPower:
				if v, ok := attrU32(p); ok {
					ch.MaxTxPower = mBmToDBm(v)
				}
			}
		}
		if it.err != nil {
			return it.err
		}
		if !haveFreq {
			continue
		}
		addChannel(r, ch)
	}
	return freqs.err
}

// addChannel appends a channel and updates the derived band and tx-power range.
func addChannel(r *Radio, ch Channel) {
	r.Channels = append(r.Channels, ch)
	if b := bandForFreq(ch.Freq); b != BandUnknown {
		r.Band = b
		r.Known |= KnownBand
	}
	if ch.MaxTxPower != 0 {
		if r.Known&KnownRange == 0 || ch.MaxTxPower > r.TxPowerMax {
			r.TxPowerMax = ch.MaxTxPower
		}
		// TxPowerMin stays at the driver floor (0 dBm) until a device exposes a
		// real minimum; the validator only needs an upper bound to stay safe.
		r.Known |= KnownRange
	}
}

// mBmToDBm converts milli-dBm to whole dBm, clamped to int8 range.
func mBmToDBm(mBm uint32) int8 {
	d := int32(mBm) / 100
	if d > 127 {
		return 127
	}
	if d < -128 {
		return -128
	}
	return int8(d)
}

// parseInterfaceMLD extracts MLD membership from an NL80211_CMD_NEW_INTERFACE
// (GET_INTERFACE) reply. It reads a top-level MLO_LINK_ID / MLD_ADDR, and also
// a per-link id nested under MLO_LINKS, because MLO kernels differ in shape.
// Which shape the running kernel emits is PENDING device confirmation; both are
// parsed and tested. linkID is only valid when hasLink is true.
func parseInterfaceMLD(attrs []byte) (linkID uint8, addr [6]byte, hasLink, hasAddr bool, err error) {
	it := newAttrIter(attrs)
	for {
		atype, p, ok := it.next()
		if !ok {
			break
		}
		switch atype {
		case nl80211AttrMLOLinkID:
			if len(p) >= 1 {
				linkID = p[0]
				hasLink = true
			}
		case nl80211AttrMLDAddr:
			if len(p) >= 6 {
				copy(addr[:], p[:6])
				hasAddr = true
			}
		case nl80211AttrMLOLinks:
			if id, ok := firstLinkID(p); ok && !hasLink {
				linkID = id
				hasLink = true
			}
		}
	}
	if it.err != nil {
		return 0, [6]byte{}, false, false, it.err
	}
	return linkID, addr, hasLink, hasAddr, nil
}

// firstLinkID returns the first MLO_LINK_ID found in a nested MLO_LINKS array.
func firstLinkID(payload []byte) (uint8, bool) {
	outer := newAttrIter(payload)
	for {
		_, link, ok := outer.next()
		if !ok {
			break
		}
		inner := newAttrIter(link)
		for {
			atype, p, ok := inner.next()
			if !ok {
				break
			}
			if atype == nl80211AttrMLOLinkID && len(p) >= 1 {
				return p[0], true
			}
		}
	}
	return 0, false
}

// parseTxPowerLevelDBm reads NL80211_ATTR_WIPHY_TX_POWER_LEVEL (u32 mBm) from a
// GET_INTERFACE or GET_WIPHY reply and converts it to whole dBm. ok is false
// when the attribute is absent. This is the confirmed tx-power read-back: the
// same value `iw dev <iface> info` prints as "txpower NN.NN dBm". It iterates
// with the pure attribute walker (not the linux-only topU32) so it compiles and
// is tested on any host.
func parseTxPowerLevelDBm(attrs []byte) (int, bool) {
	it := newAttrIter(attrs)
	for {
		atype, p, ok := it.next()
		if !ok {
			break
		}
		if atype == nl80211AttrWiphyTxPowerLevel {
			if v, ok := attrU32(p); ok {
				return int(int32(v)) / 100, true
			}
		}
	}
	return 0, false
}

// parseOperChannel reads the operating channel width and center frequency from a
// GET_INTERFACE / GET_WIPHY reply (NL80211_ATTR_CHANNEL_WIDTH, WIPHY_FREQ). It
// returns the width in MHz and the primary channel number, with ok flags.
func parseOperChannel(attrs []byte) (width uint16, haveWidth bool, chNum uint16, haveCh bool, err error) {
	it := newAttrIter(attrs)
	for {
		atype, p, ok := it.next()
		if !ok {
			break
		}
		switch atype {
		case nl80211AttrChannelWidth:
			if v, ok := attrU32(p); ok {
				width = chanWidthToMHz(v)
				haveWidth = width != 0
			}
		case nl80211AttrWiphyFreq:
			if v, ok := attrU32(p); ok {
				chNum = channelForFreq(uint16(v))
				haveCh = chNum != 0
			}
		}
	}
	if it.err != nil {
		return 0, false, 0, false, it.err
	}
	return width, haveWidth, chNum, haveCh, nil
}
