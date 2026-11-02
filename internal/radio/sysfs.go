package radio

import (
	"os"
	"path/filepath"
	"strconv"
	"strings"

	"wantasticmesh/internal/wmproto"
)

// Enumerator discovers radios from sysfs. Root defaults to "/" but tests set it
// to a temp directory holding a fake sysfs tree, so enumeration needs no real
// device and runs on any host. It reads only metadata that sysfs exposes
// directly: phy name and index under /sys/class/ieee80211, and the netdev ->
// phy association under /sys/class/net/<if>/phy80211. Everything that needs a
// live nl80211 call (channel lists, tx-power range, operating state) is left
// for the Linux probe to fill; here those fields stay zero with no Known bit.
type Enumerator struct {
	Root string
}

func (e *Enumerator) root() string {
	if e.Root == "" {
		return "/"
	}
	return e.Root
}

// Enumerate returns the radios visible in sysfs, each with Index/Phy set and,
// where resolvable, the AP netdev. The result is capability-shaped but
// approximate: no channel/power/state is proven without the nl80211 probe.
func (e *Enumerator) Enumerate() ([]Radio, error) {
	base := e.root()
	ieeeDir := filepath.Join(base, "sys", "class", "ieee80211")
	ents, err := os.ReadDir(ieeeDir)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil // no wireless subsystem; honest empty set
		}
		return nil, err
	}

	netIndex := e.netdevByPhy(base)

	radios := make([]Radio, 0, len(ents))
	for _, ent := range ents {
		phy := ent.Name()
		if phy == "" {
			continue
		}
		r := newRadio()
		r.Phy = phy
		r.Index = phyIndex(ieeeDir, phy)
		if nd, ok := netIndex[phy]; ok {
			r.Netdev = nd
		}
		radios = append(radios, r)
		if len(radios) >= wmproto.MaxRadios {
			break
		}
	}
	return radios, nil
}

// phyIndex reads /sys/class/ieee80211/<phy>/index, falling back to parsing the
// trailing digits of the phy name ("phy0" -> 0).
func phyIndex(ieeeDir, phy string) uint8 {
	if b, err := os.ReadFile(filepath.Join(ieeeDir, phy, "index")); err == nil {
		if n, ok := parseUint8(strings.TrimSpace(string(b))); ok {
			return n
		}
	}
	// fall back to trailing digits of the name.
	i := len(phy)
	for i > 0 && phy[i-1] >= '0' && phy[i-1] <= '9' {
		i--
	}
	if i < len(phy) {
		if n, ok := parseUint8(phy[i:]); ok {
			return n
		}
	}
	return 0
}

// netdevByPhy maps a phy name to the first netdev bound to it, read through
// /sys/class/net/<if>/phy80211/name (no os.Readlink, which keeps it identical on
// a temp-dir fake and sidesteps a TinyGo symlink question).
func (e *Enumerator) netdevByPhy(base string) map[string]string {
	out := make(map[string]string)
	netDir := filepath.Join(base, "sys", "class", "net")
	ents, err := os.ReadDir(netDir)
	if err != nil {
		return out
	}
	for _, ent := range ents {
		ifname := ent.Name()
		nameFile := filepath.Join(netDir, ifname, "phy80211", "name")
		b, err := os.ReadFile(nameFile)
		if err != nil {
			continue
		}
		phy := strings.TrimSpace(string(b))
		if phy == "" {
			continue
		}
		if _, seen := out[phy]; !seen {
			out[phy] = ifname
		}
	}
	return out
}

func parseUint8(s string) (uint8, bool) {
	n, err := strconv.ParseUint(s, 10, 8)
	if err != nil {
		return 0, false
	}
	return uint8(n), true
}
