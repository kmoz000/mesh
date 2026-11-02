package wmproto

import (
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
)

// backhaulLabel domain-separates the backhaul credential from the WMSH-v0-enc
// transport subkeys so the two never coincide.
var backhaulLabel = []byte("WMSH-v0-backhaul")

// BackhaulPSK derives the wireless-backhaul passphrase from the network master
// key. Both nodes hold the master key, so they compute the same 256-bit WPA2 PSK
// with no provisioning, onboarding (DPP/WPS) or secret exchanged on the air: the
// controller brings up the backhaul BSS with it and an agent's backhaul STA uses
// the same value (wpa_key_mgmt=WPA-PSK-SHA256). It is HMAC-SHA256 over a distinct
// label, mirroring deriveAEAD, rendered as 64 lowercase hex characters (a raw
// 32-byte PSK, which hostapd/wpa_supplicant accept in place of a passphrase).
//
// Security note: this binds the data-plane backhaul credential to the single
// shared master key, so a compromised node exposes the backhaul. Per-node
// identity and key rotation remain a production gate (research/CGO-TOOLCHAIN.md
// and the roadmap), tracked separately from backhaul formation.
func BackhaulPSK(masterKey []byte) (string, error) {
	if len(masterKey) != KeySize {
		return "", ErrKey
	}
	m := hmac.New(sha256.New, masterKey)
	m.Write(backhaulLabel)
	return hex.EncodeToString(m.Sum(nil)), nil
}
