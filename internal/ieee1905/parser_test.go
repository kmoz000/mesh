package ieee1905

import (
	"encoding/binary"
	"testing"
)

func TestParseTopologyDiscovery(t *testing.T) {
	f := testFrame()
	m, err := ParseEthernet(f)
	if err != nil {
		t.Fatal(err)
	}
	if m.MsgType != MsgTopologyDiscovery || !m.HasALMAC || !m.HasInterface || m.Fragmented {
		t.Fatalf("unexpected parsed message: %+v", m)
	}
}

func TestParseIncompleteFirstFragmentWithoutEndTLV(t *testing.T) {
	f := testFrame()
	// The first (number zero) fragment is non-final and may end at a TLV boundary.
	f[EtherHdr+7] = 0
	f = f[:len(f)-3]
	m, err := ParseEthernet(f)
	if err != nil {
		t.Fatal(err)
	}
	if !m.Fragmented || m.LastFragment {
		t.Fatalf("fragment flags not preserved: %+v", m)
	}
}

func TestParseRejectsTruncatedTLV(t *testing.T) {
	f := testFrame()[:EtherHdr+CMDUHdr+3]
	if _, err := ParseEthernet(f); err != ErrTLVLength {
		t.Fatalf("error=%v, want %v", err, ErrTLVLength)
	}
}

func TestParseIgnoresReservedTLVLengthBitsAndEthernetPadding(t *testing.T) {
	f := testFrame()
	// Reserved high bits of the TLV length are ignored on receive.
	f[EtherHdr+CMDUHdr+1] = 0xc0
	f = append(f, 0, 0, 0) // Ethernet padding after end-of-message TLV.
	if _, err := ParseEthernet(f); err != nil {
		t.Fatal(err)
	}
}

func testFrame() []byte {
	f := make([]byte, EtherHdr+CMDUHdr)
	copy(f[:6], []byte{1, 0x80, 0xc2, 0, 0, 0x13})
	copy(f[6:12], []byte{2, 0, 0, 0, 0, 1})
	binary.BigEndian.PutUint16(f[12:14], EtherType)
	p := f[EtherHdr:]
	p[0] = 0
	binary.BigEndian.PutUint16(p[2:4], MsgTopologyDiscovery)
	binary.BigEndian.PutUint16(p[4:6], 7)
	p[7] = FlagLastFragment
	for _, tlv := range [][]byte{
		{TLVALMAC, 0, 6, 2, 0, 0, 0, 0, 1},
		{TLVInterface, 0, 6, 2, 0, 0, 0, 0, 2},
		{TLVEnd, 0, 0},
	} {
		p = append(p, tlv...)
	}
	return append(f[:EtherHdr], p...)
}
