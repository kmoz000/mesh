//go:build linux

// wmeshd provides the foreground daemon and its local diagnostic CLI.
// The current daemon mode is ingress-only and never changes radio state.
package main

import (
	"encoding/binary"
	"encoding/hex"
	"flag"
	"fmt"
	"os"
	"strconv"
	"strings"
	"syscall"
	"time"
	"unsafe"

	"wantasticmesh/internal/ieee1905"
	"wantasticmesh/internal/topology"
	"wantasticmesh/internal/wmproto"
)

const (
	maxFrame = 2048
)

var p1905Group = [6]byte{0x01, 0x80, 0xc2, 0x00, 0x00, 0x13}

func main() {
	args := os.Args[1:]
	if len(args) == 0 {
		usage()
		os.Exit(2)
	}
	switch args[0] {
	case "daemon", "observe":
		fs := flag.NewFlagSet(args[0], flag.ContinueOnError)
		seconds := fs.Int("duration", 0, "stop after this many seconds (0 runs until terminated)")
		if err := fs.Parse(args[1:]); err != nil {
			os.Exit(2)
		}
		if fs.NArg() != 0 || *seconds < 0 {
			fmt.Fprintln(os.Stderr, "invalid daemon arguments")
			os.Exit(2)
		}
		if os.Geteuid() != 0 {
			fmt.Fprintln(os.Stderr, "need root or CAP_NET_RAW to open an IEEE 1905 packet socket")
			os.Exit(1)
		}
		if err := observe(*seconds); err != nil {
			fmt.Fprintln(os.Stderr, "wmeshd:", err)
			os.Exit(1)
		}
	case "version", "--version":
		fmt.Println("wmeshd 0.1.0-dev (IEEE 1905 observer; WantasticMesh v0 wire codec; daemon exchange not wired)")
	case "selftest":
		if err := selftest(); err != nil {
			fmt.Fprintln(os.Stderr, "wmeshd selftest:", err)
			os.Exit(1)
		}
		fmt.Println("selftest ok: wmproto v0 encode/decode and report round-trip verified")
	case "check":
		if err := checkFrame(args[1:]); err != nil {
			fmt.Fprintln(os.Stderr, "wmeshd check:", err)
			os.Exit(1)
		}
	case "controller":
		if err := runController(args[1:]); err != nil {
			fmt.Fprintln(os.Stderr, "wmeshd controller:", err)
			os.Exit(1)
		}
	case "agent":
		if err := runAgent(args[1:]); err != nil {
			fmt.Fprintln(os.Stderr, "wmeshd agent:", err)
			os.Exit(1)
		}
	case "status":
		if err := runStatus(args[1:]); err != nil {
			fmt.Fprintln(os.Stderr, "wmeshd status:", err)
			os.Exit(1)
		}
	case "radio":
		if err := runRadio(args[1:]); err != nil {
			fmt.Fprintln(os.Stderr, "wmeshd radio:", err)
			os.Exit(1)
		}
	case "backhaul":
		if err := runBackhaul(args[1:]); err != nil {
			fmt.Fprintln(os.Stderr, "wmeshd backhaul:", err)
			os.Exit(1)
		}
	case "run":
		// Role is chosen by the ap_mode environment variable: "root" runs the
		// controller (the AP-mode root of the mesh), anything else runs the
		// relay (agent). The explicit controller/agent subcommands above remain
		// as overrides for development and tests.
		role, runErr := runByMode(args[1:])
		if runErr != nil {
			fmt.Fprintf(os.Stderr, "wmeshd run (%s): %v\n", role, runErr)
			os.Exit(1)
		}
	case "help", "--help", "-h":
		usage()
	default:
		fmt.Fprintf(os.Stderr, "unknown command %q\n", args[0])
		usage()
		os.Exit(2)
	}
}

