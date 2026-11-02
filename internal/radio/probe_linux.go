//go:build linux

package radio

// LinuxProber assembles a Capabilities snapshot from sysfs enumeration plus
// standard nl80211: GET_WIPHY (a split dump) for per-band channel lists, the
// derived band, and a regulatory tx-power bound; GET_INTERFACE per netdev for
// the operating channel/width and MLD membership (MLD_ADDR / MLO_LINK_ID — all
// standard nl80211, no vendor command). Fields it cannot prove are left at an
// honest zero with no Known bit; in particular per-radio width capability
// (HT/VHT/HE/EHT decode) is not parsed yet (PENDING) and Widths stays empty, so
// a width change is refused until a device fills it.
//
// DEVICE EXECUTION IS PENDING: this has not run against a real kernel. The
// refresher calls Probe on a cadence and feeds the result to Source.Set.
type LinuxProber struct {
	// Root is the sysfs root for enumeration (default "/"; tests of the pure
	// parts set a fake tree, but this file itself needs a live socket).
	Root string

	conn      *genlConn
	nl80211ID uint16
}

// NewLinuxProber opens a socket and resolves the nl80211 family id.
func NewLinuxProber(root string) (*LinuxProber, error) {
	c, err := openGenl()
	if err != nil {
		return nil, err
	}
	res := &LinuxFamilyResolver{conn: c}
	fi, err := res.Resolve(genlFamilyNameNL80211)
	if err != nil {
		c.Close()
		return nil, err
	}
	return &LinuxProber{Root: root, conn: c, nl80211ID: fi.ID}, nil
}

// Close releases the socket.
func (p *LinuxProber) Close() error {
	if p == nil || p.conn == nil {
		return nil
	}
	return p.conn.Close()
}

// Probe returns a fresh capability snapshot.
func (p *LinuxProber) Probe() (Capabilities, error) {
	en := Enumerator{Root: p.Root}
	radios, err := en.Enumerate()
	if err != nil {
		return Capabilities{}, err
	}
	caps := Capabilities{Radios: radios}

	if err := p.fillWiphy(&caps); err != nil {
		return caps, err
	}
	for i := range caps.Radios {
		if caps.Radios[i].Netdev == "" {
			continue
		}
		_ = p.fillInterface(&caps.Radios[i]) // best-effort per interface
	}
	return caps, nil
}

// fillWiphy runs one GET_WIPHY split dump and routes each message's band data to
// the radio whose phy index matches NL80211_ATTR_WIPHY.
func (p *LinuxProber) fillWiphy(caps *Capabilities) error {
	scratch := make([]byte, 512)
	return p.conn.execute(true,
		func(seq uint32) ([]byte, error) {
			w := newBuilder(scratch)
			w.putHeader(p.nl80211ID, nlmFRequest|nlmFDump, seq, p.conn.pid, nl80211CmdGetWiphy, 0)
			w.putFlag(nl80211AttrSplitWiphyDump)
			return w.finalize()
		},
		func(_ uint8, attrs []byte) error {
			idx, ok := topU32(attrs, nl80211AttrWiphy)
			if !ok {
				return nil
			}
			r := caps.radio(uint8(idx))
			if r == nil {
				return nil
			}
			return applyWiphyAttrs(attrs, r)
		},
	)
}

// fillInterface reads a radio's operating channel/width and MLD membership.
func (p *LinuxProber) fillInterface(r *Radio) error {
	ifindex, err := ifindexOf(r.Netdev)
	if err != nil {
		return err
	}
	scratch := make([]byte, 256)
	// Use a GET_INTERFACE *dump* matched by ifindex, not a filtered doit request:
	// on the QCA 5.4 target the doit GET_INTERFACE reply is not delivered to the
	// handler (the same reason actuator_linux.go's operChannelViaInterface dumps),
	// so a doit here left CurChannel/CurWidth unset and radios reported channel 0.
	return p.conn.execute(true,
		func(seq uint32) ([]byte, error) {
			w := newBuilder(scratch)
			w.putHeader(p.nl80211ID, nlmFRequest|nlmFDump, seq, p.conn.pid, nl80211CmdGetInterface, 0)
			return w.finalize()
		},
		func(_ uint8, attrs []byte) error {
			if idx, ok := topU32(attrs, nl80211AttrIfIndex); !ok || idx != ifindex {
				return nil
			}
			if width, hw, ch, hc, perr := parseOperChannel(attrs); perr == nil {
				if hw {
					r.CurWidth = width
					r.Known |= KnownWidth
				}
				if hc {
					r.CurChannel = ch
					r.Known |= KnownChannel
				}
			}
			if linkID, _, hasLink, _, perr := parseInterfaceMLD(attrs); perr == nil && hasLink {
				r.MLDLink = linkID
				r.MLO = true
			}
			return nil
		},
	)
}

// topU32 returns a top-level u32 attribute by type.
func topU32(attrs []byte, atype uint16) (uint32, bool) {
	it := newAttrIter(attrs)
	for {
		t, p, ok := it.next()
		if !ok {
			break
		}
		if t == atype {
			return attrU32(p)
		}
	}
	return 0, false
}
