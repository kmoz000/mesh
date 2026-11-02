package transport

import "testing"

func TestParseInet6(t *testing.T) {
	// sockaddr_in6: family=AF_INET6(10) LE, port=0xBEEF big-endian, flowinfo=0,
	// addr=fe80::1, scope_id=3 little-endian.
	sa := make([]byte, sockaddrInet6Len)
	sa[0] = 10
	sa[2], sa[3] = 0xbe, 0xef // port, network order
	sa[8], sa[9] = 0xfe, 0x80 // fe80::
	sa[23] = 0x01             // ...::1
	sa[24] = 3                // scope id LE
	got, ok := parseInet6(sa)
	if !ok {
		t.Fatal("parseInet6 returned !ok on a full buffer")
	}
	if got.Port != 0xbeef {
		t.Fatalf("port=%#x want 0xbeef", got.Port)
	}
	if got.Zone != 3 {
		t.Fatalf("zone=%d want 3", got.Zone)
	}
	if got.IP[0] != 0xfe || got.IP[1] != 0x80 || got.IP[15] != 0x01 {
		t.Fatalf("addr=%x unexpected", got.IP)
	}
}

func TestParseInet6ShortBuffer(t *testing.T) {
	if _, ok := parseInet6(make([]byte, sockaddrInet6Len-1)); ok {
		t.Fatal("parseInet6 accepted a short buffer")
	}
}

func TestAddrIsZero(t *testing.T) {
	if !(Addr{}).IsZero() {
		t.Fatal("zero Addr should report IsZero")
	}
	if (Addr{Port: 1}).IsZero() {
		t.Fatal("non-zero Addr should not report IsZero")
	}
}

func TestV4Mapped(t *testing.T) {
	a := V4Mapped(10, 255, 255, 38, 48213)
	if !a.IsV4Mapped() {
		t.Fatal("V4Mapped address should report IsV4Mapped")
	}
	if a.Port != 48213 {
		t.Fatalf("port = %d, want 48213", a.Port)
	}
	want := [16]byte{0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0xff, 0xff, 10, 255, 255, 38}
	if a.IP != want {
		t.Fatalf("IP = %v, want %v", a.IP, want)
	}
	// A link-local address is not v4-mapped.
	ll := Addr{IP: [16]byte{0xfe, 0x80}, Zone: 3, Port: 1}
	if ll.IsV4Mapped() {
		t.Fatal("link-local must not report IsV4Mapped")
	}
}