func usage() {
	fmt.Fprintln(os.Stderr, "usage: wmeshd <run|daemon|observe|controller|agent|status|radio|backhaul|check|selftest|version|help>")
	fmt.Fprintln(os.Stderr, "  run [role flags]             pick role from ap_mode env (root=controller, else relay)")
	fmt.Fprintln(os.Stderr, "  daemon [-duration seconds]   run the read-only packet observer")
	fmt.Fprintln(os.Stderr, "  observe [-duration seconds]  alias of daemon")
	fmt.Fprintln(os.Stderr, "  controller -net <hex> -agents <file> [-id <hex>] [-port n] [-state f] [-duration s]")
	fmt.Fprintln(os.Stderr, "                               run the controller role (query/report/ack exchange)")
	fmt.Fprintln(os.Stderr, "  agent -net <hex> -controller-id <hex> [-id <hex>] [-key-file f] [-key-id n] [-port n] [-duration s]")
	fmt.Fprintln(os.Stderr, "                               run the agent role (answers queries with observed links)")
	fmt.Fprintln(os.Stderr, "  status -state <file>         print a controller's last written graph")
	fmt.Fprintln(os.Stderr, "  radio <show|txpower|channel> show radios, or guardedly set tx power / switch channel (txpower & channel need root)")
	fmt.Fprintln(os.Stderr, "  backhaul <form|status|down>  form/inspect/remove wmeshd's own WPA3-SAE wireless backhaul STA (form/down need root)")
	fmt.Fprintln(os.Stderr, "  check <ethernet-frame-hex>   validate one IEEE 1905 frame")
	fmt.Fprintln(os.Stderr, "  selftest                     verify the WantasticMesh v0 codec in-process")
}

// selftest exercises the WantasticMesh v0 codec end to end without any I/O so
// it can confirm the crypto and codec paths work on the target at runtime. It
// uses a fixed non-secret key; it authenticates nothing on the wire.
func selftest() error {
	network := wmproto.NetworkID{1, 2, 3, 4, 5, 6, 7, 8}
	sender := wmproto.NodeID{0xaa, 0xbb, 0xcc, 0xdd, 0xee, 0xff, 1, 2, 3, 4, 5, 6, 7, 8, 9, 10}
	boot := wmproto.BootID{9, 9, 9, 9, 9, 9, 9, 9}
	chal := wmproto.ExchangeID{0xde, 0xad, 0xbe, 0xef, 1, 2, 3, 4, 5, 6, 7, 8, 9, 10, 11, 12}
	var key [wmproto.KeySize]byte
	for i := range key {
		key[i] = 0x5a
	}
	codec, err := wmproto.NewCodec(network, 1, key[:])
	if err != nil {
		return err
	}

	rb := &wmproto.ReportBody{NumRadios: 1, NumLinks: 1}
	rb.Radios[0] = wmproto.RadioRecord{Index: 0, Band: 3, Channel: 37, Width: 160, TxPower: 23, Utilization: 42, MLDLink: 0}
	var peerMAC [6]byte
	copy(peerMAC[:], sender[:6])
	rb.Links[0] = wmproto.LinkRecord{LocalRadio: 0, Peer: peerMAC, RSSI: -58, AgeMs: 800, Confidence: 180, Flags: 1}
	var body [wmproto.MaxBody]byte
	encoded, err := wmproto.AppendReport(body[:0], rb)
	if err != nil {
		return err
	}
	h := wmproto.Header{Type: wmproto.Report, Flags: wmproto.CompleteRadios | wmproto.CompleteLinks, Network: network, Sender: sender, Boot: boot, Exchange: chal, Sequence: 1, LeaseSeconds: wmproto.ReportLease, KeyID: 1}
	var datagram [wmproto.MaxDatagram]byte
	n, err := codec.Encode(datagram[:], h, encoded)
	if err != nil {
		return err
	}
	gotH, gotBody, err := codec.Decode(datagram[:n], sender)
	if err != nil {
		return err
	}
	var out wmproto.ReportBody
	if err := wmproto.ParseReport(gotBody, &out); err != nil {
		return err
	}
	if gotH.Sequence != 1 || out.NumRadios != 1 || out.NumLinks != 1 || out.Radios[0] != rb.Radios[0] || out.Links[0] != rb.Links[0] {
		return fmt.Errorf("round-trip mismatch")
	}

	var ctrl wmproto.Controller
	nowFn := time.Unix(1000, 0)
	if err := ctrl.IssueChallenge(sender, chal, nowFn); err != nil {
		return err
	}
	if err := ctrl.AcceptReport(gotH, nowFn); err != nil {
		return err
	}
	return nil
}

