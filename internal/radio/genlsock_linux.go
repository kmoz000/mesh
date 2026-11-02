//go:build linux

package radio

import (
	"errors"
	"syscall"
	"unsafe"
)

// NETLINK_GENERIC client. It follows udp_linux.go's house style: the socket
// wrapper is used where TinyGo provides it, and the netlink sockaddr is built by
// hand (12 bytes) and passed through a raw bind syscall rather than relying on
// syscall.SockaddrNetlink, which TinyGo may not define. Requests are written
// with Write (an unbound generic-netlink socket targets the kernel, pid 0) and
// replies read with Read; a dump is drained until NLMSG_DONE or NLMSG_ERROR.
//
// This file is the ONLY netlink I/O in the package; all framing/parsing is in
// the pure genlmsg.go / probe.go / actuate.go / family.go, so tests never open
// a socket. DEVICE EXECUTION IS PENDING: this has not run against a real kernel.

const (
	netlinkGeneric = 16 // NETLINK_GENERIC
	afNetlink      = 16 // AF_NETLINK
	sockaddrNlLen  = 12 // struct sockaddr_nl
)

// genlConn is an open NETLINK_GENERIC socket.
type genlConn struct {
	fd  int
	seq uint32
	pid uint32
}

var errNoSocket = errors.New("radio: netlink socket not open")

// openGenl opens and binds a generic-netlink socket. nl_pid is left 0 so the
// kernel auto-assigns a unique port id.
func openGenl() (*genlConn, error) {
	fd, err := syscall.Socket(afNetlink, syscall.SOCK_RAW, netlinkGeneric)
	if err != nil {
		return nil, err
	}
	var sa [sockaddrNlLen]byte
	// nl_family (u16) = AF_NETLINK; nl_pad, nl_pid, nl_groups all 0.
	sa[0] = byte(afNetlink)
	sa[1] = byte(afNetlink >> 8)
	if _, _, e := syscall.Syscall(syscall.SYS_BIND, uintptr(fd),
		uintptr(unsafe.Pointer(&sa[0])), uintptr(sockaddrNlLen)); e != 0 {
		syscall.Close(fd)
		return nil, e
	}
	return &genlConn{fd: fd, seq: 1}, nil
}

// Close closes the socket.
func (c *genlConn) Close() error {
	if c == nil || c.fd < 0 {
		return nil
	}
	err := syscall.Close(c.fd)
	c.fd = -1
	return err
}

func (c *genlConn) nextSeq() uint32 {
	c.seq++
	return c.seq
}

// execute sends a framed request and collects reply attribute regions. build
// receives the sequence number and must frame the message (into the supplied
// scratch buffer). onMsg is called for each non-control reply message's attrs.
// It returns when NLMSG_DONE arrives (dump) or after a single reply for a
// non-dump request, and maps NLMSG_ERROR to an error.
func (c *genlConn) execute(dump bool, build func(seq uint32) ([]byte, error), onMsg func(cmd uint8, attrs []byte) error) error {
	if c == nil || c.fd < 0 {
		return errNoSocket
	}
	seq := c.nextSeq()
	req, err := build(seq)
	if err != nil {
		return err
	}
	if _, err := syscall.Write(c.fd, req); err != nil {
		return err
	}
	buf := make([]byte, 32*1024)
	for {
		n, err := syscall.Read(c.fd, buf)
		if err != nil {
			return err
		}
		data := buf[:n]
		done := false
		for len(data) >= nlMsgHdrLen {
			m, consumed, perr := parseMessage(data)
			if perr != nil {
				return perr
			}
			if consumed <= 0 || consumed > len(data) {
				break
			}
			switch m.msgType {
			case nlMsgError:
				if m.seq != seq {
					// A stale ACK left on the socket by a previous request. Skip
					// it so it does not terminate this call early.
					data = data[consumed:]
					continue
				}
				if code := errorCode(m.attrs); code != 0 {
					return syscall.Errno(-code)
				}
				// zero code is an ACK (end of a non-dump request).
				return nil
			case nlMsgDone:
				done = true
			case nlMsgNoop, nlMsgOverrun:
				// ignore
			default:
				if m.seq == seq {
					if err := onMsg(m.cmd, m.attrs); err != nil {
						return err
					}
				}
			}
			data = data[consumed:]
		}
		if done {
			return nil
		}
		if !dump {
			// A non-dump reply arrives as a single message (plus maybe an ACK);
			// one buffer is enough unless the kernel also sends an ACK, which the
			// nlMsgError case above returns on. If we got here, we are done.
			return nil
		}
	}
}
