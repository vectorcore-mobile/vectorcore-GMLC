// Package gad decodes compact TS 23.032 geographic area descriptions.
package gad

import "errors"

var (
	ErrEmpty       = errors.New("empty GAD")
	ErrUnsupported = errors.New("unsupported GAD shape")
	ErrLength      = errors.New("invalid GAD length")
	ErrLatitude    = errors.New("invalid GAD latitude")
	ErrLongitude   = errors.New("invalid GAD longitude")
)

type ShapeType uint8

const ShapeEllipsoidPoint ShapeType = 0

type Point struct{ LatitudeDegrees, LongitudeDegrees float64 }
type Result struct {
	Shape ShapeType
	Point *Point
}

// Decode implements TS 23.032 Ellipsoid Point. Its first octet is shape type;
// the following 24 bits hold latitude sign/magnitude and the final 24 bits are
// a two's-complement longitude. Latitude resolution is 90/2^23 degrees and
// longitude resolution is 360/2^24 degrees.
func Decode(data []byte) (Result, error) {
	if len(data) == 0 {
		return Result{}, ErrEmpty
	}
	if ShapeType(data[0]) != ShapeEllipsoidPoint {
		return Result{}, ErrUnsupported
	}
	if len(data) != 7 {
		return Result{}, ErrLength
	}
	latRaw := uint32(data[1])<<16 | uint32(data[2])<<8 | uint32(data[3])
	sign := latRaw&0x800000 != 0
	mag := latRaw & 0x7fffff
	if mag >= 0x800000 {
		return Result{}, ErrLatitude
	}
	lat := float64(mag) * 90.0 / float64(1<<23)
	if sign {
		lat = -lat
	}
	if lat < -90 || lat > 90 {
		return Result{}, ErrLatitude
	}
	lonRaw := int32(uint32(data[4])<<16 | uint32(data[5])<<8 | uint32(data[6]))
	if lonRaw&0x800000 != 0 {
		lonRaw -= 1 << 24
	}
	lon := float64(lonRaw) * 360.0 / float64(1<<24)
	if lon < -180 || lon > 180 {
		return Result{}, ErrLongitude
	}
	return Result{Shape: ShapeEllipsoidPoint, Point: &Point{lat, lon}}, nil
}