func checkFrame(args []string) error {
	if len(args) != 1 {
		return fmt.Errorf("usage: wmeshd check <ethernet-frame-hex>")
	}
	encoded := strings.TrimPrefix(args[0], "0x")
	if len(encoded) > maxFrame*2 {
		return fmt.Errorf("frame exceeds %d bytes", maxFrame)
	}
	var frame [maxFrame]byte
	n, err := hex.Decode(frame[:], []byte(encoded))
	if err != nil {
		return fmt.Errorf("decode hex frame: %w", err)
	}
	msg, err := ieee1905.ParseEthernet(frame[:n])
	if err != nil {
		return err
	}
	fmt.Printf("valid IEEE1905 src=%x dst=%x type=0x%04x id=%d fragment=%d last=%t fragmented=%t al=%t interface=%t tlvs=%d\n", msg.Src, msg.Dst, msg.MsgType, msg.MessageID, msg.Fragment, msg.LastFragment, msg.Fragmented, msg.HasALMAC, msg.HasInterface, msg.TLVCount)
	return nil
}

func observe(durationSeconds int) error {
	protocol := htons(ieee1905.EtherType)
	fd, err := syscall.Socket(syscall.AF_PACKET, syscall.SOCK_RAW, int(protocol))
	if err != nil {
		return fmt.Errorf("open AF_PACKET socket: %w", err)
	}
	defer syscall.Close(fd)
	if err := syscall.Bind(fd, &syscall.SockaddrLinklayer{Protocol: protocol}); err != nil {
		return fmt.Errorf("bind IEEE 1905 packet socket: %w", err)
	}
	joined, failed := join1905Multicast(fd)
	if joined == 0 {
		return fmt.Errorf("could not join IEEE 1905 multicast on any interface (failures=%d)", failed)
	}
	if err := syscall.SetNonblock(fd, true); err != nil {
		return fmt.Errorf("set nonblocking: %w", err)
	}
	if err := syscall.SetsockoptInt(fd, syscall.SOL_SOCKET, syscall.SO_RCVBUF, 256*1024); err != nil {
		return fmt.Errorf("set receive buffer: %w", err)
	}

	fmt.Printf("wmeshd daemon started ether=0x%04x mode=observe-only ingress=1 multicast-memberships=%d failed=%d max-peers=%d\n", ieee1905.EtherType, joined, failed, topology.MaxPeers)
	var peers topology.Table
	var frame [maxFrame]byte
	var rx, valid, malformed, fragments, topoCount uint64
	lastReport := time.Now()
	deadline := time.Time{}
	if durationSeconds > 0 {
		deadline = time.Now().Add(time.Duration(durationSeconds) * time.Second)
	}

	for {
		n, truncated, recvErr := recvPacket(fd, frame[:])
		if recvErr == 0 {
			rx++
			if truncated {
				malformed++
			} else if msg, parseErr := ieee1905.ParseEthernet(frame[:n]); parseErr != nil || !ieee1905.IsUsableSource(msg.Src) {
				malformed++
			} else {
				valid++
				if msg.Fragmented {
					fragments++
				} else if validTopology(msg) {
					topoCount++
					if peers.Update(msg, time.Now()) {
						writeNode(msg.ALMAC, msg.Src)
					}
				}
			}
		} else if recvErr != syscall.EAGAIN && recvErr != syscall.EWOULDBLOCK && recvErr != syscall.EINTR {
			return fmt.Errorf("receive packet: %w", recvErr)
		}

		now := time.Now()
		if now.Sub(lastReport) >= 10*time.Second {
			writeStats("stats", rx, valid, malformed, fragments, topoCount, peers.Count(now))
			lastReport = now
		}
		if !deadline.IsZero() && !now.Before(deadline) {
			writeStats("summary", rx, valid, malformed, fragments, topoCount, peers.Count(now))
			return nil
		}
		if recvErr == syscall.EAGAIN || recvErr == syscall.EWOULDBLOCK {
			time.Sleep(20 * time.Millisecond)
		}
	}
}

