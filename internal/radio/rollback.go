package radio

import (
	"encoding/binary"
	"errors"
	"os"
	"path/filepath"
	"time"
)

// Rollback persistence. Before any actuation the transaction persists the
// inverse Change (the one that undoes the pending one) so that a crash, or a
// COMMIT that never arrives, can be recovered into a safe state. The record is
// a fixed binary layout (not JSON: stdlib-only, allocation-light, and trivially
// decodable), length-prefixed only for the netdev string.

// Clock is the time source the transaction uses. A mock returns a controllable
// time so the watchdog is tested without sleeping.
type Clock interface {
	Now() time.Time
}

// RealClock is the production Clock.
type RealClock struct{}

// Now returns the current time.
func (RealClock) Now() time.Time { return time.Now() }

// Store persists a single rollback record across the lifetime of a transaction
// (and across a daemon restart). It is byte-oriented so the record format stays
// in this package; tests use an in-memory implementation and touch no disk.
type Store interface {
	// Save replaces the stored record.
	Save(b []byte) error
	// Load returns the stored record and whether one was present.
	Load() ([]byte, bool, error)
	// Clear removes any stored record. Clearing an absent record is not an error.
	Clear() error
}

const rollbackVersion = 1

// RollbackRecord is the inverse of a pending change plus the context needed to
// apply it without re-reading device state. It is what Store persists.
type RollbackRecord struct {
	RadioIndex uint8
	Mech       Mechanism
	Inverse    Change // the change that undoes the pending one
	Netdev     string // AP netdev the inverse acts on (for CSA); may be empty for power
}

// errors for rollback handling.
var (
	ErrNoRollback   = errors.New("radio: cannot build a complete rollback for this change")
	ErrBadRollback  = errors.New("radio: malformed rollback record")
	ErrNeedWatchdog = errors.New("radio: sole-backhaul retune requires a watchdog deadline and a writable store")
)

// marshal encodes the record into a fixed binary layout:
//
//	[0]     version
//	[1]     RadioIndex
//	[2]     Mech
//	[3]     flags: bit0 SetChannel, bit1 SetTxPower
//	[4:6]   Channel (big-endian u16)
//	[6:8]   Width   (big-endian u16)
//	[8]     TxPower (int8)
//	[9:11]  netdev length (big-endian u16)
//	[11:]   netdev bytes
func (r *RollbackRecord) marshal() []byte {
	var flags uint8
	if r.Inverse.SetChannel {
		flags |= 1 << 0
	}
	if r.Inverse.SetTxPower {
		flags |= 1 << 1
	}
	b := make([]byte, 11+len(r.Netdev))
	b[0] = rollbackVersion
	b[1] = r.RadioIndex
	b[2] = byte(r.Mech)
	b[3] = flags
	binary.BigEndian.PutUint16(b[4:6], r.Inverse.Channel)
	binary.BigEndian.PutUint16(b[6:8], r.Inverse.Width)
	b[8] = byte(r.Inverse.TxPower)
	binary.BigEndian.PutUint16(b[9:11], uint16(len(r.Netdev)))
	copy(b[11:], r.Netdev)
	return b
}

// unmarshalRollback decodes a record produced by marshal, with bounds checks.
func unmarshalRollback(b []byte) (RollbackRecord, error) {
	var r RollbackRecord
	if len(b) < 11 || b[0] != rollbackVersion {
		return r, ErrBadRollback
	}
	nlen := int(binary.BigEndian.Uint16(b[9:11]))
	if 11+nlen != len(b) {
		return r, ErrBadRollback
	}
	r.RadioIndex = b[1]
	r.Mech = Mechanism(b[2])
	flags := b[3]
	r.Inverse.RadioIndex = b[1]
	r.Inverse.SetChannel = flags&(1<<0) != 0
	r.Inverse.SetTxPower = flags&(1<<1) != 0
	r.Inverse.Channel = binary.BigEndian.Uint16(b[4:6])
	r.Inverse.Width = binary.BigEndian.Uint16(b[6:8])
	r.Inverse.TxPower = int8(b[8])
	r.Netdev = string(b[11 : 11+nlen])
	return r, nil
}

// MemStore is an in-memory Store for tests and for a node that accepts losing
// the rollback on a hard power cut (not used for a sole-backhaul retune, which
// requires durability — supply a FileStore there).
type MemStore struct {
	data []byte
	ok   bool
}

func (m *MemStore) Save(b []byte) error {
	m.data = append(m.data[:0], b...)
	m.ok = true
	return nil
}

func (m *MemStore) Load() ([]byte, bool, error) {
	if !m.ok {
		return nil, false, nil
	}
	out := make([]byte, len(m.data))
	copy(out, m.data)
	return out, true, nil
}

func (m *MemStore) Clear() error {
	m.data = nil
	m.ok = false
	return nil
}

// FileStore persists the rollback record to a single file. It uses only os, so
// it compiles and tests on any host and under TinyGo; the write is replace-only
// (truncate) because there is at most one live record. Durability across a
// power cut is the file system's; this is the store a sole-backhaul retune
// needs. It is not used in unit tests (those use MemStore), so no real file is
// touched unless a caller wires a FileStore.
type FileStore struct {
	Path string
}

func (f *FileStore) Save(b []byte) error {
	if f.Path == "" {
		return ErrBadRollback
	}
	if dir := filepath.Dir(f.Path); dir != "" {
		_ = os.MkdirAll(dir, 0o755)
	}
	return os.WriteFile(f.Path, b, 0o600)
}

func (f *FileStore) Load() ([]byte, bool, error) {
	b, err := os.ReadFile(f.Path)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, false, nil
		}
		return nil, false, err
	}
	return b, true, nil
}

func (f *FileStore) Clear() error {
	if f.Path == "" {
		return nil
	}
	err := os.Remove(f.Path)
	if err != nil && !os.IsNotExist(err) {
		return err
	}
	return nil
}
