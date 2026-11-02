// Package gnss reads the device's GPS fix and computes great-circle distance,
// the inputs for distance-aware topology decisions. It does not drive the GPS
// hardware: on the target a vendor daemon (/usr/sbin/gps) owns the UART and
// parses NMEA, publishing the current fix to a text file; gnss consumes that
// file. This keeps wmeshd off the UART (two readers corrupt a tty) while still
// being low-level in the sense that matters — no gpsd, no library, just the
// parsed fix and haversine. It is pure Go and unit-tested without hardware.
package gnss

import (
	"errors"
	"math"
	"os"
	"strconv"
	"strings"
	"time"
)

// maxFixAge bounds how old the published fix may be and still count as valid:
// the vendor daemon rewrites the file about once a second, so a file older than
// this means the daemon stalled or died and the position is not to be trusted.
const maxFixAge = 5 * time.Second

// Fix is a GPS position. Present reports whether GPS hardware/daemon is there at
// all (the file exists and is fresh); Valid additionally requires a satellite
// fix (indoors Present is true but Valid is false). Lat/Lon are degrees.
type Fix struct {
	Lat, Lon float64
	AltM     float64
	Present  bool
	Valid    bool
	Age      time.Duration
}

// ErrNoFile is returned when the vendor fix file is absent (no GPS subsystem).
var ErrNoFile = errors.New("gnss: vendor fix file not found")

// ReadVendorFix parses the vendor gps daemon's published fix file, whose parsed
// line reads "latitude=<v>, longitude=<v>, altitude=<v>" with "NA"/empty when
// there is no lock. Present is set when the file exists and is fresh; Valid when
// latitude and longitude parse to real numbers. Raw NMEA lines in the file (the
// daemon's debug echo) are ignored.
func ReadVendorFix(path string) (Fix, error) {
	info, err := os.Stat(path)
	if err != nil {
		return Fix{}, ErrNoFile
	}
	b, err := os.ReadFile(path)
	if err != nil {
		return Fix{}, err
	}
	var f Fix
	f.Age = time.Since(info.ModTime())
	f.Present = f.Age <= maxFixAge
	fields := parseKV(string(b))
	lat, latOK := parseCoord(fields["latitude"])
	lon, lonOK := parseCoord(fields["longitude"])
	alt, _ := parseCoord(fields["altitude"])
	f.Lat, f.Lon, f.AltM = lat, lon, alt
	f.Valid = f.Present && latOK && lonOK
	return f, nil
}

// parseKV collects key=value pairs from the "latitude=..., longitude=..." line,
// tolerating the raw NMEA lines (which contain no '=' in key position we read).
func parseKV(s string) map[string]string {
	out := make(map[string]string, 3)
	for _, line := range strings.Split(s, "\n") {
		for _, tok := range strings.Split(line, ",") {
			tok = strings.TrimSpace(tok)
			eq := strings.IndexByte(tok, '=')
			if eq <= 0 {
				continue
			}
			key := strings.TrimSpace(tok[:eq])
			val := strings.TrimSpace(tok[eq+1:])
			if key == "latitude" || key == "longitude" || key == "altitude" {
				out[key] = val
			}
		}
	}
	return out
}

// parseCoord parses a coordinate value, treating "NA"/""/non-numeric as absent.
func parseCoord(s string) (float64, bool) {
	if s == "" || strings.EqualFold(s, "NA") {
		return 0, false
	}
	v, err := strconv.ParseFloat(s, 64)
	if err != nil {
		return 0, false
	}
	return v, true
}

const earthRadiusM = 6371000.0

// Distance returns the great-circle distance in metres between two fixes using
// the haversine formula. It returns 0 if either fix is not Valid.
func Distance(a, b Fix) float64 {
	if !a.Valid || !b.Valid {
		return 0
	}
	φ1 := a.Lat * math.Pi / 180
	φ2 := b.Lat * math.Pi / 180
	dφ := (b.Lat - a.Lat) * math.Pi / 180
	dλ := (b.Lon - a.Lon) * math.Pi / 180
	h := math.Sin(dφ/2)*math.Sin(dφ/2) +
		math.Cos(φ1)*math.Cos(φ2)*math.Sin(dλ/2)*math.Sin(dλ/2)
	return 2 * earthRadiusM * math.Asin(math.Min(1, math.Sqrt(h)))
}
