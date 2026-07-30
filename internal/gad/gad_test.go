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
func TestMalformed(t *testing.T) {
	for _, tt := range []struct {
		b []byte
		e error
	}{{nil, ErrEmpty}, {[]byte{1}, ErrUnsupported}, {[]byte{0}, ErrLength}, {make([]byte, 8), ErrLength}} {
		_, e := Decode(tt.b)
		if !errors.Is(e, tt.e) {
			t.Fatalf("%x %v", tt.b, e)
		}
	}
}
