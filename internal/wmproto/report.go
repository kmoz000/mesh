package wmproto

import "encoding/binary"

// Report body schema v1 (tagless compact). Unlike IEEE 1905 TLVs, the body
// carries no per-field type or length tags: both ends know the layout from the
// header Version byte, so the only framing is two leading count bytes (radios,
// then links) followed by fixed-width records. This is denser than TLV and than
// TLV+deflate (deflate grows a payload this small), and it decodes with a
// single up-front bounds check into fixed arrays, with no allocation and no
// bit-level parsing. A neighbor is identified by the 6-byte MAC a radio hears,
// not a 16-byte node id, which is where most of the size saving comes from.
//
// CompleteRadios / CompleteLinks in the header mark a full set for that record
// class ("replace everything you hold"); their absence marks a partial update.

const (
	MaxRadios    = 8
	MaxLinks     = 32
	MaxPositions = 1 // at most one GPS position per node

	radioRecSize = 10 // Index,Band,Channel(2),Width(2),TxPower,Utilization,MLDLink,Flags
	linkRecSize  = 12 // LocalRadio,Peer(6),RSSI,AgeMs(2),Confidence,Flags
	posRecSize   = 12 // Lat(4),Lon(4),AltM(2),HDOP,Flags
	countsSize   = 3  // radio count + link count + position count

	MaxReportBody = countsSize + MaxRadios*radioRecSize + MaxLinks*linkRecSize + MaxPositions*posRecSize
)

// Link flag bits. A link observation is directional and may be approximate.
const (
	LinkApprox        uint8 = 1 << 0 // inferred from observation, not a verified link
	LinkBackhaul      uint8 = 1 << 1 // carries mesh backhaul traffic
	LinkBidirectional uint8 = 1 << 2 // confirmed usable in both directions
	LinkMLO           uint8 = 1 << 3 // part of a multi-link (MLO) association
)

// Radio flag bits.
const (
	RadioBackhaul uint8 = 1 << 0 // radio is used for a backhaul link
	RadioMLO      uint8 = 1 << 1 // radio participates in an MLD
)

// Position flag bits.
const (
	PositionValid uint8 = 1 << 0 // a satellite fix is present (else GPS-but-no-lock)
)

// latLonScale renders degrees as fixed-point int32 (1e-7 deg, ~1.1 cm, range
// well beyond +/-180). A node reporting a PositionRecord with PositionValid
// unset means "GPS present but no fix" (e.g. indoors); no record at all
// (NumPositions==0) means the node has no GPS.
const latLonScale = 1e7

// RadioRecord describes one physical radio as the originating node observes it.
// Band and Width carry raw values; wmproto validates structure, not regulatory
// policy. MLDLink is 0xff when the radio is not part of an MLD. Band and MLDLink
// together model multi-band and MLO operation.
type RadioRecord struct {
	Index       uint8
	Band        uint8
	Channel     uint16
	Width       uint16
	TxPower     int8
	Utilization uint8
	MLDLink     uint8
	Flags       uint8
}

// LinkRecord describes one directional link from the origin node to the radio
// whose MAC is Peer, measured on the origin's LocalRadio. A link in one
// direction does not imply the reverse works; AgeMs and Confidence qualify the
// measurement, and Flags marks backhaul/MLO/approximate status.
type LinkRecord struct {
	LocalRadio uint8
	Peer       [6]byte
	RSSI       int8
	AgeMs      uint16
	Confidence uint8
	Flags      uint8
}

// PositionRecord is a node's GPS position, fixed-point for a deterministic wire
// form (no floats on the wire). Lat/Lon are 1e-7 degrees; AltM is metres
// (saturating int16); HDOP is tenths (0 = unknown, saturating at 25.5). Flags
// carries PositionValid.
type PositionRecord struct {
	Lat   int32
	Lon   int32
	AltM  int16
	HDOP  uint8
	Flags uint8
}

// EncodeLatLon converts degrees to the fixed-point wire value.
func EncodeLatLon(deg float64) int32 {
	v := deg * latLonScale
	if v >= 0 {
		v += 0.5
	} else {
		v -= 0.5
	}
	if v > 2147483647 {
		v = 2147483647
	} else if v < -2147483648 {
		v = -2147483648
	}
	return int32(v)
}

// EncodeAltM converts metres to a saturating int16.
func EncodeAltM(m float64) int16 {
	if m >= 0 {
		m += 0.5
	} else {
		m -= 0.5
	}
	if m > 32767 {
		return 32767
	}
	if m < -32768 {
		return -32768
	}
	return int16(m)
}

// EncodeHDOP converts an HDOP value to tenths, saturating at 25.5. 0 = unknown.
func EncodeHDOP(h float64) uint8 {
	if h <= 0 {
		return 0
	}
	v := h*10 + 0.5
	if v > 255 {
		return 255
	}
	return uint8(v)
}

// LatDeg / LonDeg / AltMeters decode the fixed-point fields back to units.
func (p PositionRecord) LatDeg() float64    { return float64(p.Lat) / latLonScale }
func (p PositionRecord) LonDeg() float64    { return float64(p.Lon) / latLonScale }
func (p PositionRecord) AltMeters() float64 { return float64(p.AltM) }

// Valid reports whether this record carries a satellite fix.
func (p PositionRecord) Valid() bool { return p.Flags&PositionValid != 0 }

// ReportBody is a decoded Report body. It uses fixed backing arrays so decoding
// performs no allocation; NumRadios and NumLinks bound the valid entries.
type ReportBody struct {
	Radios       [MaxRadios]RadioRecord
	NumRadios    int
	Links        [MaxLinks]LinkRecord
	NumLinks     int
	Positions    [MaxPositions]PositionRecord
	NumPositions int
}

