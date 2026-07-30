package gad

import (
	"math"
	"testing"
)

func FuzzDecode(f *testing.F) {
	for _, b := range [][]byte{{0, 0, 0, 0, 0, 0, 0}, nil, {0}, {1, 0, 0, 0, 0, 0, 0}, {0, 0, 0}} {
		f.Add(b)
	}
	f.Fuzz(func(t *testing.T, b []byte) {
		before := append([]byte(nil), b...)
		r, e := Decode(b)
		if string(before) != string(b) {
			t.Fatal("input mutated")
		}
		if e == nil {
			if r.Shape != ShapeEllipsoidPoint || r.Point == nil || math.IsNaN(r.Point.LatitudeDegrees) || math.IsInf(r.Point.LatitudeDegrees, 0) || math.Abs(r.Point.LatitudeDegrees) > 90 || math.Abs(r.Point.LongitudeDegrees) > 180 {
				t.Fatalf("invalid result %+v", r)
			}
		}
	})
}
