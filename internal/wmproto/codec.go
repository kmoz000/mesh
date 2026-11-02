// Package wmproto implements the bounded, authenticated-and-encrypted
// WantasticMesh v0 wire format and its controller-side exchange state. It
// performs no I/O. The v0 format is private to WantasticMesh; it is not IEEE
// 1905 or EasyMesh. A datagram is a 72-byte cleartext header (used as AEAD
// additional data so routing/identity fields are authenticated) followed by
// the AES-256-GCM encryption of the body and its 16-byte tag. Only a node
// holding the per-agent key can read a body, which is the "only wmeshd
// understands it" property; it comes from the shared key, not from obscurity.
package wmproto

import (
	"crypto/aes"
	"crypto/cipher"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/binary"
	"errors"
	"hash"
)

const (
	Version     = 0
	HeaderSize  = 72
	TagSize     = 16 // AES-GCM tag; equals the old HMAC tag so the layout is stable
	MaxDatagram = 1200
	MaxBody     = MaxDatagram - HeaderSize - TagSize
	KeySize     = 32
	ReportLease = 90

	nonceSize = 12

	// Direction binds a subkey to who originated the message, so the two
	// directions of one key never share a keystream. It is derived from the
	// message type, not stored, so a receiver recomputes it from the header.
	dirAgent      = 0x01 // agent -> controller: Announce, Report
	dirController = 0x02 // controller -> agent: Query, Ack
)

// encLabel* are the domain-separation prefixes for subkey derivation, one per
// direction, kept as heap-resident slices so derivation allocates nothing for
// the label. The trailing byte is the direction.
var (
	encLabelAgent      = []byte("WMSH-v0-enc\x01")
	encLabelController = []byte("WMSH-v0-enc\x02")
)

func labelFor(dir byte) []byte {
	if dir == dirController {
		return encLabelController
	}
	return encLabelAgent
}

type NetworkID [8]byte
type NodeID [16]byte
type BootID [8]byte
type ExchangeID [16]byte

type MessageType uint8

const (
	Announce MessageType = 1
	Query    MessageType = 2
	Report   MessageType = 3
	Ack      MessageType = 4
)

const (
	CompleteRadios uint16 = 1 << 0
	CompleteLinks  uint16 = 1 << 1
	Urgent         uint16 = 1 << 2
)

var (
	ErrKey       = errors.New("wmproto: key must be 32 bytes")
	ErrIdentity  = errors.New("wmproto: wrong or empty identity")
	ErrLength    = errors.New("wmproto: invalid length")
	ErrHeader    = errors.New("wmproto: invalid header")
	ErrType      = errors.New("wmproto: unsupported message type")
	ErrFlags     = errors.New("wmproto: invalid flags")
	ErrAuth      = errors.New("wmproto: authentication failed")
	ErrBody      = errors.New("wmproto: invalid message body")
	ErrCapacity  = errors.New("wmproto: capacity exceeded")
	ErrChallenge = errors.New("wmproto: invalid or expired challenge")
	ErrReplay    = errors.New("wmproto: duplicate or replayed message")
	ErrClock     = errors.New("wmproto: clock moved backwards")
)

// Header is the decoded v0 header. The codec checks Network and KeyID against
// its configured values, and Decode additionally pins the expected sender.
// Sequence is scoped to Sender and Boot and, for nonce safety under GCM, must
// strictly increase across every message a node sends under one boot (Announce
// and Report for an agent; Query and Ack for a controller). Exchange is a fresh
// controller-issued challenge for query, report, and ack; an announce uses the
// zero value.
type Header struct {
	Type         MessageType
	Flags        uint16
	Network      NetworkID
	Sender       NodeID
	Boot         BootID
	Exchange     ExchangeID
	Sequence     uint64
	LeaseSeconds uint16
	KeyID        uint16
}

// Codec authenticates and encrypts messages for one network and one per-agent
// key. Use a distinct random 32-byte key for each agent, shared only with its
// configured controller; a group key would let any member read or forge
// another's traffic. Each codec encodes one direction and decodes the other,
// so it caches one AEAD per direction, rederiving only when the peer's boot id
// changes. It reuses crypto state and is therefore not safe for concurrent use.
type Codec struct {
	network NetworkID
	keyID   uint16
	kdf     hash.Hash // HMAC-SHA256(masterKey), reused for subkey derivation

	encSender NodeID
	encBoot   BootID
	encDir    byte
	encAEAD   cipher.AEAD
	encSet    bool

	decSender NodeID
	decBoot   BootID
	decDir    byte
	decAEAD   cipher.AEAD
	decSet    bool

	subkey [sha256.Size]byte
	nonce  [nonceSize]byte
	// plain holds decrypted output so Decode does not overwrite the caller's
	// datagram and stays idempotent. The returned body aliases it and is valid
	// only until the next Decode on this codec.
	plain [MaxDatagram]byte
}

