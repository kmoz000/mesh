//go:build linux

// Package hostapd is a cgo binding to a running hostapd's per-interface UNIX
// control socket (/var/run/hostapd-<radio>/<iface>). The target has no
// libwpa_client.so (hostapd links it statically), so the client is wmeshd's own
// compact C over AF_UNIX SOCK_DGRAM (ctrl.c), modeled on hostap's BSD wpa_ctrl.c.
//
// This is the vendor-aligned control path EasyMesh itself drives: STATUS, the
// associated-station list (topology input), and CHAN_SWITCH (CSA) with the
// unsolicited event stream (AP-CSA-FINISHED) that lets an actuation be confirmed
// by event instead of by a blind sleep. It issues no QCA vendor nl80211 command.
package hostapd

/*
#include <stdlib.h>
#include "ctrl.h"
*/
import "C"

import (
	"errors"
	"strings"
	"time"
	"unsafe"
)

// Conn is an open control connection to one hostapd interface socket.
type Conn struct {
	c    *C.struct_hapd_ctrl
	path string
}

var (
	// ErrOpen means the control socket could not be opened/connected.
	ErrOpen = errors.New("hostapd: cannot open control socket")
	// ErrClosed means the connection has been closed.
	ErrClosed = errors.New("hostapd: connection closed")
	// ErrTimeout means no reply/event arrived within the deadline.
	ErrTimeout = errors.New("hostapd: timed out")
	// ErrRequest means hostapd returned an error for the request.
	ErrRequest = errors.New("hostapd: request failed")
)

// Open connects to the hostapd control socket at path, e.g.
// "/var/run/hostapd-wifi0/ath0".
func Open(path string) (*Conn, error) {
	cp := C.CString(path)
	defer C.free(unsafe.Pointer(cp))
	h := C.hapd_open(cp)
	if h == nil {
		return nil, ErrOpen
	}
	return &Conn{c: h, path: path}, nil
}

// Path returns the control-socket path this connection was opened with.
func (c *Conn) Path() string { return c.path }

// Close releases the socket. Safe to call more than once.
func (c *Conn) Close() {
	if c != nil && c.c != nil {
		C.hapd_close(c.c)
		c.c = nil
	}
}

// Request sends a command and returns hostapd's reply (trimmed). Unsolicited
// event datagrams that arrive while waiting are skipped. timeout bounds the wait.
func (c *Conn) Request(cmd string, timeout time.Duration) (string, error) {
	if c == nil || c.c == nil {
		return "", ErrClosed
	}
	ccmd := C.CString(cmd)
	defer C.free(unsafe.Pointer(ccmd))
	var buf [4096]byte
	n := C.hapd_request(c.c, ccmd,
		(*C.char)(unsafe.Pointer(&buf[0])), C.int(len(buf)), C.int(timeout.Milliseconds()))
	switch {
	case int(n) == -11:
		return "", ErrTimeout
	case int(n) < 0:
		return "", ErrRequest
	}
	return string(buf[:int(n)]), nil
}

// RequestOK sends a command that is expected to answer "OK".
func (c *Conn) RequestOK(cmd string, timeout time.Duration) error {
	r, err := c.Request(cmd, timeout)
	if err != nil {
		return err
	}
	if !strings.Contains(r, "OK") {
		return errors.New("hostapd: " + cmd + " not acknowledged: " + strings.TrimSpace(r))
	}
	return nil
}

// Attach subscribes this connection to hostapd's unsolicited event stream.
func (c *Conn) Attach(timeout time.Duration) error { return c.RequestOK("ATTACH", timeout) }

// Detach unsubscribes from the event stream.
func (c *Conn) Detach(timeout time.Duration) error { return c.RequestOK("DETACH", timeout) }

// WaitEvent blocks until an event whose text contains substr arrives, or until
// deadline elapses. It requires a prior Attach. The wait is sliced so the Go
// scheduler is never blocked for the whole deadline inside cgo.
func (c *Conn) WaitEvent(substr string, deadline time.Duration) (string, error) {
	if c == nil || c.c == nil {
		return "", ErrClosed
	}
	end := time.Now().Add(deadline)
	var buf [4096]byte
	for {
		remain := time.Until(end)
		if remain <= 0 {
			return "", ErrTimeout
		}
		step := remain
		if step > 500*time.Millisecond {
			step = 500 * time.Millisecond
		}
		n := C.hapd_recv_event(c.c,
			(*C.char)(unsafe.Pointer(&buf[0])), C.int(len(buf)), C.int(step.Milliseconds()))
		if int(n) < 0 {
			return "", ErrRequest
		}
		if int(n) == 0 {
			continue // timeout slice; keep waiting until the deadline
		}
		ev := strings.TrimSpace(string(buf[:int(n)]))
		if strings.Contains(ev, substr) {
			return ev, nil
		}
	}
}

// ChanSwitch issues a CSA (channel switch announcement) and waits for hostapd's
// AP-CSA-FINISHED event before returning, so the caller knows the switch actually
// completed instead of guessing with a sleep. tail is the chan_switch argument
// vector without the verb, e.g. ["5","2437","sec_channel_offset=1",
// "center_freq1=2447","bandwidth=40","ht"]. settle bounds the wait for the
// finished event. It attaches for the event stream and detaches on return.
//
// Waiting on the event is what makes a switch-then-revert reliable: a second CSA
// issued while the first is still in progress is what hostapd rejects, so the
// revert must not fire until AP-CSA-FINISHED for the forward switch has arrived.
func (c *Conn) ChanSwitch(tail []string, settle time.Duration) error {
	if c == nil || c.c == nil {
		return ErrClosed
	}
	if err := c.Attach(2 * time.Second); err != nil {
		return err
	}
	defer c.Detach(time.Second)
	cmd := "CHAN_SWITCH " + strings.Join(tail, " ")
	if err := c.RequestOK(cmd, 2*time.Second); err != nil {
		return err
	}
	if settle <= 0 {
		settle = 5 * time.Second
	}
	if _, err := c.WaitEvent("AP-CSA-FINISHED", settle); err != nil {
		return errors.New("hostapd: CSA issued but AP-CSA-FINISHED not seen: " + err.Error())
	}
	return nil
}

// Stations returns the MAC addresses of the stations currently associated on
// this BSS, read via STA-FIRST/STA-NEXT. This is a direct topology input: who is
// connected to this radio right now.
func (c *Conn) Stations(timeout time.Duration) ([]string, error) {
	var macs []string
	r, err := c.Request("STA-FIRST", timeout)
	if err != nil {
		return nil, err
	}
	for {
		mac := firstLine(r)
		if mac == "" || strings.HasPrefix(mac, "FAIL") {
			break
		}
		macs = append(macs, mac)
		r, err = c.Request("STA-NEXT "+mac, timeout)
		if err != nil {
			return macs, err
		}
	}
	return macs, nil
}

// firstLine returns the first line of s (hostapd station blocks lead with the
// MAC address on its own line).
func firstLine(s string) string {
	s = strings.TrimSpace(s)
	if i := strings.IndexByte(s, '\n'); i >= 0 {
		return strings.TrimSpace(s[:i])
	}
	return s
}
