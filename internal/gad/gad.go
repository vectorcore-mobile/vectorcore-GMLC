// Package gad decodes compact TS 23.032 geographic area descriptions.
package gad

import (
	"errors"
	"math"
)

var (
	ErrEmpty         = errors.New("empty GAD")
	ErrUnsupported   = errors.New("unsupported GAD shape")
	ErrLength        = errors.New("invalid GAD length")
	ErrLatitude      = errors.New("invalid GAD latitude")
	ErrLongitude     = errors.New("invalid GAD longitude")
	ErrOrientation   = errors.New("invalid GAD orientation")
	ErrPolygonPoints = errors.New("invalid GAD polygon point count")
)

type ShapeType uint8

// Shape type values are TS 23.032 V16.1.0 Table 2a "Coding of Type of
// Shape" — the low nibble (bits 4-1) of the first octet. They are NOT the
// same as the TS 29.172 Supported-GAD-Shapes bitmask bit positions, which
// enumerate shapes sequentially (0,1,2,3,...) regardless of their wire
// value here.
const (
	ShapeEllipsoidPoint                   ShapeType = 0x0
	ShapeEllipsoidPointUncertaintyCircle  ShapeType = 0x1
	ShapeEllipsoidPointUncertaintyEllipse ShapeType = 0x3
	ShapePolygon                          ShapeType = 0x5
)

type Point struct{ LatitudeDegrees, LongitudeDegrees float64 }

// Ellipse holds TS 23.032 7.3.3 "Ellipsoid Point with uncertainty Ellipse"
// fields beyond the center point.
type Ellipse struct {
	SemiMajorMeters, SemiMinorMeters float64
	OrientationDegrees               float64
	// ConfidencePercent is the raw 7-bit field (TS 23.032 6.5): 0 means "no
	// information"; values above 100 are reserved and also mean "no
	// information" if received.
	ConfidencePercent uint8
}

type Result struct {
	Shape             ShapeType
	Point             *Point   // set for Point, Circle, and Ellipse (all have a single center point)
	UncertaintyMeters *float64 // set for Circle
	Ellipse           *Ellipse // set for Ellipse
	Polygon           []Point  // set for Polygon; Point is nil in this case
}