// AppendReport writes the v1 body for r to dst and returns the new slice. It
// returns ErrCapacity if a count exceeds its cap and ErrBody for a negative
// count. The result is suitable as the body argument to Codec.Encode.
func AppendReport(dst []byte, r *ReportBody) ([]byte, error) {
	if r == nil {
		return dst, ErrBody
	}
	if r.NumRadios < 0 || r.NumLinks < 0 || r.NumPositions < 0 {
		return dst, ErrBody
	}
	if r.NumRadios > MaxRadios || r.NumLinks > MaxLinks || r.NumPositions > MaxPositions {
		return dst, ErrCapacity
	}
	dst = append(dst, byte(r.NumRadios), byte(r.NumLinks), byte(r.NumPositions))
	for i := 0; i < r.NumRadios; i++ {
		rec := &r.Radios[i]
		dst = append(dst, rec.Index, rec.Band)
		dst = binary.BigEndian.AppendUint16(dst, rec.Channel)
		dst = binary.BigEndian.AppendUint16(dst, rec.Width)
		dst = append(dst, byte(rec.TxPower), rec.Utilization, rec.MLDLink, rec.Flags)
	}
	for i := 0; i < r.NumLinks; i++ {
		rec := &r.Links[i]
		dst = append(dst, rec.LocalRadio)
		dst = append(dst, rec.Peer[:]...)
		dst = append(dst, byte(rec.RSSI))
		dst = binary.BigEndian.AppendUint16(dst, rec.AgeMs)
		dst = append(dst, rec.Confidence, rec.Flags)
	}
	for i := 0; i < r.NumPositions; i++ {
		rec := &r.Positions[i]
		dst = binary.BigEndian.AppendUint32(dst, uint32(rec.Lat))
		dst = binary.BigEndian.AppendUint32(dst, uint32(rec.Lon))
		dst = binary.BigEndian.AppendUint16(dst, uint16(rec.AltM))
		dst = append(dst, rec.HDOP, rec.Flags)
	}
	return dst, nil
}

// reportBodyLen returns the exact byte length a well-formed body with the given
// counts must have, or -1 if the counts exceed the caps.
func reportBodyLen(nr, nl, np int) int {
	if nr > MaxRadios || nl > MaxLinks || np > MaxPositions {
		return -1
	}
	return countsSize + nr*radioRecSize + nl*linkRecSize + np*posRecSize
}

// ParseReport decodes a validated v1 body into out without allocation. out must
// be non-nil; its previous contents are overwritten. The body must be exactly
// the length its counts imply, with no trailing bytes.
func ParseReport(body []byte, out *ReportBody) error {
	if out == nil {
		return ErrBody
	}
	if len(body) < countsSize {
		return ErrBody
	}
	nr := int(body[0])
	nl := int(body[1])
	np := int(body[2])
	need := reportBodyLen(nr, nl, np)
	if need < 0 {
		return ErrCapacity
	}
	if len(body) != need {
		return ErrBody
	}
	out.NumRadios = nr
	out.NumLinks = nl
	out.NumPositions = np
	off := countsSize
	for i := 0; i < nr; i++ {
		p := body[off : off+radioRecSize]
		rec := &out.Radios[i]
		rec.Index = p[0]
		rec.Band = p[1]
		rec.Channel = binary.BigEndian.Uint16(p[2:4])
		rec.Width = binary.BigEndian.Uint16(p[4:6])
		rec.TxPower = int8(p[6])
		rec.Utilization = p[7]
		rec.MLDLink = p[8]
		rec.Flags = p[9]
		off += radioRecSize
	}
	for i := 0; i < nl; i++ {
		p := body[off : off+linkRecSize]
		rec := &out.Links[i]
		rec.LocalRadio = p[0]
		copy(rec.Peer[:], p[1:7])
		rec.RSSI = int8(p[7])
		rec.AgeMs = binary.BigEndian.Uint16(p[8:10])
		rec.Confidence = p[10]
		rec.Flags = p[11]
		off += linkRecSize
	}
	for i := 0; i < np; i++ {
		p := body[off : off+posRecSize]
		rec := &out.Positions[i]
		rec.Lat = int32(binary.BigEndian.Uint32(p[0:4]))
		rec.Lon = int32(binary.BigEndian.Uint32(p[4:8]))
		rec.AltM = int16(binary.BigEndian.Uint16(p[8:10]))
		rec.HDOP = p[10]
		rec.Flags = p[11]
		off += posRecSize
	}
	return nil
}

// scanReportBody validates body structure without decoding into a ReportBody.
// It is the allocation-free check used on the Decode hot path; callers that
// need the fields call ParseReport afterwards. It must stay consistent with
// ParseReport's acceptance rules.
func scanReportBody(body []byte) error {
	if len(body) < countsSize {
		return ErrBody
	}
	need := reportBodyLen(int(body[0]), int(body[1]), int(body[2]))
	if need < 0 {
		return ErrCapacity
	}
	if len(body) != need {
		return ErrBody
	}
	return nil
}

// validateBody enforces the per-type body rules. Announce, Query, and Ack have
// empty bodies (already checked by validateHeader); Report bodies are scanned
// for structural validity. It is allocation-free.
func validateBody(h Header, body []byte) error {
	switch h.Type {
	case Announce, Query, Ack:
		if len(body) != 0 {
			return ErrBody
		}
		return nil
	case Report:
		return scanReportBody(body)
	default:
		return ErrType
	}
}
