//go:build linux

package main

import (
	"encoding/hex"
	"errors"
	"flag"
	"fmt"
	"os"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"

	"wantasticmesh/internal/agent"
	"wantasticmesh/internal/backhaul"
	"wantasticmesh/internal/controller"
	"wantasticmesh/internal/coord"
	"wantasticmesh/internal/gnss"
	"wantasticmesh/internal/ieee1905"
	"wantasticmesh/internal/nlwifi"
	"wantasticmesh/internal/radio"
	"wantasticmesh/internal/transport"
	"wantasticmesh/internal/wmproto"
)

// radioRefreshInterval is how often the agent re-probes its radios; capability
// state changes rarely, so this stays well above the announce cadence.
const radioRefreshInterval = 30 * time.Second

// meshGroup is the link-local IPv6 multicast group agents announce to and
// controllers join (ff02::1:9535). Announce-based discovery means neither role
// needs a pre-configured peer address: the controller learns an agent's address
// from its announce, and the agent learns the controller's from the query.
var meshGroup = [16]byte{0xff, 0x02, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0x01, 0x95, 0x35}

const (
	defaultPort   = 48213
	tickInterval  = 2 * time.Second
	neighborTTL   = 90 * time.Second
	approxLinkBit = 0x01 // LinkRecord.Flags: inferred from 1905 observation, not verified
	approxConf    = 32   // low confidence for observation-only links
)

// addrBook maps node ids to their last known transport address, bounded like
// the rest of the daemon's state.
type addrBook struct {
	used  [controller.MaxAgents]bool
	ids   [controller.MaxAgents]wmproto.NodeID
	addrs [controller.MaxAgents]transport.Addr
}

func (a *addrBook) note(id wmproto.NodeID, addr transport.Addr) {
	free := -1
	for i := range a.ids {
		if a.used[i] {
			if a.ids[i] == id {
				a.addrs[i] = addr
				return
			}
		} else if free < 0 {
			free = i
		}
	}
	if free >= 0 {
		a.used[free] = true
		a.ids[free] = id
		a.addrs[free] = addr
	}
}

func (a *addrBook) lookup(id wmproto.NodeID) (transport.Addr, bool) {
	for i := range a.ids {
		if a.used[i] && a.ids[i] == id {
			return a.addrs[i], true
		}
	}
	return transport.Addr{}, false
}

// observedNeighbors turns IEEE 1905 topology observations into approximate
// neighbor links. It is the agent's NeighborSource. A seen AL MAC is an
// advertised neighbor, not a verified bidirectional radio link, so every link
// it emits is flagged approximate with low confidence.
type observedNeighbors struct {
	used     [wmproto.MaxLinks]bool
	al       [wmproto.MaxLinks][6]byte
	lastSeen [wmproto.MaxLinks]time.Time
}

func (o *observedNeighbors) note(al [6]byte, now time.Time) {
	free, oldest := -1, -1
	for i := range o.used {
		if !o.used[i] {
			if free < 0 {
				free = i
			}
			continue
		}
		if o.al[i] == al {
			o.lastSeen[i] = now
			return
		}
		if oldest < 0 || o.lastSeen[i].Before(o.lastSeen[oldest]) {
			oldest = i
		}
	}
	idx := free
	if idx < 0 {
		idx = oldest
	}
	if idx < 0 {
		return
	}
	o.used[idx] = true
	o.al[idx] = al
	o.lastSeen[idx] = now
}

// Links implements agent.NeighborSource. It emits one approximate link per
// non-expired observed neighbor.
func (o *observedNeighbors) Links(dst []wmproto.LinkRecord, now time.Time) int {
	n := 0
	for i := range o.used {
		if !o.used[i] {
			continue
		}
		age := now.Sub(o.lastSeen[i])
		if age >= neighborTTL {
			o.used[i] = false
			continue
		}
		if n >= len(dst) {
			break
		}
		ms := age.Milliseconds()
		if ms > 0xffff {
			ms = 0xffff
		}
		dst[n] = wmproto.LinkRecord{
			LocalRadio: 0,
			Peer:       o.al[i], // the neighbor's observed MAC
			AgeMs:      uint16(ms),
			Confidence: approxConf,
			Flags:      approxLinkBit,
		}
		n++
	}
	return n
}

// measuredNeighbors emits *measured* links from nl80211 station dumps (real
// RSSI per associated peer), read through the nlwifi binding. On an AP/backhaul
// radio the associated peers are the clients and the backhaul STA that joined
// it, so once a backhaul link forms this reports it with a real signal and the
// LinkBackhaul flag — the high-confidence, measured half of the topology that
// the 1905-observed links only approximate.
type measuredNeighbors struct {
	src   *radio.Source
	peers peerProvider // optional: known peers to read by-MAC (QCA dumps are empty)
}

// measuredPeer is a peer whose metrics must be read by a doit GET_STATION by MAC
// because the QCA station *dump* is empty on AP/STA vaps. The backhaul manager
// supplies its uplink peer (the AP BSSID) this way so the leaf reports a real,
// measured backhaul link into the controller's topology.
type measuredPeer struct {
	LocalRadio uint8   // phy index hosting the local vdev (Radio.Index)
	StaIfName  string  // local vdev whose ifindex is queried, e.g. "wmbhsta0"
	MAC        [6]byte // peer MAC to query (for a STA vdev, the AP BSSID)
	Backhaul   bool
}

