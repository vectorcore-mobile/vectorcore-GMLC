package domain

import "context"

// LCS-Client-Type values (TS 29.172 V16.1.0 clause 7.4, AVP reused from TS
// 32.299). EMERGENCY_SERVICES and LAWFUL_INTERCEPT_SERVICES carry regulatory
// weight at the MME/HSS (e.g. privacy-check bypass) and must only ever be set
// from trusted, operator-controlled configuration — never from untrusted
// caller input.
const (
	ClientTypeEmergencyServices    uint32 = 0
	ClientTypeValueAddedServices   uint32 = 1
	ClientTypePLMNOperatorServices uint32 = 2
	ClientTypeLawfulIntercept      uint32 = 3
)

// LocationRequest is deliberately protocol-neutral. Phase 3A supports only
// immediate Current and Current-or-Last-Known requests.
type LocationRequest struct {
	Target            Target
	LocationType      uint32
	ClientType        uint32
	ClientName        string
	RequestorName     string
	ServiceTypeID     *uint32
	Priority          *uint32
	VelocityRequested bool
}
type PositioningResult struct {
	Kind                         string // location_estimate, additional_information, deferred, no_immediate_result
	RawLocationEstimate          []byte
	RawVelocityEstimate          []byte
	ECGI                         []byte
	EUTRANPositioningData        []byte
	AccuracyFulfilment           *uint32
	AgeOfLocationEstimate        *uint32
	ResultCode, ExperimentalCode uint32
	Position                     *GeographicPosition
}
type GeographicPosition struct {
	Shape             string   `json:"shape"`
	Latitude          float64  `json:"latitude"`
	Longitude         float64  `json:"longitude"`
	UncertaintyMeters *float64 `json:"uncertainty_meters,omitempty"`
	// The following are set only for shape "ellipsoid_point_uncertainty_ellipse"
	// (TS 23.032 7.3.3).
	SemiMajorMeters    *float64 `json:"semi_major_meters,omitempty"`
	SemiMinorMeters    *float64 `json:"semi_minor_meters,omitempty"`
	OrientationDegrees *float64 `json:"orientation_degrees,omitempty"`
	ConfidencePercent  *uint32  `json:"confidence_percent,omitempty"`
}
type LocationProvider interface {
	ProvideLocation(context.Context, ServingNode, LocationRequest) (PositioningResult, error)
}
