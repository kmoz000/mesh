package radio

import "errors"

// Kernel-module family resolution. The hyfi-bridging.ko and emesh-sp.ko modules
// each register a generic-netlink family whose id is a runtime-assigned number
// that differs per device (the hashes differ across T2-A/T2-B), so the id is
// ALWAYS resolved by name with CTRL_CMD_GETFAMILY and NEVER hardcoded. The
// family *names* are strings compiled into the .ko (see ids.go).
//
// This file holds the pure message builder/parser and the resolver interface;
// the real socket round-trip is in family_linux.go. Tests use a mock resolver
// and craft GETFAMILY replies as bytes.

// FamilyResolver resolves a generic-netlink family id by name. The real
// implementation issues CTRL_CMD_GETFAMILY on a netlink socket; a mock returns
// canned ids so the rest of the package tests without a socket.
type FamilyResolver interface {
	Resolve(name string) (FamilyInfo, error)
}

// FamilyInfo is the resolved family: its id and any multicast groups (name -> id)
// discovered in the reply. Groups are kept because async events arrive on them.
type FamilyInfo struct {
	ID     uint16
	Groups map[string]uint32
}

// family resolution errors.
var (
	ErrFamilyNotFound = errors.New("radio: generic-netlink family not found")
	ErrFamilyReply    = errors.New("radio: malformed CTRL_CMD_GETFAMILY reply")
)

// buildGetFamily writes a CTRL_CMD_GETFAMILY request for name into buf and
// returns the framed message. It is pure (no socket) and tested on crafted
// bytes. seq/pid are the caller's; the socket layer fills real values.
func buildGetFamily(buf []byte, name string, seq, pid uint32) ([]byte, error) {
	w := newBuilder(buf)
	w.putHeader(genlIDCtrl, nlmFRequest|nlmFAck, seq, pid, ctrlCmdGetFamily, 1)
	w.putString(ctrlAttrFamilyName, name)
	return w.finalize()
}

// parseGetFamilyReply extracts the family id and multicast groups from the
// attribute region of a CTRL_CMD_GETFAMILY reply. It does explicit bounds
// checks via attrIter and allocates the groups map only if groups are present.
func parseGetFamilyReply(attrs []byte) (FamilyInfo, error) {
	var fi FamilyInfo
	haveID := false
	it := newAttrIter(attrs)
	for {
		atype, payload, ok := it.next()
		if !ok {
			break
		}
		switch atype {
		case ctrlAttrFamilyID:
			if v, ok := attrU16(payload); ok {
				fi.ID = v
				haveID = true
			}
		case ctrlAttrMcastGroups:
			fi.Groups = parseMcastGroups(payload)
		}
	}
	if it.err != nil {
		return FamilyInfo{}, it.err
	}
	if !haveID {
		return FamilyInfo{}, ErrFamilyReply
	}
	return fi, nil
}

// parseMcastGroups walks the nested CTRL_ATTR_MCAST_GROUPS set: an outer array
// of nests, each holding a NAME string and an ID u32.
func parseMcastGroups(payload []byte) map[string]uint32 {
	out := make(map[string]uint32)
	outer := newAttrIter(payload)
	for {
		_, grp, ok := outer.next()
		if !ok {
			break
		}
		var name string
		var id uint32
		var haveName, haveID bool
		inner := newAttrIter(grp)
		for {
			atype, p, ok := inner.next()
			if !ok {
				break
			}
			switch atype {
			case ctrlAttrMcastGrpName:
				name = cstr(p)
				haveName = true
			case ctrlAttrMcastGrpID:
				if v, ok := attrU32(p); ok {
					id = v
					haveID = true
				}
			}
		}
		if haveName && haveID {
			out[name] = id
		}
	}
	if len(out) == 0 {
		return nil
	}
	return out
}

// cstr trims a trailing NUL (and anything after it) from a netlink string
// attribute payload.
func cstr(p []byte) string {
	for i, b := range p {
		if b == 0 {
			return string(p[:i])
		}
	}
	return string(p)
}

// CachedResolver memoizes a FamilyResolver so each family name is resolved at
// most once per device. A negative result (not found) is also cached to avoid
// re-querying a module that is not loaded.
type CachedResolver struct {
	R     FamilyResolver
	cache map[string]cachedFamily
}

type cachedFamily struct {
	fi  FamilyInfo
	err error
}

// Resolve returns the cached result or resolves and caches it.
func (c *CachedResolver) Resolve(name string) (FamilyInfo, error) {
	if c.cache == nil {
		c.cache = make(map[string]cachedFamily)
	}
	if got, ok := c.cache[name]; ok {
		return got.fi, got.err
	}
	fi, err := c.R.Resolve(name)
	c.cache[name] = cachedFamily{fi: fi, err: err}
	return fi, err
}
