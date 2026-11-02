package wmproto

import (
	"math"
	"testing"
)

func TestPositionRoundTrip(t *testing.T) {
	in := ReportBody{
		NumRadios:    1,
		Radios:       [MaxRadios]RadioRecord{{Index: 3, Band: 3, Flags: RadioBackhaul}},
		NumLinks:     1,
		Links:        [MaxLinks]LinkRecord{{LocalRadio: 3, RSSI: -44, Flags: LinkBackhaul | LinkBidirectional}},
		NumPositions: 1,
		Positions: [MaxPositions]PositionRecord{{
			Lat:   EncodeLatLon(37.422),
			Lon:   EncodeLatLon(-122.084),
			AltM:  EncodeAltM(12.5),
			HDOP:  EncodeHDOP(0.9),
			Flags: PositionValid,
		}},
	}
	buf, err := AppendReport(nil, &in)
	if err != nil {
		t.Fatal(err)
	}
	var out ReportBody
	if err := ParseReport(buf, &out); err != nil {
		t.Fatal(err)
	}
	if out.NumPositions != 1 {
		t.Fatalf("NumPositions=%d", out.NumPositions)
	}
	p := out.Positions[0]
	if !p.Valid() {
		t.Error("expected PositionValid")
	}
	if math.Abs(p.LatDeg()-37.422) > 1e-6 || math.Abs(p.LonDeg()-(-122.084)) > 1e-6 {
		t.Errorf("latlon decode: %f,%f", p.LatDeg(), p.LonDeg())
	}
	if p.AltMeters() != 12 && p.AltMeters() != 13 { // rounded metre
		t.Errorf("alt decode: %f", p.AltMeters())
	}
	if p.HDOP != 9 {
		t.Errorf("hdop=%d want 9", p.HDOP)
	}
}

// Three states: absent (np=0), present-no-fix (np=1,Valid=0), fix (np=1,Valid=1).
func TestPositionThreeStates(t *testing.T) {
	// absent
	b0, _ := AppendReport(nil, &ReportBody{})
	var o0 ReportBody
	if err := ParseReport(b0, &o0); err != nil || o0.NumPositions != 0 {
		t.Fatalf("absent: err=%v np=%d", err, o0.NumPositions)
	}
	// present, no fix
	b1, _ := AppendReport(nil, &ReportBody{NumPositions: 1}) // Flags=0 => not Valid
	var o1 ReportBody
	if err := ParseReport(b1, &o1); err != nil {
		t.Fatal(err)
	}
	if o1.NumPositions != 1 || o1.Positions[0].Valid() {
		t.Fatalf("present-no-fix: np=%d valid=%v", o1.NumPositions, o1.Positions[0].Valid())
	}
}

func TestEncodeClamps(t *testing.T) {
	if EncodeAltM(1e9) != 32767 || EncodeAltM(-1e9) != -32768 {
		t.Error("alt not clamped")
	}
	if EncodeHDOP(1000) != 255 || EncodeHDOP(-1) != 0 {
		t.Error("hdop not clamped")
	}
}
