package radio

import (
	"encoding/binary"
	"errors"
)

// Pure generic-netlink message codec: builds and parses nlmsghdr + genlmsghdr +
// nlattr buffers with explicit length checks and a caller-owned backing array,
// so there is no allocation on the message path and no dependency on package
// syscall (it compiles and tests on any host). Netlink is host byte order, so
// numeric fields use binary.NativeEndian; the test host and the arm64 target are
// both little-endian, so the crafted-buffer tests exercise the real wire layout.

const (
	nlMsgHdrLen  = 16 // struct nlmsghdr
	genlHdrLen   = 4  // struct genlmsghdr
	nlAttrHdrLen = 4  // struct nlattr

	// nlattr type flags (stripped when reading a type).
	nlaFNested     = 0x8000
	nlaFNetByteOrd = 0x4000
	nlaTypeMask    = 0x3fff

	// netlink message flags we use.
	nlmFRequest = 0x01
	nlmFAck     = 0x04
	nlmFDump    = 0x300 // NLM_F_ROOT|NLM_F_MATCH

	// control message types.
	nlMsgNoop    = 0x1
	nlMsgError   = 0x2
	nlMsgDone    = 0x3
	nlMsgOverrun = 0x4
)

var (
	// ErrShort means a buffer was too small for a header or a claimed length.
	ErrShort = errors.New("radio: short netlink buffer")
	// ErrBounds means a declared length ran past the end of the buffer.
	ErrBounds = errors.New("radio: netlink length out of bounds")
	// ErrFull means a builder ran out of its fixed backing array.
	ErrFull = errors.New("radio: netlink builder buffer full")
	// ErrMalformed means an attribute length was below its own header size.
	ErrMalformed = errors.New("radio: malformed netlink attribute")
)

func nlAlign(n int) int { return (n + 3) &^ 3 }

// builder writes a single request message into a fixed backing array. Every
// method is a no-op once err is set, so callers check err once at the end.
type builder struct {
	b    []byte
	n    int
	err  error
	nest []int // stack of nlattr header offsets awaiting a length patch
}

func newBuilder(buf []byte) *builder { return &builder{b: buf} }

func (w *builder) grow(n int) (int, bool) {
	if w.err != nil {
		return 0, false
	}
	if w.n+n > len(w.b) {
		w.err = ErrFull
		return 0, false
	}
	off := w.n
	w.n += n
	return off, true
}

// putHeader writes the nlmsghdr+genlmsghdr prologue. Len is patched by finalize.
func (w *builder) putHeader(msgType uint16, flags uint16, seq, pid uint32, cmd, version uint8) {
	off, ok := w.grow(nlMsgHdrLen + genlHdrLen)
	if !ok {
		return
	}
	binary.NativeEndian.PutUint32(w.b[off:], 0) // len, patched later
	binary.NativeEndian.PutUint16(w.b[off+4:], msgType)
	binary.NativeEndian.PutUint16(w.b[off+6:], flags)
	binary.NativeEndian.PutUint32(w.b[off+8:], seq)
	binary.NativeEndian.PutUint32(w.b[off+12:], pid)
	w.b[off+16] = cmd
	w.b[off+17] = version
	w.b[off+18] = 0
	w.b[off+19] = 0
}

// putAttr writes one attribute (header + payload) with 4-byte alignment padding.
func (w *builder) putAttr(atype uint16, payload []byte) {
	total := nlAttrHdrLen + len(payload)
	off, ok := w.grow(nlAlign(total))
	if !ok {
		return
	}
	binary.NativeEndian.PutUint16(w.b[off:], uint16(total))
	binary.NativeEndian.PutUint16(w.b[off+2:], atype)
	copy(w.b[off+nlAttrHdrLen:], payload)
	// pad bytes left as whatever the array held; alignment only matters for
	// offset math, kernel ignores pad content. Zero them for cleanliness.
	for i := off + total; i < off+nlAlign(total); i++ {
		w.b[i] = 0
	}
}

func (w *builder) putU16(atype uint16, v uint16) {
	var tmp [2]byte
	binary.NativeEndian.PutUint16(tmp[:], v)
	w.putAttr(atype, tmp[:])
}

func (w *builder) putU32(atype uint16, v uint32) {
	var tmp [4]byte
	binary.NativeEndian.PutUint32(tmp[:], v)
	w.putAttr(atype, tmp[:])
}

func (w *builder) putString(atype uint16, s string) {
	// nl80211 strings are NUL-terminated.
	total := nlAttrHdrLen + len(s) + 1
	off, ok := w.grow(nlAlign(total))
	if !ok {
		return
	}
	binary.NativeEndian.PutUint16(w.b[off:], uint16(total))
	binary.NativeEndian.PutUint16(w.b[off+2:], atype)
	copy(w.b[off+nlAttrHdrLen:], s)
	for i := off + nlAttrHdrLen + len(s); i < off+nlAlign(total); i++ {
		w.b[i] = 0
	}
}

func (w *builder) putFlag(atype uint16) { w.putAttr(atype, nil) }