// Decode implements TS 23.032 V16.1.0 clause 7 for four shapes: Ellipsoid
// Point (0x0), Ellipsoid Point with Uncertainty Circle (0x1), Ellipsoid
// Point with Uncertainty Ellipse (0x3), and Polygon (0x5) — covering the
// shapes real Cell-ID/E-CID/OTDOA positioning actually returns. Other
// shapes (altitude variants, arc, high-accuracy variants) are not yet
// implemented and fail explicitly via ErrUnsupported.
//
// The shape type occupies the low nibble of the first octet (bits 4-1);
// for Point/Circle/Ellipse the high nibble is spare (ignored on decode).
// For Polygon the high nibble instead carries the point count (3-15).
//
// Latitude/longitude are a 24-bit sign+magnitude / two's-complement pair
// (resolution 90/2^23 and 360/2^24 degrees respectively) reused by every
// shape here. Circle appends one octet, Ellipse appends four, coding
// uncertainty/confidence per TS 23.032 clause 6: uncertainty (radius or
// ellipse semi-axis) r = C((1+x)^K - 1) meters with C=10, x=0.1; confidence
// is the raw percentage value.
func Decode(data []byte) (Result, error) {
	if len(data) == 0 {
		return Result{}, ErrEmpty
	}
	switch ShapeType(data[0] & 0x0f) {
	case ShapeEllipsoidPoint:
		return decodeEllipsoidPoint(data)
	case ShapeEllipsoidPointUncertaintyCircle:
		return decodeUncertaintyCircle(data)
	case ShapeEllipsoidPointUncertaintyEllipse:
		return decodeUncertaintyEllipse(data)
	case ShapePolygon:
		return decodePolygon(data)
	default:
		return Result{}, ErrUnsupported
	}
}
func decodeEllipsoidPoint(data []byte) (Result, error) {
	if len(data) != 7 {
		return Result{}, ErrLength
	}
	lat, lon, err := decodePoint(data[1:7])
	if err != nil {
		return Result{}, err
	}
	return Result{Shape: ShapeEllipsoidPoint, Point: &Point{lat, lon}}, nil
}
func decodeUncertaintyCircle(data []byte) (Result, error) {
	if len(data) != 8 {
		return Result{}, ErrLength
	}
	lat, lon, err := decodePoint(data[1:7])
	if err != nil {
		return Result{}, err
	}
	u := decodeUncertaintyValue(data[7])
	return Result{Shape: ShapeEllipsoidPointUncertaintyCircle, Point: &Point{lat, lon}, UncertaintyMeters: &u}, nil
}
func decodeUncertaintyEllipse(data []byte) (Result, error) {
	if len(data) != 11 {
		return Result{}, ErrLength
	}
	lat, lon, err := decodePoint(data[1:7])
	if err != nil {
		return Result{}, err
	}
	orientation := float64(data[9])
	if orientation >= 180 {
		return Result{}, ErrOrientation
	}
	e := &Ellipse{
		SemiMajorMeters:    decodeUncertaintyValue(data[7]),
		SemiMinorMeters:    decodeUncertaintyValue(data[8]),
		OrientationDegrees: orientation,
		ConfidencePercent:  data[10] & 0x7f,
	}
	return Result{Shape: ShapeEllipsoidPointUncertaintyEllipse, Point: &Point{lat, lon}, Ellipse: e}, nil
}
func decodePolygon(data []byte) (Result, error) {
	n := int(data[0] >> 4)
	if n < 3 || n > 15 {
		return Result{}, ErrPolygonPoints
	}
	if len(data) != 1+6*n {
		return Result{}, ErrLength
	}
	pts := make([]Point, n)
	for i := 0; i < n; i++ {
		lat, lon, err := decodePoint(data[1+6*i : 7+6*i])
		if err != nil {
			return Result{}, err
		}
		pts[i] = Point{lat, lon}
	}
	return Result{Shape: ShapePolygon, Polygon: pts}, nil
}
func decodePoint(data []byte) (lat, lon float64, err error) {
	latRaw := uint32(data[0])<<16 | uint32(data[1])<<8 | uint32(data[2])
	sign := latRaw&0x800000 != 0
	mag := latRaw & 0x7fffff
	lat = float64(mag) * 90.0 / float64(1<<23)
	if sign {
		lat = -lat
	}
	if lat < -90 || lat > 90 {
		return 0, 0, ErrLatitude
	}
	lonRaw := int32(uint32(data[3])<<16 | uint32(data[4])<<8 | uint32(data[5]))
	if lonRaw&0x800000 != 0 {
		lonRaw -= 1 << 24
	}
	lon = float64(lonRaw) * 360.0 / float64(1<<24)
	if lon < -180 || lon > 180 {
		return 0, 0, ErrLongitude
	}
	return lat, lon, nil
}
func decodeUncertaintyValue(octet byte) float64 {
	k := float64(octet & 0x7f)
	return 10.0 * (math.Pow(1.1, k) - 1.0)
}

// EncodeUncertaintyValue is the inverse of the TS 23.032 uncertainty-code
// formula decodeUncertaintyValue implements (meters = 10*(1.1^k - 1)),
// solved for k. It's exported for internal/slg's SLg-QoS Horizontal-Accuracy/
// Vertical-Accuracy AVPs (TS 29.172 7.4.7/7.4.8), which reuse the same TS
// 23.032 "Uncertainty Code" encoding rather than defining their own. The
// log-scale code can't represent every value exactly, so k is floored
// (never rounded to nearest): the requested accuracy is a requirement, not a
// measurement, and this guarantees the encoded AVP never asks the network
// for a *looser* bound than the caller specified — only ever an equal or
// tighter one. Negative input is treated as zero uncertainty; the result is
// clamped to the 7-bit range (0-127) the wire format allows.
func EncodeUncertaintyValue(meters float64) byte {
	if meters <= 0 {
		return 0
	}
	k := math.Log(meters/10.0+1.0) / math.Log(1.1)
	k = math.Floor(k)
	if k < 0 {
		k = 0
	}
	if k > 127 {
		k = 127
	}
	return byte(k)
}
