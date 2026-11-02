package wmproto

import (
	"strings"
	"testing"
)

func TestBackhaulPSKDeterministicAndShaped(t *testing.T) {
	key := make([]byte, KeySize)
	for i := range key {
		key[i] = byte(i + 1)
	}
	a, err := BackhaulPSK(key)
	if err != nil {
		t.Fatal(err)
	}
	b, err := BackhaulPSK(key)
	if err != nil {
		t.Fatal(err)
	}
	if a != b {
		t.Fatal("BackhaulPSK must be deterministic for a given master key")
	}
	if len(a) != 64 {
		t.Fatalf("want 64 hex chars (32-byte PSK), got %d", len(a))
	}
	if strings.Trim(a, "0123456789abcdef") != "" {
		t.Fatalf("want lowercase hex, got %q", a)
	}
}

func TestBackhaulPSKKeyDependent(t *testing.T) {
	k1 := make([]byte, KeySize)
	k2 := make([]byte, KeySize)
	k2[0] = 1
	a, _ := BackhaulPSK(k1)
	b, _ := BackhaulPSK(k2)
	if a == b {
		t.Fatal("different master keys must yield different backhaul PSKs")
	}
}

func TestBackhaulPSKRejectsWrongKeyLen(t *testing.T) {
	if _, err := BackhaulPSK(make([]byte, KeySize-1)); err == nil {
		t.Fatal("expected error for short key")
	}
}

// The backhaul PSK must not collide with a transport subkey derived from the
// same master key (domain separation).
func TestBackhaulPSKDomainSeparated(t *testing.T) {
	key := make([]byte, KeySize)
	psk, _ := BackhaulPSK(key)
	c, err := NewCodec(NetworkID{1}, 1, key)
	if err != nil {
		t.Fatal(err)
	}
	aead, err := c.deriveAEAD(dirAgent, NodeID{}, BootID{})
	if err != nil {
		t.Fatal(err)
	}
	_ = aead
	// The enc subkey is in c.subkey after deriveAEAD; its hex must differ.
	var sub [64]byte
	n := copy(sub[:], []byte(psk))
	_ = n
	if psk == hexOf(c.subkey[:KeySize]) {
		t.Fatal("backhaul PSK collides with transport subkey")
	}
}

func hexOf(b []byte) string {
	const h = "0123456789abcdef"
	out := make([]byte, 0, len(b)*2)
	for _, x := range b {
		out = append(out, h[x>>4], h[x&0xf])
	}
	return string(out)
}
