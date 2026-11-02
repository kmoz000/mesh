//go:build linux

package transport

import (
	"fmt"
	"syscall"
	"unsafe"
)

// UDP is a non-blocking AF_INET6 datagram socket.
type UDP struct {
	fd int
}

// ListenUDP opens an IPv6 UDP socket bound to [::]:port. It enables address
// reuse so controller and agent roles can share a well-known port across
// restarts, and dual-stack is left at the kernel default.
func ListenUDP(port uint16) (*UDP, error) {
	fd, err := syscall.Socket(syscall.AF_INET6, syscall.SOCK_DGRAM, syscall.IPPROTO_UDP)
	if err != nil {
		return nil, fmt.Errorf("open UDP socket: %w", err)
	}
	if err := syscall.SetsockoptInt(fd, syscall.SOL_SOCKET, syscall.SO_REUSEADDR, 1); err != nil {
		syscall.Close(fd)
		return nil, fmt.Errorf("set SO_REUSEADDR: %w", err)
	}
	// Dual-stack: allow IPv4-mapped (::ffff:a.b.c.d) peers so the daemon can
	// reach a controller over an IPv4 routed or tunnelled backhaul
	// where link-local multicast discovery cannot reach. Default is usually
	// dual-stack already; set it explicitly rather than depend on sysctl.
	if err := syscall.SetsockoptInt(fd, syscall.IPPROTO_IPV6, syscall.IPV6_V6ONLY, 0); err != nil {
		syscall.Close(fd)
		return nil, fmt.Errorf("clear IPV6_V6ONLY: %w", err)
	}
	if err := syscall.Bind(fd, &syscall.SockaddrInet6{Port: int(port)}); err != nil {
		syscall.Close(fd)
		return nil, fmt.Errorf("bind UDP [::]:%d: %w", port, err)
	}
	if err := syscall.SetNonblock(fd, true); err != nil {
		syscall.Close(fd)
		return nil, fmt.Errorf("set nonblocking: %w", err)
	}
	if err := syscall.SetsockoptInt(fd, syscall.SOL_SOCKET, syscall.SO_RCVBUF, 256*1024); err != nil {
		syscall.Close(fd)
		return nil, fmt.Errorf("set receive buffer: %w", err)
	}
	return &UDP{fd: fd}, nil
}

// Fd returns the underlying file descriptor for the event loop.
func (u *UDP) Fd() int { return u.fd }

// Close closes the socket.
func (u *UDP) Close() error { return syscall.Close(u.fd) }

// JoinMulticast joins the IPv6 group on the given interface index. group is the
// 16-byte address; ifindex scopes a link-local join. It builds struct ipv6_mreq
// by hand to avoid depending on a syscall wrapper that TinyGo may not provide.
func (u *UDP) JoinMulticast(group [16]byte, ifindex uint32) error {
	var mreq [20]byte // in6_addr(16) + unsigned int ifindex(4)
	copy(mreq[0:16], group[:])
	mreq[16] = byte(ifindex)
	mreq[17] = byte(ifindex >> 8)
	mreq[18] = byte(ifindex >> 16)
	mreq[19] = byte(ifindex >> 24)
	if err := syscall.SetsockoptString(u.fd, syscall.IPPROTO_IPV6, syscall.IPV6_JOIN_GROUP, string(mreq[:])); err != nil {
		return fmt.Errorf("join IPv6 group on ifindex %d: %w", ifindex, err)
	}
	return nil
}

// Recv reads one datagram into buf without allocating a Sockaddr, mirroring the
// observer's recvPacket. It returns the datagram length, its source address,
// whether it was truncated, and a raw errno (0 on success, EAGAIN when empty).
func (u *UDP) Recv(buf []byte) (n int, from Addr, truncated bool, errno syscall.Errno) {
	if len(buf) == 0 {
		return 0, Addr{}, false, syscall.EINVAL
	}
	var sa [sockaddrInet6Len]byte
	salen := uint32(len(sa))
	r1, _, e := syscall.Syscall6(
		syscall.SYS_RECVFROM,
		uintptr(u.fd),
		uintptr(unsafe.Pointer(&buf[0])),
		uintptr(len(buf)),
		uintptr(syscall.MSG_TRUNC),
		uintptr(unsafe.Pointer(&sa[0])),
		uintptr(unsafe.Pointer(&salen)),
	)
	if e != 0 {
		return 0, Addr{}, false, e
	}
	from, _ = parseInet6(sa[:])
	size := int(r1)
	if size > len(buf) {
		return len(buf), from, true, 0
	}
	return size, from, false, 0
}

// SendTo sends b to dst. Sends are infrequent (query/report/ack/announce at
// cadence), so the Sockaddr allocation here is acceptable.
func (u *UDP) SendTo(dst Addr, b []byte) error {
	sa := &syscall.SockaddrInet6{Port: int(dst.Port), ZoneId: dst.Zone, Addr: dst.IP}
	if err := syscall.Sendto(u.fd, b, 0, sa); err != nil {
		return fmt.Errorf("sendto: %w", err)
	}
	return nil
}