// peerProvider yields known peers to measure by MAC.
type peerProvider interface {
	MeasuredPeers() []measuredPeer
}

func (m *measuredNeighbors) Links(dst []wmproto.LinkRecord, now time.Time) int {
	if m == nil || m.src == nil {
		return 0
	}
	caps := m.src.Snapshot()
	n := 0
	for _, r := range caps.Radios {
		if n >= len(dst) || r.Netdev == "" {
			continue
		}
		idx, err := nlwifi.IfIndex(r.Netdev)
		if err != nil {
			continue
		}
		stas, err := nlwifi.Stations(idx)
		if err != nil {
			continue
		}
		for _, s := range stas {
			if n >= len(dst) {
				break
			}
			flags := wmproto.LinkBidirectional
			if r.Backhaul {
				flags |= wmproto.LinkBackhaul
			}
			ms := s.InactiveMs
			if ms > 0xffff {
				ms = 0xffff
			}
			dst[n] = wmproto.LinkRecord{
				LocalRadio: r.Index,
				Peer:       s.MAC,
				RSSI:       s.SignalDBm,
				AgeMs:      uint16(ms),
				Confidence: 200, // measured via nl80211, high confidence
				Flags:      flags,
			}
			n++
		}
	}
	// Known peers read by MAC: on QCA the station dump above returns nothing for
	// an AP/STA vap, but a doit GET_STATION by MAC succeeds. This is how the
	// backhaul uplink (peer = AP BSSID) becomes a measured link in the report.
	if m.peers != nil {
		for _, pr := range m.peers.MeasuredPeers() {
			if n >= len(dst) {
				break
			}
			idx, err := nlwifi.IfIndex(pr.StaIfName)
			if err != nil {
				continue
			}
			st, ok, err := nlwifi.GetStation(idx, pr.MAC)
			if err != nil || !ok {
				continue
			}
			flags := wmproto.LinkBidirectional
			if pr.Backhaul {
				flags |= wmproto.LinkBackhaul
			}
			ms := st.InactiveMs
			if ms > 0xffff {
				ms = 0xffff
			}
			dst[n] = wmproto.LinkRecord{
				LocalRadio: pr.LocalRadio,
				Peer:       pr.MAC,
				RSSI:       st.SignalDBm,
				AgeMs:      uint16(ms),
				Confidence: 200,
				Flags:      flags,
			}
			n++
		}
	}
	return n
}

// combinedNeighbors reports measured links first (real RSSI), then fills any
// remaining room with the 1905-observed approximate links.
type combinedNeighbors struct {
	measured *measuredNeighbors
	observed *observedNeighbors
}

func (c *combinedNeighbors) Links(dst []wmproto.LinkRecord, now time.Time) int {
	n := c.measured.Links(dst, now)
	if n < len(dst) {
		n += c.observed.Links(dst[n:], now)
	}
	return n
}

// --- shared config helpers ---

func parseHexInto(dst []byte, s, what string) error {
	s = strings.TrimPrefix(s, "0x")
	if len(s) != len(dst)*2 {
		return fmt.Errorf("%s must be %d hex chars, got %d", what, len(dst)*2, len(s))
	}
	if _, err := hex.Decode(dst, []byte(s)); err != nil {
		return fmt.Errorf("decode %s: %w", what, err)
	}
	return nil
}

// loadKey reads a 32-byte key from -key-file (hex or raw) or the WMESH_KEY env
// (hex). It never returns a default; a missing key is an error.
func loadKey(path string) ([]byte, error) {
	var raw []byte
	if path != "" {
		data, err := os.ReadFile(path)
		if err != nil {
			return nil, fmt.Errorf("read key file: %w", err)
		}
		raw = data
	} else if env := os.Getenv("WMESH_KEY"); env != "" {
		raw = []byte(env)
	} else {
		return nil, errors.New("no key provided: set -key-file or WMESH_KEY")
	}
	key := make([]byte, wmproto.KeySize)
	trimmed := strings.TrimSpace(string(raw))
	if len(trimmed) == wmproto.KeySize*2 {
		if _, err := hex.Decode(key, []byte(trimmed)); err == nil {
			return key, nil
		}
	}
	if len(raw) == wmproto.KeySize {
		copy(key, raw)
		return key, nil
	}
	return nil, fmt.Errorf("key must be %d raw bytes or %d hex chars", wmproto.KeySize, wmproto.KeySize*2)
}

// deriveNodeID builds a node id from the first usable Ethernet MAC when the
// operator does not pass -id. The MAC occupies the first six bytes.
func deriveNodeID() (wmproto.NodeID, error) {
	var id wmproto.NodeID
	entries, err := os.ReadDir("/sys/class/net")
	if err != nil {
		return id, err
	}
	for _, e := range entries {
		name := e.Name()
		if name == "lo" {
			continue
		}
		kind, err := os.ReadFile("/sys/class/net/" + name + "/type")
		if err != nil || stringTrimSpace(kind) != "1" {
			continue
		}
		macRaw, err := os.ReadFile("/sys/class/net/" + name + "/address")
		if err != nil {
			continue
		}
		mac := parseMAC(stringTrimSpace(macRaw))
		if mac == ([6]byte{}) {
			continue
		}
		copy(id[:6], mac[:])
		return id, nil
	}
	return id, errors.New("no usable Ethernet MAC to derive a node id; pass -id")
}

