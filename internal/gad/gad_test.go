package gad

import (
	"errors"
	"math"
	"testing"
)

func TestEllipsoidPoint(t *testing.T) {
	for _, tt := range []struct {
		b        []byte
		lat, lon float64
	}{{[]byte{0, 0, 0, 0, 0, 0, 0}, 0, 0}, {[]byte{0, 0x40, 0, 0, 0x40, 0, 0}, 45, 90}, {[]byte{0, 0xc0, 0, 0, 0xc0, 0, 0}, -45, -90}, {[]byte{0, 0x7f, 0xff, 0xff, 0x7f, 0xff, 0xff}, 90 - float64(90)/float64(1<<23), 180 - float64(360)/float64(1<<24)}} {
		r, e := Decode(tt.b)
		if e != nil || math.Abs(r.Point.LatitudeDegrees-tt.lat) > 1e-9 || math.Abs(r.Point.LongitudeDegrees-tt.lon) > 1e-9 {
			t.Fatalf("%x: %+v %v", tt.b, r, e)
		}
	}
}
func TestEncodeUncertaintyValueRoundTrip(t *testing.T) {
	for _, meters := range []float64{0, 1, 10, 50, 100, 500, 1000, 5000} {
		octet := EncodeUncertaintyValue(meters)
		got := decodeUncertaintyValue(octet)
		// The wire format is a lossy 7-bit log-scale code: an accuracy
		// request is a requirement, not a measurement, so decoding what we
		// just encoded must never exceed (loosen) the requested accuracy —
		// only ever round down to an equal-or-tighter representable step.
		if got > meters+1e-9 {
			t.Fatalf("meters=%v encoded octet=%d decoded=%v: overshoot (looser than requested)", meters, octet, got)
		}
	}
	if got := EncodeUncertaintyValue(-5); got != 0 {
		t.Fatalf("negative meters: expected 0, got %d", got)
	}
	if got := EncodeUncertaintyValue(1e9); got != 127 {
		t.Fatalf("huge meters: expected clamp to 127, got %d", got)
	}
}
func TestMalformed(t *testing.T) {
	for _, tt := range []struct {
		b []byte
		e error
	}{{nil, ErrEmpty}, {[]byte{4}, ErrUnsupported}, {[]byte{0}, ErrLength}, {make([]byte, 8), ErrLength}, {[]byte{1, 0, 0, 0, 0, 0, 0}, ErrLength}} {
		_, e := Decode(tt.b)
		if !errors.Is(e, tt.e) {
			t.Fatalf("%x %v", tt.b, e)
		}
	}
}
func TestEllipsoidPointUncertaintyCircle(t *testing.T) {
	for _, tt := range []struct {
		b                     []byte
		lat, lon, uncertainty float64
	}{
		{[]byte{1, 0, 0, 0, 0, 0, 0, 0}, 0, 0, 0},
		{[]byte{1, 0x40, 0, 0, 0x40, 0, 0, 1}, 45, 90, 10 * (math.Pow(1.1, 1) - 1)},
		{[]byte{1, 0, 0, 0, 0, 0, 0, 127}, 0, 0, 10 * (math.Pow(1.1, 127) - 1)},
	} {
		r, e := Decode(tt.b)
		if e != nil || r.Shape != ShapeEllipsoidPointUncertaintyCircle || r.Point == nil || r.UncertaintyMeters == nil {
			t.Fatalf("%x: %+v %v", tt.b, r, e)
		}
		if math.Abs(r.Point.LatitudeDegrees-tt.lat) > 1e-9 || math.Abs(r.Point.LongitudeDegrees-tt.lon) > 1e-9 || math.Abs(*r.UncertaintyMeters-tt.uncertainty) > 1e-6 {
			t.Fatalf("%x: %+v", tt.b, r)
		}
	}
}

