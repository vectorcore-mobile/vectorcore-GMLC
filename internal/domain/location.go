package domain

import "context"

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
	Shape     string  `json:"shape"`
	Latitude  float64 `json:"latitude"`
	Longitude float64 `json:"longitude"`
}
type LocationProvider interface {
	ProvideLocation(context.Context, ServingNode, LocationRequest) (PositioningResult, error)
}
