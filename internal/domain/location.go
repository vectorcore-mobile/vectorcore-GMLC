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

// SLg-Location-Type values (TS 29.172 V16.1.0 clause 7.4.9). Phase 3A/API-LOCTYPE
// support only these two; deferred/periodic location types are out of scope.
const (
	LocationCurrent            uint32 = 0
	LocationCurrentOrLastKnown uint32 = 1
)

// LCS-QoS-Class values (TS 29.172 7.4.27).
const (
	QoSClassAssured    uint32 = 0
	QoSClassBestEffort uint32 = 1
)

// Response-Time values (TS 29.172 7.4.11).
const (
	ResponseTimeLowDelay      uint32 = 0
	ResponseTimeDelayTolerant uint32 = 1
)

// LCS-Privacy-Check values (TS 29.172 7.4.14). Like LCS-Client-Type, this is
// an operator-configured, per-client attribute resolved server-side — never
// settable via the REST API. It governs whether/how the target subscriber
// is notified or consulted before their location is disclosed, so letting a
// caller set it themselves would let any LCS client bypass the subscriber's
// own privacy protection.
const (
	PrivacyAllowedWithoutNotification uint32 = 0
	PrivacyAllowedWithNotification    uint32 = 1
	PrivacyAllowedIfNoResponse        uint32 = 2
	PrivacyRestrictedIfNoResponse     uint32 = 3
	PrivacyNotAllowed                 uint32 = 4
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
	QoS               *QoSRequest
	// PrivacyCheck is TS 29.172 LCS-Privacy-Check (7.4.14); nil omits the
	// LCS-Privacy-Check-Non-Session AVP, which itself defaults to
	// PrivacyAllowedWithoutNotification when absent.
	PrivacyCheck *uint32
}

// QoSRequest is the TS 29.172 LCS-QoS grouped AVP (7.4.6); every field is
// independently optional per the spec's ABNF, so a nil field simply omits
// that child AVP rather than sending a zero value. Accuracy is expressed in
// meters, not the raw TS 23.032 log-scale wire code — internal/gad owns that
// encoding, matching how it already owns decoding the same code elsewhere.
type QoSRequest struct {
	Class                    *uint32 // LCS-QoS-Class (7.4.27): QoSClassAssured/QoSClassBestEffort
	HorizontalAccuracyMeters *float64
	VerticalAccuracyMeters   *float64
	VerticalRequested        bool
	ResponseTimeClass        *uint32 // Response-Time (7.4.11): ResponseTimeLowDelay/ResponseTimeDelayTolerant
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

// Location-Event values (TS 29.172 7.4.20) — the trigger for an inbound
// Location-Report-Request (LRR). EMERGENCY_CALL_ORIGINATION/RELEASE/HANDOVER
// and MO_LR are unsolicited from this GMLC's perspective: nothing in this
// codebase requests deferred/periodic/delayed reporting (LocationType only
// supports Current/Current-or-Last-Known, and BuildPLR never sends
// PLR-Flags), so DEFERRED_MT_LR_RESPONSE/DEFERRED_MO_LR_TTTP_INITIATION/
// DELAYED_LOCATION_REPORTING can never correlate to a request this GMLC
// itself made — see internal/lrr's correlation handling.
const (
	LocationEventEmergencyCallOrigination   uint32 = 0
	LocationEventEmergencyCallRelease       uint32 = 1
	LocationEventMOLR                       uint32 = 2
	LocationEventEmergencyCallHandover      uint32 = 3
	LocationEventDeferredMTLRResponse       uint32 = 4
	LocationEventDeferredMOLRTTTPInitiation uint32 = 5
	LocationEventDelayedLocationReporting   uint32 = 6
)

// LocationReport is a decoded inbound TS 29.172 Location-Report-Request
// (LRR) — the mirror image of PositioningResult (which decodes a
// GMLC-initiated PLA), but MME/SGSN-initiated. Target/ClientName are
// best-effort identity hints the network may include, not guaranteed
// present — LRR's own ABNF marks both optional.
type LocationReport struct {
	LocationEvent uint32
	Target        Target
	ClientName    string
	// LCSReferenceNumber is TS 29.172 7.4.37 (OctetString of length 1); nil
	// if the AVP was absent, which is the normal case for every event this
	// GMLC can currently trigger (see the Location-Event doc comment above).
	LCSReferenceNumber    *byte
	RawLocationEstimate   []byte
	ECGI                  []byte
	RawVelocityEstimate   []byte
	EUTRANPositioningData []byte
	AccuracyFulfilment    *uint32
	AgeOfLocationEstimate *uint32
	Position              *GeographicPosition
}
