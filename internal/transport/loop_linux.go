//go:build linux

package transport

import (
	"errors"
	"syscall"
	"time"
	"unsafe"
)

const (
	pollIn      = 0x0001
	sfdCloexec  = 0x80000
	sfdNonblock = 0x800
	sigBlock    = 0
)

type pollfd struct {
	fd      int32
	events  int16
	revents int16
}

// Loop multiplexes a small fixed set of file descriptors with a periodic tick,
// using ppoll with a computed timeout rather than a busy sleep. It wakes when
// any registered fd is readable or the tick interval elapses, whichever comes
// first. A signalfd for SIGINT/SIGTERM drives graceful shutdown when available.
// The loop is single-threaded; handlers run inline and must not block.
type Loop struct {
	interval time.Duration
	fds      []int
	onRead   []func()
	sigfd    int
	stop     bool
}

// NewLoop creates a loop with the given tick interval. It attempts to install a
// signalfd for SIGINT/SIGTERM; if that fails (for example under a runtime that
// lacks the syscall), the loop still runs and stops only via Stop or a deadline.
func NewLoop(interval time.Duration) *Loop {
	l := &Loop{interval: interval, sigfd: -1}
	if fd, err := installSignalfd(); err == nil {
		l.sigfd = fd
	}
	return l
}

// Add registers a readable fd with its handler. The handler is called whenever
// ppoll reports the fd readable; it should drain the fd without blocking.
func (l *Loop) Add(fd int, onRead func()) {
	l.fds = append(l.fds, fd)
	l.onRead = append(l.onRead, onRead)
}

// Stop requests the loop to exit after the current iteration.
func (l *Loop) Stop() { l.stop = true }

// Close releases the signalfd.
func (l *Loop) Close() {
	if l.sigfd >= 0 {
		syscall.Close(l.sigfd)
		l.sigfd = -1
	}
}

// Run drives the loop until Stop is called, a signal arrives, deadline passes
// (when non-zero), or a fatal receive error occurs. tick is called every
// interval with the current time; now supplies the clock (time.Now in
// production, a fake in tests of callers). Run returns nil on a clean stop.
func (l *Loop) Run(now func() time.Time, deadline time.Time, tick func(t time.Time)) error {
	defer l.Close()
	n := len(l.fds)
	poll := make([]pollfd, n+1) // +1 for the optional signalfd
	for i, fd := range l.fds {
		poll[i] = pollfd{fd: int32(fd), events: pollIn}
	}
	nfds := n
	if l.sigfd >= 0 {
		poll[n] = pollfd{fd: int32(l.sigfd), events: pollIn}
		nfds = n + 1
	}

	last := now()
	for !l.stop {
		cur := now()
		if !deadline.IsZero() && !cur.Before(deadline) {
			tick(cur)
			return nil
		}
		wait := l.interval - cur.Sub(last)
		if wait < 0 {
			wait = 0
		}
		if !deadline.IsZero() {
			if d := deadline.Sub(cur); d < wait {
				wait = d
			}
		}
		ts := syscall.NsecToTimespec(int64(wait))
		_, _, errno := syscall.Syscall6(
			syscall.SYS_PPOLL,
			uintptr(unsafe.Pointer(&poll[0])),
			uintptr(nfds),
			uintptr(unsafe.Pointer(&ts)),
			0, 0, 0,
		)
		if errno != 0 && errno != syscall.EINTR {
			return errors.New("ppoll: " + errno.Error())
		}
		for i := 0; i < n; i++ {
			if poll[i].revents&pollIn != 0 {
				l.onRead[i]()
			}
			poll[i].revents = 0
		}
		if l.sigfd >= 0 && poll[n].revents&pollIn != 0 {
			drainSignalfd(l.sigfd)
			return nil
		}
		if t := now(); t.Sub(last) >= l.interval {
			tick(t)
			last = t
		}
	}
	return nil
}

// installSignalfd blocks SIGINT/SIGTERM and returns a non-blocking signalfd for
// them, so the loop learns of a stop request through ppoll instead of a Go
// signal handler (which a TinyGo runtime may not support).
func installSignalfd() (int, error) {
	var mask uint64 = (1 << (uint(syscall.SIGINT) - 1)) | (1 << (uint(syscall.SIGTERM) - 1))
	if _, _, e := syscall.Syscall6(syscall.SYS_RT_SIGPROCMASK, sigBlock, uintptr(unsafe.Pointer(&mask)), 0, 8, 0, 0); e != 0 {
		return -1, e
	}
	r1, _, e := syscall.Syscall6(syscall.SYS_SIGNALFD4, ^uintptr(0), uintptr(unsafe.Pointer(&mask)), 8, sfdCloexec|sfdNonblock, 0, 0)
	if e != 0 {
		return -1, e
	}
	return int(r1), nil
}

func drainSignalfd(fd int) {
	var buf [128]byte // sizeof(struct signalfd_siginfo)
	for {
		n, err := syscall.Read(fd, buf[:])
		if n <= 0 || err != nil {
			return
		}
	}
}