func parseMAC(s string) [6]byte {
	var mac [6]byte
	parts := strings.Split(s, ":")
	if len(parts) != 6 {
		return mac
	}
	for i, p := range parts {
		v, err := strconv.ParseUint(p, 16, 8)
		if err != nil {
			return [6]byte{}
		}
		mac[i] = byte(v)
	}
	return mac
}

// parseV4Addr parses a dotted-quad IPv4 string into an IPv4-mapped transport
// address on the given port, for unicast to a controller over a routed/tunnel
// backhaul. It rejects anything that is not four 0-255 octets.
func parseV4Addr(s string, port uint16) (transport.Addr, error) {
	parts := strings.Split(strings.TrimSpace(s), ".")
	if len(parts) != 4 {
		return transport.Addr{}, fmt.Errorf("controller-addr %q: want IPv4 a.b.c.d", s)
	}
	var o [4]byte
	for i, p := range parts {
		v, err := strconv.ParseUint(p, 10, 8)
		if err != nil {
			return transport.Addr{}, fmt.Errorf("controller-addr %q: bad octet %q", s, p)
		}
		o[i] = byte(v)
	}
	return transport.V4Mapped(o[0], o[1], o[2], o[3], port), nil
}

// usableIfindexes returns the indexes of Ethernet-type, non-loopback interfaces.
func usableIfindexes() []uint32 {
	var out []uint32
	entries, err := os.ReadDir("/sys/class/net")
	if err != nil {
		return out
	}
	for _, e := range entries {
		name := e.Name()
		if name == "lo" {
			continue
		}
		kind, err := os.ReadFile("/sys/class/net/" + name + "/type")
		if err != nil || stringTrimSpace(kind) != "1" {
			continue
		}
		idxRaw, err := os.ReadFile("/sys/class/net/" + name + "/ifindex")
		if err != nil {
			continue
		}
		idx, err := strconv.Atoi(stringTrimSpace(idxRaw))
		if err != nil || idx <= 0 {
			continue
		}
		out = append(out, uint32(idx))
	}
	return out
}

func openPacketObserver() (int, int, error) {
	protocol := htons(ieee1905.EtherType)
	fd, err := syscall.Socket(syscall.AF_PACKET, syscall.SOCK_RAW, int(protocol))
	if err != nil {
		return -1, 0, fmt.Errorf("open AF_PACKET socket: %w", err)
	}
	if err := syscall.Bind(fd, &syscall.SockaddrLinklayer{Protocol: protocol}); err != nil {
		syscall.Close(fd)
		return -1, 0, fmt.Errorf("bind packet socket: %w", err)
	}
	joined, _ := join1905Multicast(fd)
	if err := syscall.SetNonblock(fd, true); err != nil {
		syscall.Close(fd)
		return -1, 0, fmt.Errorf("set nonblocking: %w", err)
	}
	return fd, joined, nil
}

func parseDeadline(seconds int) time.Time {
	if seconds > 0 {
		return time.Now().Add(time.Duration(seconds) * time.Second)
	}
	return time.Time{}
}

// apMode reads the role selector from the environment. It accepts WMESH_AP_MODE
// first and the bare ap_mode name second, both case-insensitively. "root" means
// the controller role; any other value (including unset) means the relay role.
func apMode() string {
	v := os.Getenv("WMESH_AP_MODE")
	if v == "" {
		v = os.Getenv("ap_mode")
	}
	if strings.EqualFold(strings.TrimSpace(v), "root") {
		return "root"
	}
	return "relay"
}

// applyBackhaul marks radios whose netdev is listed in WMESH_BACKHAUL (comma- or
// space-separated, e.g. "ath2,ath3") as backhaul radios, per the operator's
// manual assignment at install time. The controller's coordination engine then
// never retunes them (the backhaul is respected) while it plans the others.
// Unlisted radios are left exactly as probed.
func applyBackhaul(caps *radio.Capabilities) {
	v := os.Getenv("WMESH_BACKHAUL")
	if v == "" {
		return
	}
	set := make(map[string]bool)
	for _, f := range strings.FieldsFunc(v, func(r rune) bool { return r == ',' || r == ' ' }) {
		if f != "" {
			set[f] = true
		}
	}
	for i := range caps.Radios {
		if set[caps.Radios[i].Netdev] {
			caps.Radios[i].Backhaul = true
		}
	}
}

// backhaulManager forms and keeps wmeshd's own wireless backhaul STA link on a
// leaf, from the agent's own network key — no separate credential. It is driven
// by install-time environment, consistent with WMESH_BACKHAUL / WMESH_AP_MODE:
//
//	WMESH_BACKHAUL_SSID   backhaul SSID to join (its presence enables formation)
//	WMESH_BACKHAUL_RADIO  radio netdev hosting the STA vdev, e.g. wifi2 (for its phy)
//	WMESH_BACKHAUL_STA_IF STA vdev name to create (default wmbhsta0)
//	WMESH_BACKHAUL_BSSID  optional AP BSSID lock
//	WMESH_BACKHAUL_4ADDR  "1" to create the vdev with 4addr (bridged backhaul)
//	WMESH_BACKHAUL_FREQ   optional scan frequency in MHz, e.g. 6595
//
// reconcile is called on each tick: it forms the link if absent and re-forms it
// if it has dropped, so the backhaul self-heals (the standing goal) without the
// manual `backhaul` subcommand. The SAE password is wmproto.BackhaulPSK(key),
// which both ends derive identically.
type backhaulManager struct {
	cfg      backhaul.Config
	interval time.Duration

	mu      sync.Mutex
	link    *backhaul.Link
	forming bool
	lastTry time.Time
}

