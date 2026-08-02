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
		if e != nil {
			return
		}
		validPoint := func(p *Point) {
			if p == nil || math.IsNaN(p.LatitudeDegrees) || math.IsInf(p.LatitudeDegrees, 0) || math.Abs(p.LatitudeDegrees) > 90 || math.IsNaN(p.LongitudeDegrees) || math.IsInf(p.LongitudeDegrees, 0) || math.Abs(p.LongitudeDegrees) > 180 {
				t.Fatalf("invalid point %+v", p)
			}
		}
		validUncertainty := func(u float64) {
			if u < 0 || math.IsNaN(u) || math.IsInf(u, 0) {
				t.Fatalf("invalid uncertainty %v", u)
			}
		}
		switch r.Shape {
		case ShapeEllipsoidPoint:
			validPoint(r.Point)
			if r.UncertaintyMeters != nil || r.Ellipse != nil || r.Polygon != nil {
				t.Fatalf("unexpected extra fields %+v", r)
			}
		case ShapeEllipsoidPointUncertaintyCircle:
			validPoint(r.Point)
			if r.UncertaintyMeters == nil {
				t.Fatalf("missing uncertainty %+v", r)
			}
			validUncertainty(*r.UncertaintyMeters)
		case ShapeEllipsoidPointUncertaintyEllipse:
			validPoint(r.Point)
			if r.Ellipse == nil {
				t.Fatalf("missing ellipse %+v", r)
			}
			validUncertainty(r.Ellipse.SemiMajorMeters)
			validUncertainty(r.Ellipse.SemiMinorMeters)
			if r.Ellipse.OrientationDegrees < 0 || r.Ellipse.OrientationDegrees >= 180 {
				t.Fatalf("invalid orientation %+v", r.Ellipse)
			}
			if r.Ellipse.ConfidencePercent > 127 {
				t.Fatalf("invalid confidence %+v", r.Ellipse)
			}
		case ShapePolygon:
			if r.Point != nil {
				t.Fatalf("polygon should not set Point %+v", r)
			}
			if len(r.Polygon) < 3 || len(r.Polygon) > 15 {
				t.Fatalf("invalid polygon point count %+v", r)
			}
			for i := range r.Polygon {
				validPoint(&r.Polygon[i])
			}
		default:
			t.Fatalf("unexpected shape %+v", r)
		}
	})
}
