package wmproto

import (
	"bytes"
	"testing"
)

// encodeReport is a helper that builds and encodes a Report with the given
// sequence using codec c.
func encodeReport(t *testing.T, c *Codec, seq uint64, dst []byte) int {
	t.Helper()
	rb := &ReportBody{NumRadios: 1}
	rb.Radios[0] = RadioRecord{Index: 0, Band: 3, Channel: 37, Width: 160, TxPower: 20}
	var body [MaxBody]byte
	encoded, err := AppendReport(body[:0], rb)
	if err != nil {
		t.Fatalf("AppendReport: %v", err)
	}
	h := Header{Type: Report, Flags: CompleteRadios, Network: testNet, Sender: testSender, Boot: testBoot, Exchange: testChal, Sequence: seq, LeaseSeconds: ReportLease, KeyID: 7}
	n, err := c.Encode(dst, h, encoded)
	if err != nil {
		t.Fatalf("Encode: %v", err)
	}
	return n
}

func TestBodyIsEncrypted(t *testing.T) {
	c := newTestCodec(t)
	rb := &ReportBody{NumRadios: 1}
	rb.Radios[0] = RadioRecord{Index: 0, Band: 3, Channel: 37, Width: 160, TxPower: 20}
	var body [MaxBody]byte
	encoded, _ := AppendReport(body[:0], rb)
	h := Header{Type: Report, Flags: CompleteRadios, Network: testNet, Sender: testSender, Boot: testBoot, Exchange: testChal, Sequence: 1, LeaseSeconds: ReportLease, KeyID: 7}
	var buf [MaxDatagram]byte
	n, err := c.Encode(buf[:], h, encoded)
	if err != nil {
		t.Fatal(err)
	}
	// The plaintext record bytes must not appear verbatim in the datagram body.
	if bytes.Contains(buf[HeaderSize:n], encoded) {
		t.Fatal("plaintext body found in datagram: body is not encrypted")
	}
	// The header (AAD) stays cleartext for routing/key selection.
	if buf[5] != byte(Report) {
		t.Fatal("header type should be cleartext")
	}
}

func TestWrongKeyCannotDecrypt(t *testing.T) {
	c := newTestCodec(t)
	otherKey := bytes.Repeat([]byte{0x11}, KeySize)
	other, err := NewCodec(testNet, 7, otherKey) // same network/keyID, different key
	if err != nil {
		t.Fatal(err)
	}
	var buf [MaxDatagram]byte
	n := encodeReport(t, c, 1, buf[:])
	if _, _, err := other.Decode(buf[:n], testSender); err != ErrAuth {
		t.Fatalf("wrong key: got %v want ErrAuth", err)
	}
}

func TestNonceVariesBySequence(t *testing.T) {
	c := newTestCodec(t)
	var a, b [MaxDatagram]byte
	na := encodeReport(t, c, 1, a[:])
	nb := encodeReport(t, c, 2, b[:])
	if na != nb {
		t.Fatalf("lengths differ: %d vs %d", na, nb)
	}
	// Same body, same key, different sequence -> different ciphertext+tag.
	if bytes.Equal(a[HeaderSize:na], b[HeaderSize:nb]) {
		t.Fatal("ciphertext identical across sequences: nonce not varying")
	}
}

func TestDecodeIsNonDestructive(t *testing.T) {
	c := newTestCodec(t)
	var buf [MaxDatagram]byte
	n := encodeReport(t, c, 1, buf[:])
	snapshot := append([]byte(nil), buf[:n]...)
	if _, _, err := c.Decode(buf[:n], testSender); err != nil {
		t.Fatalf("first decode: %v", err)
	}
	if !bytes.Equal(buf[:n], snapshot) {
		t.Fatal("Decode mutated the input datagram")
	}
	// A second decode of the same bytes must still authenticate.
	if _, _, err := c.Decode(buf[:n], testSender); err != nil {
		t.Fatalf("second decode: %v", err)
	}
}

func TestBootChangeRederivesKey(t *testing.T) {
	// A sender that reboots (new boot id) must still decode under the same
	// master key: the subkey is rederived for the new boot.
	key := bytes.Repeat([]byte{0x5a}, KeySize)
	enc, _ := NewCodec(testNet, 7, key)
	dec, _ := NewCodec(testNet, 7, key)
	var buf [MaxDatagram]byte

	h := Header{Type: Announce, Network: testNet, Sender: testSender, Boot: testBoot, Sequence: 1, KeyID: 7}
	n, _ := enc.Encode(buf[:], h, nil)
	if _, _, err := dec.Decode(buf[:n], testSender); err != nil {
		t.Fatalf("boot1 decode: %v", err)
	}
	// Same codecs, new boot id, sequence restarts.
	boot2 := testBoot
	boot2[0] = 0x77
	h.Boot = boot2
	h.Sequence = 1
	n, _ = enc.Encode(buf[:], h, nil)
	if _, _, err := dec.Decode(buf[:n], testSender); err != nil {
		t.Fatalf("boot2 decode after rekey: %v", err)
	}
}