// newBackhaulManagerFromEnv builds a manager from the environment and the
// network key. When WMESH_BACKHAUL_SSID is unset the manager is inert
// (wanted()==false) and reconcile does nothing. key is read here (before the
// caller wipes it) to derive the SAE password once.
func newBackhaulManagerFromEnv(key []byte) (*backhaulManager, error) {
	m := &backhaulManager{interval: 15 * time.Second}
	ssid := os.Getenv("WMESH_BACKHAUL_SSID")
	if ssid == "" {
		return m, nil // backhaul formation not requested
	}
	radioDev := os.Getenv("WMESH_BACKHAUL_RADIO")
	if radioDev == "" {
		return nil, errors.New("WMESH_BACKHAUL_SSID set but WMESH_BACKHAUL_RADIO (radio netdev) is empty")
	}
	phy, err := backhaul.PhyIndexOf(radioDev)
	if err != nil {
		return nil, fmt.Errorf("backhaul radio %q: %w", radioDev, err)
	}
	psk, err := wmproto.BackhaulPSK(key)
	if err != nil {
		return nil, fmt.Errorf("derive backhaul key: %w", err)
	}
	staIf := os.Getenv("WMESH_BACKHAUL_STA_IF")
	if staIf == "" {
		staIf = "wmbhsta0"
	}
	freq := 0
	if f := os.Getenv("WMESH_BACKHAUL_FREQ"); f != "" {
		freq, _ = strconv.Atoi(f)
	}
	m.cfg = backhaul.Config{
		Phy: phy, IfName: staIf, SSID: ssid,
		BSSID:       os.Getenv("WMESH_BACKHAUL_BSSID"),
		SAEPassword: psk,
		FourAddr:    os.Getenv("WMESH_BACKHAUL_4ADDR") == "1",
		ScanFreqMHz: freq,
	}
	return m, nil
}

// wanted reports whether backhaul formation was requested.
func (m *backhaulManager) wanted() bool { return m != nil && m.cfg.SSID != "" }