func NewCodec(network NetworkID, keyID uint16, key []byte) (*Codec, error) {
	if len(key) != KeySize {
		return nil, ErrKey
	}
	if network == (NetworkID{}) {
		return nil, ErrIdentity
	}
	return &Codec{network: network, keyID: keyID, kdf: hmac.New(sha256.New, key)}, nil
}

func directionOf(t MessageType) byte {
	switch t {
	case Announce, Report:
		return dirAgent
	case Query, Ack:
		return dirController
	default:
		return 0
	}
}

// deriveAEAD computes the per-(direction, sender, boot) AES-256-GCM subkey. It
// allocates (NewCipher/NewGCM) and runs only on a cache miss, i.e. once per
// boot id, so the steady-state Encode/Decode path stays allocation-free.
func (c *Codec) deriveAEAD(dir byte, sender NodeID, boot BootID) (cipher.AEAD, error) {
	c.kdf.Reset()
	c.kdf.Write(labelFor(dir))
	c.kdf.Write(sender[:])
	c.kdf.Write(boot[:])
	c.kdf.Sum(c.subkey[:0])
	block, err := aes.NewCipher(c.subkey[:KeySize])
	if err != nil {
		return nil, err
	}
	return cipher.NewGCM(block)
}

func (c *Codec) encAEADFor(dir byte, sender NodeID, boot BootID) (cipher.AEAD, error) {
	if c.encSet && c.encDir == dir && c.encSender == sender && c.encBoot == boot {
		return c.encAEAD, nil
	}
	aead, err := c.deriveAEAD(dir, sender, boot)
	if err != nil {
		return nil, err
	}
	c.encDir, c.encSender, c.encBoot, c.encAEAD, c.encSet = dir, sender, boot, aead, true
	return aead, nil
}

func (c *Codec) decAEADFor(dir byte, sender NodeID, boot BootID) (cipher.AEAD, error) {
	if c.decSet && c.decDir == dir && c.decSender == sender && c.decBoot == boot {
		return c.decAEAD, nil
	}
	aead, err := c.deriveAEAD(dir, sender, boot)
	if err != nil {
		return nil, err
	}
	c.decDir, c.decSender, c.decBoot, c.decAEAD, c.decSet = dir, sender, boot, aead, true
	return aead, nil
}

// setNonce writes a 12-byte nonce of four zero bytes and the 8-byte sequence.
// Sequence is unique per subkey, so the nonce never repeats under one key.
func (c *Codec) setNonce(seq uint64) {
	c.nonce[0], c.nonce[1], c.nonce[2], c.nonce[3] = 0, 0, 0, 0
	binary.BigEndian.PutUint64(c.nonce[4:12], seq)
}

// Encode writes one authenticated, encrypted datagram to dst and returns the
// number of bytes used. dst may overlap body. On an error, dst is unchanged in
// length (its bytes may be partially written). Caller-generated boot IDs and
// challenges must be unpredictable and non-repeating, and Sequence must be
// unique per boot across all message types; the codec does not generate
// randomness and does not track sequences for Encode.
func (c *Codec) Encode(dst []byte, h Header, body []byte) (int, error) {
	if c == nil || c.kdf == nil {
		return 0, ErrKey
	}
	if h.Network != c.network || h.KeyID != c.keyID {
		return 0, ErrIdentity
	}
	if err := validateHeader(h, len(body)); err != nil {
		return 0, err
	}
	if err := validateBody(h, body); err != nil {
		return 0, err
	}
	dir := directionOf(h.Type)
	if dir == 0 {
		return 0, ErrType
	}
	total := HeaderSize + len(body) + TagSize
	if len(dst) < total {
		return 0, ErrLength
	}
	// Copy the body into place first so it survives even when it overlaps the
	// region that becomes the header.
	copy(dst[HeaderSize:HeaderSize+len(body)], body)
	copy(dst[0:4], "WMSH")
	dst[4] = Version
	dst[5] = byte(h.Type)
	binary.BigEndian.PutUint16(dst[6:8], h.Flags)
	copy(dst[8:16], h.Network[:])
	copy(dst[16:32], h.Sender[:])
	copy(dst[32:40], h.Boot[:])
	copy(dst[40:56], h.Exchange[:])
	binary.BigEndian.PutUint64(dst[56:64], h.Sequence)
	binary.BigEndian.PutUint16(dst[64:66], h.LeaseSeconds)
	binary.BigEndian.PutUint16(dst[66:68], uint16(len(body)))
	binary.BigEndian.PutUint16(dst[68:70], h.KeyID)
	dst[70], dst[71] = 0, 0

	aead, err := c.encAEADFor(dir, h.Sender, h.Boot)
	if err != nil {
		return 0, err
	}
	c.setNonce(h.Sequence)
	// Seal in place: plaintext[:0] as dst so the ciphertext+tag overwrite the
	// plaintext region with no allocation.
	ct := aead.Seal(dst[HeaderSize:HeaderSize], c.nonce[:], dst[HeaderSize:HeaderSize+len(body)], dst[:HeaderSize])
	if len(ct) != len(body)+TagSize {
		return 0, ErrLength
	}
	return total, nil
}

