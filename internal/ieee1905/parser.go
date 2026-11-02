// Package ieee1905 contains a bounded parser for Ethernet-carried IEEE 1905.1 CMDUs.
package ieee1905

import "encoding/binary"

const (
	EtherType               = 0x893a
	EtherHdr                = 14
	CMDUHdr                 = 8
	MaxTLVs                 = 256
	TLVEnd                  = 0x00
	TLVALMAC                = 0x01
	TLVInterface            = 0x02
	MsgTopologyDiscovery    = 0x0000
	MsgTopologyNotification = 0x0001
	FlagLastFragment        = 0x80
	FlagRelay               = 0x40
)

type Message struct {
	Dst          [6]byte
	Src          [6]byte
	Version      byte
	MsgType      uint16
	MessageID    uint16
	Fragment     byte
	Flags        byte
	TLVCount     uint16
	HasALMAC     bool
	ALMAC        [6]byte
	HasInterface bool
	InterfaceMAC [6]byte
	LastFragment bool
	Relay        bool
	Fragmented   bool
}

// ParseEthernet parses one Ethernet frame carrying an IEEE 1905.1 CMDU fragment.
// Non-final fragments may omit the end-of-message TLV; callers must not derive
// complete topology state from fragmented messages without reassembly.
func ParseEthernet(frame []byte) (Message, error) {
	var m Message
	if len(frame) < EtherHdr+CMDUHdr {
		return m, ErrShortFrame
	}
	if binary.BigEndian.Uint16(frame[12:14]) != EtherType {
		return m, ErrWrongEtherType
	}
	copy(m.Dst[:], frame[0:6])
	copy(m.Src[:], frame[6:12])
	p := frame[EtherHdr:]
	m.Version = p[0]
	m.MsgType = binary.BigEndian.Uint16(p[2:4])
	m.MessageID = binary.BigEndian.Uint16(p[4:6])
	m.Fragment = p[6]
	m.Flags = p[7]
	m.LastFragment = m.Flags&FlagLastFragment != 0
	m.Relay = m.Flags&FlagRelay != 0
	m.Fragmented = m.Fragment != 0 || !m.LastFragment

	off := CMDUHdr
	for count := 0; count < MaxTLVs; count++ {
		if off+3 > len(p) {
			if !m.LastFragment && off == len(p) {
				return m, nil
			}
			return Message{}, ErrMissingEndTLV
		}
		typ := p[off]
		length := int(binary.BigEndian.Uint16(p[off+1:off+3]) & 0x3fff)
		off += 3
		if length > len(p)-off {
			return Message{}, ErrTLVLength
		}
		if typ == TLVEnd {
			if length != 0 {
				return Message{}, ErrEndTLVLength
			}
			if !m.LastFragment {
				return Message{}, ErrUnexpectedEndTLV
			}
			m.TLVCount = uint16(count + 1)
			return m, nil
		}
		if typ == TLVALMAC && length == 6 && !m.HasALMAC {
			copy(m.ALMAC[:], p[off:off+6])
			m.HasALMAC = true
		}
		if typ == TLVInterface && length == 6 && !m.HasInterface {
			copy(m.InterfaceMAC[:], p[off:off+6])
			m.HasInterface = true
		}
		m.TLVCount = uint16(count + 1)
		off += length
	}
	return Message{}, ErrTooManyTLVs
}

// IsUsableSource rejects all-zero and multicast source addresses.
func IsUsableSource(mac [6]byte) bool {
	var any byte
	for _, b := range mac {
		any |= b
	}
	return any != 0 && mac[0]&1 == 0
}