func join1905Multicast(fd int) (joined, failed int) {
	entries, err := os.ReadDir("/sys/class/net")
	if err != nil {
		return 0, 1
	}
	for _, entry := range entries {
		name := entry.Name()
		kind, err := os.ReadFile("/sys/class/net/" + name + "/type")
		if err != nil || stringTrimSpace(kind) != "1" {
			continue // only Ethernet-like interfaces
		}
		data, err := os.ReadFile("/sys/class/net/" + name + "/ifindex")
		if err != nil {
			failed++
			continue
		}
		index, err := strconv.Atoi(stringTrimSpace(data))
		if err != nil || index <= 0 {
			failed++
			continue
		}
		var req [16]byte // struct packet_mreq: ifindex, type, alen, address[8]
		binary.LittleEndian.PutUint32(req[0:4], uint32(index))
		binary.LittleEndian.PutUint16(req[4:6], uint16(syscall.PACKET_MR_MULTICAST))
		binary.LittleEndian.PutUint16(req[6:8], uint16(len(p1905Group)))
		copy(req[8:14], p1905Group[:])
		if err := syscall.SetsockoptString(fd, syscall.SOL_PACKET, syscall.PACKET_ADD_MEMBERSHIP, string(req[:])); err != nil {
			failed++
			fmt.Fprintf(os.Stderr, "multicast membership skipped iface=%s ifindex=%d: %v\n", name, index, err)
			continue
		}
		joined++
	}
	return joined, failed
}

func stringTrimSpace(b []byte) string {
	start, end := 0, len(b)
	for start < end && (b[start] == ' ' || b[start] == '\n' || b[start] == '\t' || b[start] == '\r') {
		start++
	}
	for end > start && (b[end-1] == ' ' || b[end-1] == '\n' || b[end-1] == '\t' || b[end-1] == '\r') {
		end--
	}
	return string(b[start:end])
}

func validTopology(msg ieee1905.Message) bool {
	if msg.Version != 0 || !msg.HasALMAC || !ieee1905.IsUsableSource(msg.ALMAC) {
		return false
	}
	switch msg.MsgType {
	case ieee1905.MsgTopologyDiscovery:
		return msg.HasInterface && ieee1905.IsUsableSource(msg.InterfaceMAC)
	case ieee1905.MsgTopologyNotification:
		return true
	default:
		return false
	}
}

func htons(v uint16) uint16 { return v<<8 | v>>8 }

// recvPacket avoids syscall.Recvmsg's Sockaddr allocation on every frame.
// Linux returns the original datagram size when MSG_TRUNC is an input flag.
func recvPacket(fd int, buf []byte) (n int, truncated bool, err syscall.Errno) {
	if len(buf) == 0 {
		return 0, false, syscall.EINVAL
	}
	r1, _, errno := syscall.Syscall6(
		syscall.SYS_RECVFROM,
		uintptr(fd),
		uintptr(unsafe.Pointer(&buf[0])),
		uintptr(len(buf)),
		uintptr(syscall.MSG_TRUNC),
		0,
		0,
	)
	if errno != 0 {
		return 0, false, errno
	}
	n = int(r1)
	if n > len(buf) {
		return len(buf), true, 0
	}
	return n, false, 0
}

func writeStats(label string, rx, valid, malformed, fragments, topology uint64, peers int) {
	var buf [192]byte
	b := buf[:0]
	b = append(b, label...)
	b = append(b, " rx="...)
	b = appendUint(b, rx)
	b = append(b, " valid="...)
	b = appendUint(b, valid)
	b = append(b, " malformed="...)
	b = appendUint(b, malformed)
	b = append(b, " fragmented="...)
	b = appendUint(b, fragments)
	b = append(b, " topology="...)
	b = appendUint(b, topology)
	b = append(b, " peers="...)
	b = appendUint(b, uint64(peers))
	b = append(b, '\n')
	_, _ = syscall.Write(1, b)
}

func writeNode(al, source [6]byte) {
	var buf [64]byte
	b := append(buf[:0], "topology-node al="...)
	b = appendMAC(b, al)
	b = append(b, " source="...)
	b = appendMAC(b, source)
	b = append(b, '\n')
	_, _ = syscall.Write(1, b)
}

func appendMAC(dst []byte, mac [6]byte) []byte {
	const digits = "0123456789abcdef"
	for _, v := range mac {
		dst = append(dst, digits[v>>4], digits[v&0x0f])
	}
	return dst
}

func appendUint(dst []byte, v uint64) []byte {
	var tmp [20]byte
	i := len(tmp)
	if v == 0 {
		i--
		tmp[i] = '0'
	} else {
		for v > 0 {
			i--
			tmp[i] = byte('0' + v%10)
			v /= 10
		}
	}
	return append(dst, tmp[i:]...)
}