// Decode authenticates, decrypts, and validates exactly one datagram from
// expectedSender. The returned body aliases datagram (the plaintext is written
// in place over the ciphertext) and is valid only until datagram is reused. Do
// not select a key solely by trusting the unauthenticated sender field.
func (c *Codec) Decode(datagram []byte, expectedSender NodeID) (Header, []byte, error) {
	var h Header
	if c == nil || c.kdf == nil {
		return h, nil, ErrKey
	}
	if expectedSender == (NodeID{}) {
		return h, nil, ErrIdentity
	}
	if len(datagram) < HeaderSize+TagSize || len(datagram) > MaxDatagram {
		return h, nil, ErrLength
	}
	bodyLen := int(binary.BigEndian.Uint16(datagram[66:68]))
	if bodyLen > MaxBody || len(datagram) != HeaderSize+bodyLen+TagSize {
		return h, nil, ErrLength
	}
	if string(datagram[:4]) != "WMSH" || datagram[4] != Version || datagram[70] != 0 || datagram[71] != 0 {
		return h, nil, ErrHeader
	}
	h.Type = MessageType(datagram[5])
	h.Flags = binary.BigEndian.Uint16(datagram[6:8])
	copy(h.Network[:], datagram[8:16])
	copy(h.Sender[:], datagram[16:32])
	copy(h.Boot[:], datagram[32:40])
	copy(h.Exchange[:], datagram[40:56])
	h.Sequence = binary.BigEndian.Uint64(datagram[56:64])
	h.LeaseSeconds = binary.BigEndian.Uint16(datagram[64:66])
	h.KeyID = binary.BigEndian.Uint16(datagram[68:70])
	if h.Network != c.network || h.Sender != expectedSender || h.KeyID != c.keyID {
		return Header{}, nil, ErrIdentity
	}
	dir := directionOf(h.Type)
	if dir == 0 {
		return Header{}, nil, ErrType
	}
	aead, err := c.decAEADFor(dir, h.Sender, h.Boot)
	if err != nil {
		return Header{}, nil, err
	}
	c.setNonce(h.Sequence)
	// Open into the codec's own buffer, not over the input: Decode must leave
	// the caller's datagram intact (callers redeliver it, and the daemon reads
	// into a shared buffer). The 72-byte header is the AAD, so tampering with
	// any identity/routing field fails authentication.
	pt, err := aead.Open(c.plain[:0], c.nonce[:], datagram[HeaderSize:HeaderSize+bodyLen+TagSize], datagram[:HeaderSize])
	if err != nil {
		return Header{}, nil, ErrAuth
	}
	if err := validateHeader(h, bodyLen); err != nil {
		return Header{}, nil, err
	}
	if err := validateBody(h, pt); err != nil {
		return Header{}, nil, err
	}
	return h, pt, nil
}

func validateHeader(h Header, bodyLen int) error {
	if bodyLen < 0 || bodyLen > MaxBody {
		return ErrLength
	}
	if h.Network == (NetworkID{}) || h.Sender == (NodeID{}) || h.Boot == (BootID{}) {
		return ErrIdentity
	}
	switch h.Type {
	case Announce:
		if h.Flags & ^Urgent != 0 {
			return ErrFlags
		}
		if h.Exchange != (ExchangeID{}) || h.LeaseSeconds != 0 || bodyLen != 0 {
			return ErrHeader
		}
	case Query, Ack:
		if h.Flags != 0 {
			return ErrFlags
		}
		if h.Exchange == (ExchangeID{}) || h.LeaseSeconds != 0 || bodyLen != 0 {
			return ErrHeader
		}
	case Report:
		if h.Flags & ^(CompleteRadios|CompleteLinks) != 0 {
			return ErrFlags
		}
		if h.Exchange == (ExchangeID{}) || h.LeaseSeconds != ReportLease {
			return ErrHeader
		}
	default:
		return ErrType
	}
	return nil
}