// reconcile decides, without blocking the agent loop, whether to (re)form the
// link. Forming takes several seconds (vdev create + scan + SAE), so it runs in
// a background goroutine: the single-threaded agent loop must keep servicing
// announces and queries meanwhile, or the controller never learns the node. One
// worker at a time, rate-limited to one attempt per interval.
func (m *backhaulManager) reconcile(now time.Time) {
	if !m.wanted() {
		return
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.forming || now.Sub(m.lastTry) < m.interval {
		return
	}
	m.forming = true
	m.lastTry = now
	go m.work()
}

// work runs one (re)formation attempt off the loop goroutine. It verifies an
// existing link, clears a stale/old vdev, forms afresh and records the result.
func (m *backhaulManager) work() {
	defer func() {
		m.mu.Lock()
		m.forming = false
		m.mu.Unlock()
	}()

	m.mu.Lock()
	link := m.link
	m.mu.Unlock()
	if link != nil {
		if ok, _ := link.Associated(); ok {
			return // still healthy
		}
		link.Teardown()
		m.mu.Lock()
		m.link = nil
		m.mu.Unlock()
	}
	// Clear any stale vdev left by a prior run/attempt before creating afresh.
	if _, e := nlwifi.IfIndex(m.cfg.IfName); e == nil {
		if l, ae := backhaul.Attach(backhaul.Config{IfName: m.cfg.IfName, GlobalCtrl: m.cfg.GlobalCtrl}); ae == nil {
			l.Teardown()
		}
	}
	formed, err := backhaul.Form(m.cfg, 20*time.Second)
	if err != nil {
		fmt.Fprintln(os.Stderr, "agent backhaul:", err)
		return
	}
	m.mu.Lock()
	m.link = formed
	m.mu.Unlock()
	st, ok, _ := formed.PeerSignal()
	if ok {
		fmt.Printf("agent backhaul up: %s <-> %s signal=%ddBm rx=%.0fMbit/s\n",
			m.cfg.IfName, formed.PeerBSSID(), st.SignalDBm, st.RxMbps())
	} else {
		fmt.Printf("agent backhaul up: %s <-> %s\n", m.cfg.IfName, formed.PeerBSSID())
	}
}

// MeasuredPeers reports the backhaul uplink peer (the AP BSSID) so the agent's
// measured-link pipeline reads it by MAC and includes it, flagged LinkBackhaul,
// in the report to the controller. Empty until the link is up.
func (m *backhaulManager) MeasuredPeers() []measuredPeer {
	if m == nil {
		return nil
	}
	m.mu.Lock()
	link := m.link
	m.mu.Unlock()
	if link == nil {
		return nil
	}
	bssid := m.cfg.BSSID
	if bssid == "" {
		bssid = link.PeerBSSID()
	}
	if bssid == "" {
		return nil
	}
	return []measuredPeer{{
		LocalRadio: uint8(m.cfg.Phy),
		StaIfName:  m.cfg.IfName,
		MAC:        parseMAC(bssid),
		Backhaul:   true,
	}}
}

// markRadio flags the backhaul host radio (the phy carrying the STA vdev) as a
// backhaul radio in a probed capability snapshot, so the controller's engine
// sees HasBackhaulRadio and expects a backhaul link on it. This ties the
// radio-level backhaul flag to the actual backhaul config rather than a
// separate env list.
func (m *backhaulManager) markRadio(caps *radio.Capabilities) {
	if !m.wanted() {
		return
	}
	for i := range caps.Radios {
		if int(caps.Radios[i].Index) == m.cfg.Phy {
			caps.Radios[i].Backhaul = true
		}
	}
}

// positionSourceOrNil returns g as an agent.PositionSource, or a true nil
// interface when g is nil (so agent sees no position source rather than a
// non-nil interface wrapping a nil pointer).
func positionSourceOrNil(g *gpsPositionSource) agent.PositionSource {
	if g == nil {
		return nil
	}
	return g
}

// gpsPositionSource is the agent's PositionSource (internal/agent) backed by the
// vendor GPS daemon's published fix file. It is installed only when GPS sync is
// enabled (WMESH_GPS); once enabled it always returns a record so the controller
// can tell "GPS present, no lock" (indoors) from "no GPS" (no record at all) —
// the PositionValid flag is set only on an actual satellite fix.
type gpsPositionSource struct{ path string }

func (g *gpsPositionSource) Position(_ time.Time) (wmproto.PositionRecord, bool) {
	var rec wmproto.PositionRecord
	if f, err := gnss.ReadVendorFix(g.path); err == nil && f.Valid {
		rec.Lat = wmproto.EncodeLatLon(f.Lat)
		rec.Lon = wmproto.EncodeLatLon(f.Lon)
		rec.AltM = wmproto.EncodeAltM(f.AltM)
		rec.Flags |= wmproto.PositionValid
	}
	return rec, true
}

// newGPSPositionSource returns a PositionSource when WMESH_GPS enables it, else
// nil (GPS reporting off). WMESH_GPS_FILE overrides the default fix-file path.
func newGPSPositionSource() *gpsPositionSource {
	v := os.Getenv("WMESH_GPS")
	if v != "1" && !strings.EqualFold(v, "true") && !strings.EqualFold(v, "on") {
		return nil
	}
	path := os.Getenv("WMESH_GPS_FILE")
	if path == "" {
		path = "/tmp/gps_info.txt"
	}
	return &gpsPositionSource{path: path}
}

// runByMode dispatches to the controller or agent role based on ap_mode. It
// returns the resolved role name alongside any error so the caller can report
// which role failed.
func runByMode(args []string) (string, error) {
	switch apMode() {
	case "root":
		fmt.Fprintln(os.Stderr, "wmeshd: ap_mode=root -> controller role")
		return "controller", runController(args)
	default:
		fmt.Fprintln(os.Stderr, "wmeshd: ap_mode!=root -> relay (agent) role")
		return "relay", runAgent(args)
	}
}

// --- controller role ---

func runController(args []string) error {
	fs := flag.NewFlagSet("controller", flag.ContinueOnError)
	netHex := fs.String("net", "", "network id, 16 hex chars (8 bytes)")
	idHex := fs.String("id", "", "controller node id, 32 hex chars (16 bytes); derived from a MAC if empty")
	agentsPath := fs.String("agents", "", "agent keystore file: lines of 'nodeid-hex keyid key-hex'")
	port := fs.Int("port", defaultPort, "UDP port")
	statePath := fs.String("state", "", "optional file to write the current graph for 'status'")
	staleTTL := fs.Int("stale-ttl", 0, "seconds of silence before a node is flagged stale (0 = default 5m); the self-heal engine emits a rejoin for a stale node")
	seconds := fs.Int("duration", 0, "stop after this many seconds (0 runs until terminated)")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if os.Geteuid() != 0 {
		return errors.New("need root or CAP_NET_RAW for the packet observer")
	}
	var network wmproto.NetworkID
	if err := parseHexInto(network[:], *netHex, "-net"); err != nil {
		return err
	}
	self, err := resolveID(*idHex)
	if err != nil {
		return err
	}
	ctrl, err := controller.New(network, self, nil)
	if err != nil {
		return err
	}
	if *staleTTL > 0 {
		ctrl.SetStaleTTL(time.Duration(*staleTTL) * time.Second)
	}
	if *agentsPath == "" {
		return errors.New("-agents keystore is required")
	}
	if err := loadAgents(ctrl, *agentsPath); err != nil {
		return err
	}

	udp, err := transport.ListenUDP(uint16(*port))
	if err != nil {
		return err
	}
	defer udp.Close()
	for _, idx := range usableIfindexes() {
		if err := udp.JoinMulticast(meshGroup, idx); err != nil {
			fmt.Fprintln(os.Stderr, "controller:", err)
		}
	}
	pktFd, joined, err := openPacketObserver()
	if err != nil {
		return err
	}
	defer syscall.Close(pktFd)

	fmt.Printf("wmeshd controller started port=%d multicast-joins=%d observer-joins=%d agents=%d\n", *port, len(usableIfindexes()), joined, agentCount(ctrl))

	var book addrBook
	var frame [maxFrame]byte
	var udpBuf [wmproto.MaxDatagram]byte
	var ackBuf [wmproto.MaxDatagram]byte
	var qBuf [wmproto.MaxDatagram]byte

	loop := transport.NewLoop(tickInterval)
	loop.Add(pktFd, func() {
		for {
			n, trunc, e := recvPacket(pktFd, frame[:])
			if e != 0 {
				return
			}
			if trunc {
				continue
			}
			if msg, err := ieee1905.ParseEthernet(frame[:n]); err == nil && !msg.Fragmented && validTopology(msg) {
				_ = msg // controller currently only needs agent reports; observer runs for parity
			}
		}
	})
	loop.Add(udp.Fd(), func() {
		for {
			n, from, trunc, e := udp.Recv(udpBuf[:])
			if e != 0 {
				return
			}
			if trunc {
				continue
			}
			now := time.Now()
			sender, ok, ack, herr := ctrl.HandleInbound(udpBuf[:n], ackBuf[:], now)
			if ok {
				book.note(sender, from)
			}
			if herr != nil {
				continue
			}
			if ack != nil {
				_ = udp.SendTo(from, ack)
			}
		}
	})
	tick := func(now time.Time) {
		ctrl.Maintain(now)
		for i := range book.used {
			if !book.used[i] {
				continue
			}
			id := book.ids[i]
			if !ctrl.NeedsQuery(id, now) {
				continue
			}
			n, qerr := ctrl.BuildQuery(id, qBuf[:], now)
			if qerr != nil {
				continue
			}
			_ = udp.SendTo(book.addrs[i], qBuf[:n])
		}
		if *statePath != "" {
			writeState(ctrl, *statePath, now)
		}
	}
	return loop.Run(time.Now, parseDeadline(*seconds), tick)
}

func resolveID(idHex string) (wmproto.NodeID, error) {
	if idHex != "" {
		var id wmproto.NodeID
		if err := parseHexInto(id[:], idHex, "-id"); err != nil {
			return id, err
		}
		return id, nil
	}
	return deriveNodeID()
}

func loadAgents(ctrl *controller.Controller, path string) error {
	data, err := os.ReadFile(path)
	if err != nil {
		return fmt.Errorf("read agents file: %w", err)
	}
	for ln, line := range strings.Split(string(data), "\n") {
		line = strings.TrimSpace(line)
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		fields := strings.Fields(line)
		if len(fields) != 3 {
			return fmt.Errorf("agents file line %d: want 'nodeid keyid key'", ln+1)
		}
		var id wmproto.NodeID
		if err := parseHexInto(id[:], fields[0], "agent id"); err != nil {
			return err
		}
		keyID, err := strconv.ParseUint(fields[1], 10, 16)
		if err != nil {
			return fmt.Errorf("agents file line %d: bad keyid", ln+1)
		}
		key := make([]byte, wmproto.KeySize)
		if err := parseHexInto(key, fields[2], "agent key"); err != nil {
			return err
		}
		if err := ctrl.AddAgent(id, uint16(keyID), key); err != nil {
			return fmt.Errorf("agents file line %d: %w", ln+1, err)
		}
	}
	return nil
}

func agentCount(ctrl *controller.Controller) int {
	var ids [controller.MaxAgents]wmproto.NodeID
	return ctrl.Agents(ids[:])
}

func writeState(ctrl *controller.Controller, path string, now time.Time) {
	var nodes [controller.MaxAgents]controller.Node
	n := ctrl.Nodes(nodes[:])
	var b strings.Builder
	fmt.Fprintf(&b, "wmeshd controller graph at %s nodes=%d\n", now.UTC().Format(time.RFC3339), n)
	for i := 0; i < n; i++ {
		nd := nodes[i]
		fmt.Fprintf(&b, "node %x boot=%x seq=%d radios=%d links=%d stale=%t age=%ds\n",
			nd.ID, nd.Boot, nd.Sequence, nd.NumRadios, nd.NumLinks, nd.Stale, int(now.Sub(nd.LastSeen).Seconds()))
		for j := 0; j < nd.NumRadios; j++ {
			r := nd.Radios[j]
			fmt.Fprintf(&b, "  radio idx=%d band=%d ch=%d width=%d util=%d backhaul=%t mlo=%t\n",
				r.Index, r.Band, r.Channel, r.Width, r.Utilization,
				r.Flags&wmproto.RadioBackhaul != 0, r.Flags&wmproto.RadioMLO != 0)
		}
		for j := 0; j < nd.NumLinks; j++ {
			l := nd.Links[j]
			fmt.Fprintf(&b, "  link peer=%x radio=%d rssi=%d age_ms=%d conf=%d flags=0x%02x\n",
				l.Peer, l.LocalRadio, l.RSSI, l.AgeMs, l.Confidence, l.Flags)
		}
		if nd.HasPosition {
			pr := nd.Position
			if pr.Valid() {
				fmt.Fprintf(&b, "  pos fix=true lat=%.6f lon=%.6f alt=%dm\n",
					pr.LatDeg(), pr.LonDeg(), int(pr.AltMeters()))
			} else {
				fmt.Fprintf(&b, "  pos fix=false (GPS present, no lock)\n")
			}
		} else {
			fmt.Fprintf(&b, "  pos none (GPS disabled)\n")
		}
	}
	// Inter-node great-circle distances, for nodes with a valid fix. This is the
	// distance-aware input the coordination layer will use (RSSI-vs-path-loss
	// sanity, nearest-backhaul ranking, moved-node detection); for now it is
	// surfaced in the state so a two-node deployment can be checked against the
	// real on-the-ground distance.
	type nodeFix struct {
		id  wmproto.NodeID
		fix gnss.Fix
	}
	var fixes []nodeFix
	for i := 0; i < n; i++ {
		nd := nodes[i]
		if nd.HasPosition && nd.Position.Valid() {
			fixes = append(fixes, nodeFix{nd.ID, gnss.Fix{
				Lat: nd.Position.LatDeg(), Lon: nd.Position.LonDeg(), Valid: true,
			}})
		}
	}
	for i := 0; i < len(fixes); i++ {
		for j := i + 1; j < len(fixes); j++ {
			d := gnss.Distance(fixes[i].fix, fixes[j].fix)
			fmt.Fprintf(&b, "dist %x<->%x = %.1fm\n", fixes[i].id, fixes[j].id, d)
		}
	}
	// Run the coordination + self-heal engine on the live graph and record its
	// plan. This is the controller "doing the thinking" on real device data:
	// interference-reducing channel moves (backhaul radios left alone) plus
	// per-node heal actions (silent node, missing/weak backhaul). The controller
	// only computes; each agent would apply its own directives.
	states := make([]coord.NodeState, 0, n)
	for i := 0; i < n; i++ {
		nd := nodes[i]
		radios := make([]coord.Radio, 0, nd.NumRadios)
		hasBHRadio := false
		for j := 0; j < nd.NumRadios; j++ {
			r := nd.Radios[j]
			bh := r.Flags&wmproto.RadioBackhaul != 0
			hasBHRadio = hasBHRadio || bh
			radios = append(radios, coord.Radio{
				Node: nd.ID, Index: r.Index, Band: r.Band,
				Channel: r.Channel, Width: r.Width, Backhaul: bh,
			})
		}
		hasBHLink := false
		var bestDBm int8
		for j := 0; j < nd.NumLinks; j++ {
			l := nd.Links[j]
			if l.Flags&wmproto.LinkBackhaul != 0 {
				hasBHLink = true
				if bestDBm == 0 || l.RSSI > bestDBm {
					bestDBm = l.RSSI
				}
			}
		}
		states = append(states, coord.NodeState{
			ID: nd.ID, Radios: radios, Stale: nd.Stale,
			HasBackhaulRadio: hasBHRadio, HasBackhaulLink: hasBHLink, BackhaulDBm: bestDBm,
		})
	}
	plan := coord.Engine{WeakBackhaulDBm: -75}.Tick(states, coord.AllAdjacent{})
	fmt.Fprintf(&b, "coordination: %d channel directives, %d heals\n", len(plan.Channels), len(plan.Heals))
	for _, d := range plan.Channels {
		fmt.Fprintf(&b, "  chan node=%x radio=%d band=%d %d->%d\n", d.Node, d.Index, d.Band, d.From, d.To)
	}
	for _, h := range plan.Heals {
		fmt.Fprintf(&b, "  heal node=%x kind=%s reason=%q\n", h.Node, h.Kind, h.Reason)
	}

	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, []byte(b.String()), 0o644); err != nil {
		return
	}
	_ = os.Rename(tmp, path)
}

