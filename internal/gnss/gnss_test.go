package gnss

import (
	"math"
	"os"
	"path/filepath"
	"testing"
)

func writeTmp(t *testing.T, content string) string {
	t.Helper()
	p := filepath.Join(t.TempDir(), "gps_info.txt")
	if err := os.WriteFile(p, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
	return p
}

func TestReadVendorFixNoLock(t *testing.T) {
	// The real indoor file: NMEA echo + a parsed line with NA coordinates.
	p := writeTmp(t, "$GNGGA,001122.333,,,,,0,00,99.99,,M,,M,,*1E\n"+
		"$GNGLL,,,,,001122.333,V,N*55\n"+
		"latitude=NA, longitude=NA, altitude=\n")
	f, err := ReadVendorFix(p)
	if err != nil {
		t.Fatalf("unexpected err: %v", err)
	}
	if !f.Present {
		t.Error("fresh file should be Present")
	}
	if f.Valid {
		t.Error("NA coordinates must not be Valid")
	}
}

func TestReadVendorFixWithLock(t *testing.T) {
	p := writeTmp(t, "latitude=37.422000, longitude=-122.084000, altitude=12.5\n")
	f, err := ReadVendorFix(p)
	if err != nil {
		t.Fatalf("unexpected err: %v", err)
	}
	if !f.Present || !f.Valid {
		t.Fatalf("expected Present+Valid, got %+v", f)
	}
	if math.Abs(f.Lat-37.422) > 1e-9 || math.Abs(f.Lon-(-122.084)) > 1e-9 || math.Abs(f.AltM-12.5) > 1e-9 {
		t.Fatalf("parsed wrong: %+v", f)
	}
}

func TestReadVendorFixMissing(t *testing.T) {
	if _, err := ReadVendorFix(filepath.Join(t.TempDir(), "nope.txt")); err != ErrNoFile {
		t.Fatalf("want ErrNoFile, got %v", err)
	}
}

func TestDistanceKnownBaselines(t *testing.T) {
	// One degree of latitude along a meridian ~= 111195 m (great circle).
	a := Fix{Lat: 0, Lon: 0, Valid: true}
	b := Fix{Lat: 1, Lon: 0, Valid: true}
	if d := Distance(a, b); math.Abs(d-111195) > 50 {
		t.Errorf("1 deg lat = %.1f m, want ~111195", d)
	}
	// One degree of longitude at the equator ~= 111195 m as well.
	c := Fix{Lat: 0, Lon: 1, Valid: true}
	if d := Distance(a, c); math.Abs(d-111195) > 50 {
		t.Errorf("1 deg lon @ eq = %.1f m, want ~111195", d)
	}
	// Short baseline: 0.008983 deg lon at equator ~= 1000 m.
	e := Fix{Lat: 0, Lon: 0.008983, Valid: true}
	if d := Distance(a, e); math.Abs(d-1000) > 2 {
		t.Errorf("short baseline = %.2f m, want ~1000", d)
	}
	// Identical points = 0.
	if d := Distance(a, a); d != 0 {
		t.Errorf("same point = %.3f, want 0", d)
	}
}

func TestDistanceInvalidIsZero(t *testing.T) {
	a := Fix{Lat: 1, Lon: 1, Valid: true}
	bad := Fix{Lat: 2, Lon: 2, Valid: false}
	if d := Distance(a, bad); d != 0 {
		t.Errorf("distance with an invalid fix must be 0, got %.1f", d)
	}
}
