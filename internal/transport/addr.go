// Package transport provides the WantasticMesh daemon's syscall-level UDP
// transport and a bounded ppoll event loop. The socket code is Linux-only; the
// address helpers here are pure so they can be unit-tested on any host.
//
// The daemon deliberately uses raw AF_INET6 sockets instead of the Go net
// package: the target builds with TinyGo, whose net package is a netdev fork
// rather than OS sockets, so net.ListenUDP is unavailable there.
package transport

// Addr is an IPv6 socket address. Zone is the sin6_scope_id (the interface
// index for a link-local address); Port is host byte order.
type Addr struct {
	IP   [16]byte
	Zone uint32
	Port uint16
}

const sockaddrInet6Len = 28 // sa_family(2) port(2) flowinfo(4) addr(16) scope(4)

// parseInet6 decodes a kernel sockaddr_in6 (at least 28 bytes) into an Addr.
// The port is big-endian on the wire; the scope id is native byte order, which
// is little-endian on the arm64/amd64 targets.
func parseInet6(sa []byte) (Addr, bool) {
	if len(sa) < sockaddrInet6Len {
		return Addr{}, false
	}
	var a Addr
	a.Port = uint16(sa[2])<<8 | uint16(sa[3])
	copy(a.IP[:], sa[8:24])
	a.Zone = uint32(sa[24]) | uint32(sa[25])<<8 | uint32(sa[26])<<16 | uint32(sa[27])<<24
	return a, true
}

// IsZero reports whether the address is the zero value.
func (a Addr) IsZero() bool {
	return a == Addr{}
}

// V4Mapped builds an IPv4-mapped IPv6 address (::ffff:a.b.c.d) so the daemon's
// AF_INET6 socket can reach an IPv4 peer, such as a controller across a routed
// point-to-point backhaul or management tunnel where link-local multicast
// discovery cannot reach. The socket must be dual-stack (IPV6_V6ONLY off).
func V4Mapped(a, b, c, d byte, port uint16) Addr {
	var out Addr
	out.IP[10] = 0xff
	out.IP[11] = 0xff
	out.IP[12] = a
	out.IP[13] = b
	out.IP[14] = c
	out.IP[15] = d
	out.Port = port
	return out
}

// IsV4Mapped reports whether the address is an IPv4-mapped IPv6 address.
func (a Addr) IsV4Mapped() bool {
	for i := 0; i < 10; i++ {
		if a.IP[i] != 0 {
			return false
		}
	}
	return a.IP[10] == 0xff && a.IP[11] == 0xff
}