// --- agent role ---

func runAgent(args []string) error {
	fs := flag.NewFlagSet("agent", flag.ContinueOnError)
	netHex := fs.String("net", "", "network id, 16 hex chars (8 bytes)")
	idHex := fs.String("id", "", "this agent's node id, 32 hex chars; derived from a MAC if empty")
	ctrlHex := fs.String("controller-id", "", "controller node id, 32 hex chars (16 bytes)")
	keyFile := fs.String("key-file", "", "file holding this agent's 32-byte key (hex or raw); or set WMESH_KEY")
	keyID := fs.Int("key-id", 1, "key id matching the controller's keystore entry")
	port := fs.Int("port", defaultPort, "UDP port")
	ctrlAddrStr := fs.String("controller-addr", "", "optional controller IPv4 address to unicast announces to, for routed/tunnel backhauls where multicast discovery cannot reach")
	seconds := fs.Int("duration", 0, "stop after this many seconds (0 runs until terminated)")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if os.Geteuid() != 0 {
		return errors.New("need root or CAP_NET_RAW for the packet observer")
	}
	var network wmproto.NetworkID
	if err := parseHexInto(network[:], *netHex, "-net"); err != nil {
		return err
	}
	self, err := resolveID(*idHex)
	if err != nil {
		return err
	}
	var ctrlID wmproto.NodeID
	if err := parseHexInto(ctrlID[:], *ctrlHex, "-controller-id"); err != nil {
		return err
	}
	key, err := loadKey(*keyFile)
	if err != nil {
		return err
	}
	// Build the backhaul manager from the network key before the key is wiped
	// below; it derives the SAE password once and keeps no key material after.
	bhMgr, err := newBackhaulManagerFromEnv(key)
	if err != nil {
		return err
	}
	gpsSrc := newGPSPositionSource()
	neigh := &observedNeighbors{}
	// Radio telemetry is best-effort: on a device the prober reads real radios
	// over nl80211/sysfs, but a host without wifi or without permission simply
	// reports zero radios rather than failing the agent.
	radioSrc := radio.NewSource(radio.Capabilities{})
	prober, perr := radio.NewLinuxProber("/")
	if perr != nil {
		fmt.Fprintln(os.Stderr, "agent: radio telemetry unavailable:", perr)
		prober = nil
	} else {
		defer prober.Close()
		if caps, e := prober.Probe(); e == nil {
			applyBackhaul(&caps)
			bhMgr.markRadio(&caps)
			radioSrc.Set(caps)
		} else {
			fmt.Fprintln(os.Stderr, "agent: radio probe:", e)
		}
	}
	ag, err := agent.New(agent.Config{
		Network:    network,
		Self:       self,
		Controller: ctrlID,
		KeyID:      uint16(*keyID),
		Key:        key,
		Neighbors:  &combinedNeighbors{measured: &measuredNeighbors{src: radioSrc, peers: bhMgr}, observed: neigh},
		Radios:     radioSrc,
		Position:   positionSourceOrNil(gpsSrc),
	})
	for i := range key { // wipe the key copy from this frame; the codec holds its own HMAC state
		key[i] = 0
	}
	if err != nil {
		return err
	}

	udp, err := transport.ListenUDP(uint16(*port))
	if err != nil {
		return err
	}
	defer udp.Close()
	ifindexes := usableIfindexes()
	for _, idx := range ifindexes {
		if err := udp.JoinMulticast(meshGroup, idx); err != nil {
			fmt.Fprintln(os.Stderr, "agent:", err)
		}
	}
	pktFd, joined, err := openPacketObserver()
	if err != nil {
		return err
	}
	defer syscall.Close(pktFd)

	fmt.Printf("wmeshd agent started port=%d observer-joins=%d boot=%x\n", *port, joined, ag.Boot())

	var frame [maxFrame]byte
	var udpBuf [wmproto.MaxDatagram]byte
	var outBuf [wmproto.MaxDatagram]byte
	var annBuf [wmproto.MaxDatagram]byte
	var ctrlAddr transport.Addr
	if *ctrlAddrStr != "" {
		seed, perr := parseV4Addr(*ctrlAddrStr, uint16(*port))
		if perr != nil {
			return perr
		}
		ctrlAddr = seed // unicast announces here from the start; discovery still runs in parallel
	}

	loop := transport.NewLoop(tickInterval)
	loop.Add(pktFd, func() {
		for {
			n, trunc, e := recvPacket(pktFd, frame[:])
			if e != 0 {
				return
			}
			if trunc {
				continue
			}
			if msg, perr := ieee1905.ParseEthernet(frame[:n]); perr == nil && !msg.Fragmented && validTopology(msg) {
				neigh.note(msg.ALMAC, time.Now())
			}
		}
	})
	loop.Add(udp.Fd(), func() {
		for {
			n, from, trunc, e := udp.Recv(udpBuf[:])
			if e != 0 {
				return
			}
			if trunc {
				continue
			}
			out, herr := ag.HandleInbound(udpBuf[:n], outBuf[:], time.Now())
			if herr != nil {
				continue
			}
			ctrlAddr = from // learn the controller address from its query/ack source
			if out != nil {
				_ = udp.SendTo(from, out)
			}
		}
	})
	if bhMgr.wanted() {
		fmt.Printf("agent: backhaul formation enabled (radio phy%d if=%s ssid=%q)\n",
			bhMgr.cfg.Phy, bhMgr.cfg.IfName, bhMgr.cfg.SSID)
		bhMgr.reconcile(time.Now()) // best-effort first attempt at startup
	}
	lastProbe := time.Now()
	tick := func(now time.Time) {
		bhMgr.reconcile(now)
		if prober != nil && now.Sub(lastProbe) >= radioRefreshInterval {
			lastProbe = now
			if caps, e := prober.Probe(); e == nil {
				applyBackhaul(&caps)
				bhMgr.markRadio(&caps)
				radioSrc.Set(caps)
			}
		}
		n, aerr := ag.Announce(annBuf[:], now)
		if aerr != nil {
			return
		}
		// Multicast the announce for discovery, and also unicast to a known
		// controller so re-contact after silence does not wait for a query.
		for _, idx := range ifindexes {
			_ = udp.SendTo(transport.Addr{IP: meshGroup, Zone: idx, Port: uint16(*port)}, annBuf[:n])
		}
		if !ctrlAddr.IsZero() {
			_ = udp.SendTo(ctrlAddr, annBuf[:n])
		}
	}
	return loop.Run(time.Now, parseDeadline(*seconds), tick)
}

// --- status role ---

func runStatus(args []string) error {
	fs := flag.NewFlagSet("status", flag.ContinueOnError)
	statePath := fs.String("state", "", "graph file written by a controller's -state")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if *statePath == "" {
		return errors.New("-state path is required")
	}
	data, err := os.ReadFile(*statePath)
	if err != nil {
		return fmt.Errorf("read state: %w", err)
	}
	os.Stdout.Write(data)
	return nil
}