// TestShapeNibbleIsLowBits verifies TS 23.032 Table 2a: the shape type is
// the low nibble of octet 1, so a nonzero spare high nibble on a
// Point/Circle/Ellipse payload must not change how it decodes.
func TestShapeNibbleIsLowBits(t *testing.T) {
	r, e := Decode([]byte{0xF0, 0, 0, 0, 0, 0, 0})
	if e != nil || r.Shape != ShapeEllipsoidPoint || r.Point == nil {
		t.Fatalf("spare high nibble should not affect shape decode: %+v %v", r, e)
	}
}
func TestEllipsoidPointUncertaintyEllipse(t *testing.T) {
	b := []byte{0x03, 0x40, 0, 0, 0x40, 0, 0, 1, 2, 90, 50}
	r, e := Decode(b)
	if e != nil || r.Shape != ShapeEllipsoidPointUncertaintyEllipse || r.Point == nil || r.Ellipse == nil {
		t.Fatalf("%x: %+v %v", b, r, e)
	}
	if math.Abs(r.Point.LatitudeDegrees-45) > 1e-9 || math.Abs(r.Point.LongitudeDegrees-90) > 1e-9 {
		t.Fatalf("bad center point: %+v", r.Point)
	}
	wantMajor := 10 * (math.Pow(1.1, 1) - 1)
	wantMinor := 10 * (math.Pow(1.1, 2) - 1)
	if math.Abs(r.Ellipse.SemiMajorMeters-wantMajor) > 1e-6 || math.Abs(r.Ellipse.SemiMinorMeters-wantMinor) > 1e-6 {
		t.Fatalf("bad semi-axes: %+v", r.Ellipse)
	}
	if r.Ellipse.OrientationDegrees != 90 || r.Ellipse.ConfidencePercent != 50 {
		t.Fatalf("bad orientation/confidence: %+v", r.Ellipse)
	}
	// Orientation values of 180 and above are reserved (TS 23.032 7.3.3).
	bad := []byte{0x03, 0x40, 0, 0, 0x40, 0, 0, 1, 2, 180, 50}
	if _, e := Decode(bad); !errors.Is(e, ErrOrientation) {
		t.Fatalf("expected ErrOrientation, got %v", e)
	}
	if _, e := Decode(b[:10]); !errors.Is(e, ErrLength) {
		t.Fatalf("expected ErrLength for short ellipse payload, got %v", e)
	}
}
func TestPolygon(t *testing.T) {
	b := []byte{0x35, // 3 points, shape=polygon(5)
		0, 0, 0, 0, 0, 0, // (0,0)
		0x40, 0, 0, 0x40, 0, 0, // (45,90)
		0xc0, 0, 0, 0xc0, 0, 0, // (-45,-90)
	}
	r, e := Decode(b)
	if e != nil || r.Shape != ShapePolygon || r.Point != nil || len(r.Polygon) != 3 {
		t.Fatalf("%x: %+v %v", b, r, e)
	}
	want := []Point{{0, 0}, {45, 90}, {-45, -90}}
	for i, p := range want {
		if math.Abs(r.Polygon[i].LatitudeDegrees-p.LatitudeDegrees) > 1e-9 || math.Abs(r.Polygon[i].LongitudeDegrees-p.LongitudeDegrees) > 1e-9 {
			t.Fatalf("point %d: got %+v want %+v", i, r.Polygon[i], p)
		}
	}
	// Fewer than 3 points is invalid (TS 23.032 Annex A <Number of points>
	// only defines 0011..1111, i.e. 3-15).
	tooFew := []byte{0x25, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0}
	if _, e := Decode(tooFew); !errors.Is(e, ErrPolygonPoints) {
		t.Fatalf("expected ErrPolygonPoints, got %v", e)
	}
	// Declared point count not matching payload length.
	short := []byte{0x35, 0, 0, 0, 0, 0, 0}
	if _, e := Decode(short); !errors.Is(e, ErrLength) {
		t.Fatalf("expected ErrLength for truncated polygon, got %v", e)
	}
}