// beginNested opens a nested attribute; endNested patches its length.
func (w *builder) beginNested(atype uint16) {
	off, ok := w.grow(nlAttrHdrLen)
	if !ok {
		return
	}
	binary.NativeEndian.PutUint16(w.b[off:], 0) // len patched in endNested
	binary.NativeEndian.PutUint16(w.b[off+2:], atype|nlaFNested)
	w.nest = append(w.nest, off)
}

func (w *builder) endNested() {
	if w.err != nil {
		return
	}
	if len(w.nest) == 0 {
		w.err = ErrMalformed
		return
	}
	off := w.nest[len(w.nest)-1]
	w.nest = w.nest[:len(w.nest)-1]
	length := w.n - off
	if length < nlAttrHdrLen || length > 0xffff {
		w.err = ErrBounds
		return
	}
	binary.NativeEndian.PutUint16(w.b[off:], uint16(length))
}

// finalize patches the nlmsghdr length and returns the framed message.
func (w *builder) finalize() ([]byte, error) {
	if w.err != nil {
		return nil, w.err
	}
	if len(w.nest) != 0 {
		return nil, ErrMalformed
	}
	if w.n < nlMsgHdrLen {
		return nil, ErrShort
	}
	binary.NativeEndian.PutUint32(w.b[0:], uint32(w.n))
	return w.b[:w.n], nil
}

// attrIter walks the attributes in a payload with bounds checks. A malformed
// length stops iteration and sets err; Next never reads past the slice.
type attrIter struct {
	data []byte
	off  int
	err  error
}

func newAttrIter(data []byte) *attrIter { return &attrIter{data: data} }

// next returns the next attribute's type (flags stripped) and payload. ok is
// false at the clean end or on error (check Err).
func (it *attrIter) next() (atype uint16, payload []byte, ok bool) {
	if it.err != nil {
		return 0, nil, false
	}
	if it.off == len(it.data) {
		return 0, nil, false
	}
	if it.off+nlAttrHdrLen > len(it.data) {
		it.err = ErrShort
		return 0, nil, false
	}
	alen := int(binary.NativeEndian.Uint16(it.data[it.off:]))
	rawType := binary.NativeEndian.Uint16(it.data[it.off+2:])
	if alen < nlAttrHdrLen {
		it.err = ErrMalformed
		return 0, nil, false
	}
	if it.off+alen > len(it.data) {
		it.err = ErrBounds
		return 0, nil, false
	}
	payload = it.data[it.off+nlAttrHdrLen : it.off+alen]
	it.off += nlAlign(alen)
	if it.off > len(it.data) {
		// final attribute need not be padded; clamp to end.
		it.off = len(it.data)
	}
	return rawType & nlaTypeMask, payload, true
}

func (it *attrIter) nested(payload []byte) *attrIter { return newAttrIter(payload) }

// parsedMsg is one received netlink message, header split from its attributes.
type parsedMsg struct {
	msgType uint16
	flags   uint16
	seq     uint32
	pid     uint32
	cmd     uint8
	attrs   []byte // genl attribute region
}

// parseMessage splits one message starting at buf[0]. It returns the message
// and the total aligned bytes it consumed, or an error. It never reads past buf.
func parseMessage(buf []byte) (parsedMsg, int, error) {
	var m parsedMsg
	if len(buf) < nlMsgHdrLen {
		return m, 0, ErrShort
	}
	msgLen := int(binary.NativeEndian.Uint32(buf[0:]))
	if msgLen < nlMsgHdrLen || msgLen > len(buf) {
		return m, 0, ErrBounds
	}
	m.msgType = binary.NativeEndian.Uint16(buf[4:])
	m.flags = binary.NativeEndian.Uint16(buf[6:])
	m.seq = binary.NativeEndian.Uint32(buf[8:])
	m.pid = binary.NativeEndian.Uint32(buf[12:])
	consumed := nlAlign(msgLen)
	if consumed > len(buf) {
		consumed = len(buf)
	}
	// NLMSG_ERROR/DONE carry no genlmsghdr; leave cmd/attrs empty.
	if m.msgType == nlMsgError || m.msgType == nlMsgDone ||
		m.msgType == nlMsgNoop || m.msgType == nlMsgOverrun {
		if msgLen >= nlMsgHdrLen+4 {
			m.attrs = buf[nlMsgHdrLen:msgLen]
		}
		return m, consumed, nil
	}
	if msgLen < nlMsgHdrLen+genlHdrLen {
		return m, consumed, ErrShort
	}
	m.cmd = buf[nlMsgHdrLen]
	m.attrs = buf[nlMsgHdrLen+genlHdrLen : msgLen]
	return m, consumed, nil
}

// errorCode reads the signed error from an NLMSG_ERROR body. A zero code is an
// ACK, not a failure.
func errorCode(attrs []byte) int32 {
	if len(attrs) < 4 {
		return 0
	}
	return int32(binary.NativeEndian.Uint32(attrs[0:]))
}

func attrU32(p []byte) (uint32, bool) {
	if len(p) < 4 {
		return 0, false
	}
	return binary.NativeEndian.Uint32(p), true
}

func attrU16(p []byte) (uint16, bool) {
	if len(p) < 2 {
		return 0, false
	}
	return binary.NativeEndian.Uint16(p), true
}
