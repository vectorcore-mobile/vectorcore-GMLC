// Package slg owns the TS 29.172 Release-16 PLR/PLA client wire boundary.
package slg

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/fiorix/go-diameter/v4/diam"
	"github.com/fiorix/go-diameter/v4/diam/avp"
	"github.com/fiorix/go-diameter/v4/diam/datatype"
	"github.com/fiorix/go-diameter/v4/diam/dict"
	vcdiam "github.com/vectorcore/gmlc/internal/diameter"
	"github.com/vectorcore/gmlc/internal/domain"
	"github.com/vectorcore/gmlc/internal/gad"
	"github.com/vectorcore/gmlc/internal/slh"
)

// AVP codes below are verified against 3GPP TS 29.172 V16.1.0 (Release 16),
// clause 7.4 "Information Elements" (docs/specs/ts_29172_rel16.txt).
const (
	ApplicationID          uint32 = 16777255
	CommandProvideLocation uint32 = 8388620
	CommandLocationReport  uint32 = 8388621 // TS 29.172 7.3.3/7.3.4
	VendorID               uint32 = 10415
	avpMSISDN              uint32 = 701 // MSISDN, reused from TS 29.329 (see slh.avpMSISDN)

	AVPLocationType          uint32 = 2500 // SLg-Location-Type, TS 29.172 7.4.2
	AVPClientName            uint32 = 2501 // LCS-EPS-Client-Name, 7.4.3
	AVPRequestorName         uint32 = 2502 // LCS-Requestor-Name, 7.4.4
	AVPPriority              uint32 = 2503 // LCS-Priority, 7.4.5
	AVPQoS                   uint32 = 2504 // LCS-QoS, 7.4.6 (Grouped)
	AVPHorizontalAccuracy    uint32 = 2505 // Horizontal-Accuracy, 7.4.7
	AVPVerticalAccuracy      uint32 = 2506 // Vertical-Accuracy, 7.4.8
	AVPVerticalRequested     uint32 = 2507 // Vertical-Requested, 7.4.9
	AVPVelocityRequested     uint32 = 2508 // Velocity-Requested, 7.4.10
	AVPResponseTime          uint32 = 2509 // Response-Time, 7.4.11
	AVPSupportedGADShapes    uint32 = 2510 // Supported-GAD-Shapes, 7.4.12
	AVPQoSClass              uint32 = 2523 // LCS-QoS-Class, 7.4.27
	AVPAccuracyFulfilmentInd uint32 = 2513 // Accuracy-Fulfilment-Indicator, 7.4.15
	AVPAgeOfLocationEstimate uint32 = 2514 // Age-Of-Location-Estimate, 7.4.16
	AVPVelocityEstimate      uint32 = 2515 // Velocity-Estimate, 7.4.17
	AVPEUTRANPositioningData uint32 = 2516 // EUTRAN-Positioning-Data, 7.4.18
	AVPECGI                  uint32 = 2517 // ECGI, 7.4.19
	AVPServiceTypeID         uint32 = 2520 // LCS-Service-Type-ID, 7.4.22
	AVPPrivacyCheck          uint32 = 2512 // LCS-Privacy-Check, 7.4.14
	AVPPrivacyCheckNonSess   uint32 = 2521 // LCS-Privacy-Check-Non-Session, 7.4.23 (Grouped)
	AVPLocationEvent         uint32 = 2518 // Location-Event, 7.4.20
	AVPLCSReferenceNumber    uint32 = 2531 // LCS-Reference-Number, 7.4.37 (OctetString, length 1)

	// LocationEvent* alias domain's versions — same reasoning as
	// LocationCurrent above.
	LocationEventEmergencyCallOrigination   = domain.LocationEventEmergencyCallOrigination
	LocationEventEmergencyCallRelease       = domain.LocationEventEmergencyCallRelease
	LocationEventMOLR                       = domain.LocationEventMOLR
	LocationEventEmergencyCallHandover      = domain.LocationEventEmergencyCallHandover
	LocationEventDeferredMTLRResponse       = domain.LocationEventDeferredMTLRResponse
	LocationEventDeferredMOLRTTTPInitiation = domain.LocationEventDeferredMOLRTTTPInitiation
	LocationEventDelayedLocationReporting   = domain.LocationEventDelayedLocationReporting

	// AVPLCSNameString, AVPLCSFormatIndicator, and AVPLCSRequestorIDString are
	// the LCS-EPS-Client-Name/LCS-Requestor-Name child AVPs (TS 29.172 7.4.3,
	// 7.4.4), reused from TS 32.299 with the same codes/types. There is no
	// LCS-Data-Coding-Scheme child in these two groups per the Rel-16 ABNF.
	AVPLCSFormatIndicator   uint32 = 1237
	AVPLCSNameString        uint32 = 1238
	AVPLCSRequestorIDString uint32 = 1240

	LCSFormatLogicalName uint32 = 0

	// VelocityNotRequested/VelocityIsRequested are the TS 29.172 7.4.10
	// Velocity-Requested enum values.
	VelocityNotRequested uint32 = 0
	VelocityIsRequested  uint32 = 1

	// VerticalNotRequested/VerticalIsRequested are the TS 29.172 7.4.9
	// Vertical-Requested enum values. Default when the AVP is absent is
	// VerticalNotRequested.
	VerticalNotRequested uint32 = 0
	VerticalIsRequested  uint32 = 1

	// ResponseTimeLowDelay/ResponseTimeDelayTolerant and QoSClassAssured/
	// QoSClassBestEffort alias domain's versions — same reasoning as
	// LocationCurrent above: the REST layer needs these to translate its
	// QoS JSON object without importing internal/slg.
	ResponseTimeLowDelay      = domain.ResponseTimeLowDelay
	ResponseTimeDelayTolerant = domain.ResponseTimeDelayTolerant
	QoSClassAssured           = domain.QoSClassAssured
	QoSClassBestEffort        = domain.QoSClassBestEffort

	// AccuracyFulfilled/AccuracyNotFulfilled are the TS 29.172 7.4.15
	// Accuracy-Fulfilment-Indicator enum values.
	AccuracyFulfilled    uint32 = 0
	AccuracyNotFulfilled uint32 = 1

	// supportedGADShapesMask declares the TS 23.032 shapes this GMLC can
	// decode (bit N set = shape N supported, TS 29.172 7.4.12 sequential
	// numbering — NOT the same as the shapes' TS 23.032 wire values): bit 0
	// Ellipsoid Point, bit 1 Uncertainty Circle, bit 2 Uncertainty Ellipse,
	// bit 3 Polygon. Keep this in sync with internal/gad's supported shapes.
	supportedGADShapesMask uint32 = 1<<0 | 1<<1 | 1<<2 | 1<<3

	// LocationCurrent/LocationCurrentOrLastKnown alias domain.LocationCurrent/
	// domain.LocationCurrentOrLastKnown so existing internal/slg references
	// don't need to change; domain is the canonical source (REST layer needs
	// these values too, and must not import internal/slg to get them).
	LocationCurrent                   = domain.LocationCurrent
	LocationCurrentOrLastKnown        = domain.LocationCurrentOrLastKnown
	ExperimentalUserUnknown    uint32 = 5001
	ExperimentalUnreachable    uint32 = 4221
	ExperimentalDenied         uint32 = 4224
	ExperimentalFailed         uint32 = 4225
	maxRaw                            = 8192
)

var (
	ErrInvalidRequest    = errors.New("invalid SLg request")
	ErrRouteUnavailable  = errors.New("MME route unavailable")
	ErrMalformedPLA      = errors.New("malformed PLA")
	ErrContradictoryPLA  = errors.New("contradictory PLA")
	ErrPositioningDenied = errors.New("positioning denied")
	ErrPositioningFailed = errors.New("positioning failed")
	ErrUEUnreachable     = errors.New("UE unreachable")
	ErrUnknownSubscriber = errors.New("MME unknown subscriber")
	ErrMalformedLRR      = errors.New("malformed LRR")
)

type Error struct {
	Kind                         error
	ResultCode, ExperimentalCode uint32
}

func (e *Error) Error() string { return e.Kind.Error() }
func (e *Error) Unwrap() error { return e.Kind }

type Config struct {
	OriginHost, OriginRealm string
	RequestTimeout          time.Duration
}
type Provider struct {
	cfg       Config
	transport interface {
		RoundTrip(context.Context, *diam.Message) (*diam.Message, error)
	}
	registry interface {
		RoundTripperFor(context.Context, vcdiam.RouteRequest) (vcdiam.Requester, error)
	}
}

func New(c Config, t interface {
	RoundTrip(context.Context, *diam.Message) (*diam.Message, error)
}) (*Provider, error) {
	if c.OriginHost == "" || c.OriginRealm == "" {
		return nil, fmt.Errorf("slg: origin identity required")
	}
	if c.RequestTimeout <= 0 {
		c.RequestTimeout = 5 * time.Second
	}
	return &Provider{cfg: c, transport: t}, nil
}
func NewWithRegistry(c Config, r interface {
	RoundTripperFor(context.Context, vcdiam.RouteRequest) (vcdiam.Requester, error)
}) (*Provider, error) {
	if c.OriginHost == "" || c.OriginRealm == "" || r == nil {
		return nil, fmt.Errorf("slg: origin identity and registry required")
	}
	if c.RequestTimeout <= 0 {
		c.RequestTimeout = 5 * time.Second
	}
	return &Provider{cfg: c, registry: r}, nil
}
func (p *Provider) ProvideLocation(ctx context.Context, node domain.ServingNode, r domain.LocationRequest) (domain.PositioningResult, error) {
	m, e := p.BuildPLR(node, r)
	if e != nil {
		return domain.PositioningResult{}, e
	}
	c, cancel := context.WithTimeout(ctx, p.cfg.RequestTimeout)
	defer cancel()
	t := p.transport
	if p.registry != nil {
		var err error
		t, err = p.registry.RoundTripperFor(c, vcdiam.RouteRequest{ApplicationID: ApplicationID, DestinationHost: node.MMEHost, DestinationRealm: node.MMERealm})
		if err != nil {
			return domain.PositioningResult{}, &Error{Kind: ErrRouteUnavailable}
		}
	}
	a, e := t.RoundTrip(c, m)
	if e != nil {
		return domain.PositioningResult{}, e
	}
	return p.DecodePLA(m, a)
}
func (p *Provider) BuildPLR(n domain.ServingNode, r domain.LocationRequest) (*diam.Message, error) {
	if n.Type != "mme" || strings.TrimSpace(n.MMEHost) == "" || strings.TrimSpace(n.MMERealm) == "" {
		return nil, &Error{Kind: ErrRouteUnavailable}
	}
	// ClientType is not checked against a zero value here: the orchestrator
	// always sets it to a config-validated TS 29.172 LCS-Client-Type before
	// calling ProvideLocation, and 0 (ClientTypeEmergencyServices) is itself
	// a legitimate value, not a sentinel for "unset".
	if e := r.Target.Validate(); e != nil || strings.TrimSpace(r.ClientName) == "" {
		return nil, &Error{Kind: ErrInvalidRequest}
	}
	if r.LocationType != LocationCurrent && r.LocationType != LocationCurrentOrLastKnown {
		return nil, &Error{Kind: ErrInvalidRequest}
	}
	m := diam.NewRequest(CommandProvideLocation, ApplicationID, dict.Default)
	m.Header.CommandFlags |= diam.ProxiableFlag
	m.NewAVP(avp.SessionID, avp.Mbit, 0, datatype.UTF8String(session(p.cfg.OriginHost)))
	m.NewAVP(avp.AuthSessionState, avp.Mbit, 0, datatype.Enumerated(1))
	m.NewAVP(avp.OriginHost, avp.Mbit, 0, datatype.DiameterIdentity(p.cfg.OriginHost))
	m.NewAVP(avp.OriginRealm, avp.Mbit, 0, datatype.DiameterIdentity(p.cfg.OriginRealm))
	m.NewAVP(avp.DestinationHost, avp.Mbit, 0, datatype.DiameterIdentity(n.MMEHost))
	m.NewAVP(avp.DestinationRealm, avp.Mbit, 0, datatype.DiameterIdentity(n.MMERealm))
	m.NewAVP(AVPLocationType, avp.Vbit|avp.Mbit, VendorID, datatype.Enumerated(r.LocationType))
	m.NewAVP(AVPClientName, avp.Vbit|avp.Mbit, VendorID, lcsNameGroup(r.ClientName))
	if strings.TrimSpace(r.RequestorName) != "" {
		m.NewAVP(AVPRequestorName, avp.Vbit|avp.Mbit, VendorID, &diam.GroupedAVP{AVP: []*diam.AVP{
			diam.NewAVP(AVPLCSRequestorIDString, avp.Vbit|avp.Mbit, VendorID, datatype.UTF8String(r.RequestorName)),
			diam.NewAVP(AVPLCSFormatIndicator, avp.Vbit|avp.Mbit, VendorID, datatype.Enumerated(LCSFormatLogicalName)),
		}})
	}
	if r.Priority != nil {
		m.NewAVP(AVPPriority, avp.Vbit|avp.Mbit, VendorID, datatype.Unsigned32(*r.Priority))
	}
	if g := qosGroup(r.QoS); g != nil {
		m.NewAVP(AVPQoS, avp.Vbit|avp.Mbit, VendorID, g)
	}
	// LCS-Privacy-Check-Non-Session's single child is required by the ABNF
	// ({ LCS-Privacy-Check }, not optional) once the group is sent at all —
	// unlike qosGroup's independently-optional children, there's no partial
	// form here, so this is a single conditional rather than a builder
	// function. r.PrivacyCheck is only ever set by the orchestrator from a
	// config-validated value (see domain.LocationRequest's own doc comment
	// on PrivacyCheck) — never caller input — so no range check is needed
	// here, matching ClientType's own validation-free handling above.
	if r.PrivacyCheck != nil {
		m.NewAVP(AVPPrivacyCheckNonSess, avp.Vbit|avp.Mbit, VendorID, &diam.GroupedAVP{AVP: []*diam.AVP{
			diam.NewAVP(AVPPrivacyCheck, avp.Vbit|avp.Mbit, VendorID, datatype.Enumerated(*r.PrivacyCheck)),
		}})
	}
	if r.VelocityRequested {
		m.NewAVP(AVPVelocityRequested, avp.Vbit|avp.Mbit, VendorID, datatype.Enumerated(VelocityIsRequested))
	}
	if r.ServiceTypeID != nil {
		m.NewAVP(AVPServiceTypeID, avp.Vbit|avp.Mbit, VendorID, datatype.Unsigned32(*r.ServiceTypeID))
	}
	m.NewAVP(AVPSupportedGADShapes, avp.Vbit|avp.Mbit, VendorID, datatype.Unsigned32(supportedGADShapesMask))
	m.NewAVP(avp.LCSClientType, avp.Mbit, 0, datatype.Enumerated(r.ClientType))
	if r.Target.IMSI != "" {
		m.NewAVP(avp.UserName, avp.Mbit, 0, datatype.UTF8String(r.Target.IMSI))
	}
	if r.Target.MSISDN != "" {
		b, e := slh.EncodeMSISDN(r.Target.MSISDN)
		if e != nil {
			return nil, &Error{Kind: ErrInvalidRequest}
		}
		m.NewAVP(avpMSISDN, avp.Vbit|avp.Mbit, VendorID, datatype.OctetString(b))
	}
	return m, nil
}

// lcsNameGroup builds the LCS-EPS-Client-Name grouped AVP. BuildPLR already
// requires a non-empty ClientName before this is called.
func lcsNameGroup(name string) *diam.GroupedAVP {
	return &diam.GroupedAVP{AVP: []*diam.AVP{
		diam.NewAVP(AVPLCSNameString, avp.Vbit|avp.Mbit, VendorID, datatype.UTF8String(name)),
		diam.NewAVP(AVPLCSFormatIndicator, avp.Vbit|avp.Mbit, VendorID, datatype.Enumerated(LCSFormatLogicalName)),
	}}
}

// qosGroup builds the LCS-QoS grouped AVP (TS 29.172 7.4.6); every child is
// independently optional per the ABNF, so it returns nil rather than an
// empty group when q is nil or none of its fields are set — an empty
// grouped AVP would carry no information and isn't worth sending.
// Horizontal/Vertical-Accuracy use internal/gad's TS 23.032 uncertainty-code
// encoding (the same code Position decoding already uses, just inverted).
func qosGroup(q *domain.QoSRequest) *diam.GroupedAVP {
	if q == nil {
		return nil
	}
	var children []*diam.AVP
	if q.Class != nil {
		children = append(children, diam.NewAVP(AVPQoSClass, avp.Vbit|avp.Mbit, VendorID, datatype.Enumerated(*q.Class)))
	}
	if q.HorizontalAccuracyMeters != nil {
		children = append(children, diam.NewAVP(AVPHorizontalAccuracy, avp.Vbit|avp.Mbit, VendorID, datatype.Unsigned32(gad.EncodeUncertaintyValue(*q.HorizontalAccuracyMeters))))
	}
	if q.VerticalAccuracyMeters != nil {
		children = append(children, diam.NewAVP(AVPVerticalAccuracy, avp.Vbit|avp.Mbit, VendorID, datatype.Unsigned32(gad.EncodeUncertaintyValue(*q.VerticalAccuracyMeters))))
	}
	if q.VerticalRequested {
		children = append(children, diam.NewAVP(AVPVerticalRequested, avp.Vbit|avp.Mbit, VendorID, datatype.Enumerated(VerticalIsRequested)))
	}
	if q.ResponseTimeClass != nil {
		children = append(children, diam.NewAVP(AVPResponseTime, avp.Vbit|avp.Mbit, VendorID, datatype.Enumerated(*q.ResponseTimeClass)))
	}
	if len(children) == 0 {
		return nil
	}
	return &diam.GroupedAVP{AVP: children}
}
func session(h string) string {
	b := make([]byte, 12)
	_, _ = rand.Read(b)
	return h + ";slg;" + hex.EncodeToString(b)
}
func (p *Provider) DecodePLA(req, ans *diam.Message) (domain.PositioningResult, error) {
	if ans == nil || ans.Header.ApplicationID != ApplicationID || ans.Header.CommandCode != CommandProvideLocation || ans.Header.CommandFlags&diam.RequestFlag != 0 {
		return domain.PositioningResult{}, &Error{Kind: ErrMalformedPLA}
	}
	if req != nil && (req.Header.HopByHopID != ans.Header.HopByHopID || req.Header.EndToEndID != ans.Header.EndToEndID) {
		return domain.PositioningResult{}, &Error{Kind: ErrMalformedPLA}
	}
	base := all(ans, avp.ResultCode, 0)
	exp := all(ans, avp.ExperimentalResult, 0)
	if len(base) > 1 || len(exp) > 1 || len(base)+len(exp) != 1 {
		return domain.PositioningResult{}, &Error{Kind: ErrMalformedPLA}
	}
	out := domain.PositioningResult{}
	if len(base) == 1 {
		v, ok := base[0].Data.(datatype.Unsigned32)
		if !ok {
			return out, &Error{Kind: ErrMalformedPLA}
		}
		out.ResultCode = uint32(v)
		if out.ResultCode != diam.Success {
			return out, &Error{Kind: errors.New("Diameter base failure"), ResultCode: out.ResultCode}
		}
	} else {
		g, ok := exp[0].Data.(*diam.GroupedAVP)
		if !ok {
			return out, &Error{Kind: ErrMalformedPLA}
		}
		x := allG(g, avp.ExperimentalResultCode, 0)
		if len(x) != 1 {
			return out, &Error{Kind: ErrMalformedPLA}
		}
		v, ok := x[0].Data.(datatype.Unsigned32)
		if !ok {
			return out, &Error{Kind: ErrMalformedPLA}
		}
		out.ExperimentalCode = uint32(v)
		switch out.ExperimentalCode {
		case ExperimentalUserUnknown:
			return out, &Error{Kind: ErrUnknownSubscriber, ExperimentalCode: out.ExperimentalCode}
		case ExperimentalUnreachable:
			return out, &Error{Kind: ErrUEUnreachable, ExperimentalCode: out.ExperimentalCode}
		case ExperimentalDenied:
			return out, &Error{Kind: ErrPositioningDenied, ExperimentalCode: out.ExperimentalCode}
		case ExperimentalFailed:
			return out, &Error{Kind: ErrPositioningFailed, ExperimentalCode: out.ExperimentalCode}
		default:
			return out, &Error{Kind: errors.New("3GPP experimental failure"), ExperimentalCode: out.ExperimentalCode}
		}
	}
	pp, malformed, contradiction := decodePositioningPayload(ans)
	if malformed {
		return out, &Error{Kind: ErrMalformedPLA}
	}
	if contradiction {
		return out, &Error{Kind: ErrContradictoryPLA}
	}
	out.RawLocationEstimate, out.ECGI, out.RawVelocityEstimate, out.EUTRANPositioningData = pp.raw, pp.ecgi, pp.velocity, pp.eutran
	out.AccuracyFulfilment, out.AgeOfLocationEstimate = pp.accuracyFulfilment, pp.ageOfLocationEstimate
	if len(out.RawLocationEstimate) > 0 {
		g, err := gad.Decode(out.RawLocationEstimate)
		if err != nil {
			return out, &Error{Kind: ErrMalformedPLA}
		}
		// Polygon has no single center point; RawLocationEstimate is still
		// retained for diagnostics, but there is no GeographicPosition to
		// report — that is a real, honest gap, not a decode failure.
		out.Position = positionFromGAD(g)
		out.Kind = "location_estimate"
	} else if len(out.ECGI) > 0 {
		out.Kind = "additional_information"
	} else {
		out.Kind = "no_immediate_result"
	}
	return out, nil
}
func positionFromGAD(g gad.Result) *domain.GeographicPosition {
	if g.Point == nil {
		return nil
	}
	p := &domain.GeographicPosition{Shape: gadShapeName(g.Shape), Latitude: g.Point.LatitudeDegrees, Longitude: g.Point.LongitudeDegrees, UncertaintyMeters: g.UncertaintyMeters}
	if g.Ellipse != nil {
		p.SemiMajorMeters = &g.Ellipse.SemiMajorMeters
		p.SemiMinorMeters = &g.Ellipse.SemiMinorMeters
		p.OrientationDegrees = &g.Ellipse.OrientationDegrees
		c := uint32(g.Ellipse.ConfidencePercent)
		p.ConfidencePercent = &c
	}
	return p
}
func gadShapeName(s gad.ShapeType) string {
	switch s {
	case gad.ShapeEllipsoidPointUncertaintyCircle:
		return "ellipsoid_point_uncertainty_circle"
	case gad.ShapeEllipsoidPointUncertaintyEllipse:
		return "ellipsoid_point_uncertainty_ellipse"
	case gad.ShapePolygon:
		return "polygon"
	default:
		return "ellipsoid_point"
	}
}
func all(m *diam.Message, c, v uint32) []*diam.AVP {
	var o []*diam.AVP
	for _, a := range m.AVP {
		if a.Code == c && a.VendorID == v {
			o = append(o, a)
		}
	}
	return o
}
func allG(g *diam.GroupedAVP, c, v uint32) []*diam.AVP {
	var o []*diam.AVP
	for _, a := range g.AVP {
		if a.Code == c && a.VendorID == v {
			o = append(o, a)
		}
	}
	return o
}

type positioningPayload struct {
	raw, ecgi, velocity, eutran               []byte
	accuracyFulfilment, ageOfLocationEstimate *uint32
}

// decodePositioningPayload decodes the positioning-result AVPs shared by PLA
// (7.3.2) and LRR (7.3.3) — Location-Estimate, ECGI, Velocity-Estimate,
// EUTRAN-Positioning-Data, Accuracy-Fulfilment-Indicator, and
// Age-Of-Location-Estimate all carry identical codes/types in both commands.
// contradiction is set only for a duplicate Location-Estimate AVP; every
// other decode failure sets malformed. Callers translate both into their own
// command-specific Error.Kind.
func decodePositioningPayload(m *diam.Message) (pp positioningPayload, malformed, contradiction bool) {
	for _, a := range all(m, avp.LocationEstimate, 0) {
		v, ok := a.Data.(datatype.OctetString)
		if !ok || len(v) > maxRaw {
			return pp, true, false
		}
		if pp.raw != nil {
			return pp, false, true
		}
		pp.raw = append([]byte(nil), v...)
	}
	for _, a := range all(m, AVPECGI, VendorID) {
		v, ok := a.Data.(datatype.OctetString)
		if !ok || len(v) != 7 {
			return pp, true, false
		}
		pp.ecgi = append([]byte(nil), v...)
	}
	for _, a := range all(m, AVPVelocityEstimate, VendorID) {
		v, ok := a.Data.(datatype.OctetString)
		if !ok {
			return pp, true, false
		}
		pp.velocity = append([]byte(nil), v...)
	}
	for _, a := range all(m, AVPEUTRANPositioningData, VendorID) {
		v, ok := a.Data.(datatype.OctetString)
		if !ok {
			return pp, true, false
		}
		pp.eutran = append([]byte(nil), v...)
	}
	for _, a := range all(m, AVPAccuracyFulfilmentInd, VendorID) {
		v, ok := a.Data.(datatype.Enumerated)
		if !ok {
			return pp, true, false
		}
		u := uint32(v)
		pp.accuracyFulfilment = &u
	}
	for _, a := range all(m, AVPAgeOfLocationEstimate, VendorID) {
		v, ok := a.Data.(datatype.Unsigned32)
		if !ok {
			return pp, true, false
		}
		u := uint32(v)
		pp.ageOfLocationEstimate = &u
	}
	return pp, false, false
}

// DecodeLRR decodes an inbound TS 29.172 Location-Report-Request (7.3.3).
// Unlike DecodePLA (which decodes an answer this GMLC solicited), this
// decodes a request the MME/SGSN initiated — Location-Event is the only
// mandatory field; everything else, including target identity, is optional
// per the ABNF (7.3.3).
func (p *Provider) DecodeLRR(m *diam.Message) (domain.LocationReport, *Error) {
	if m == nil || m.Header.ApplicationID != ApplicationID || m.Header.CommandCode != CommandLocationReport || m.Header.CommandFlags&diam.RequestFlag == 0 {
		return domain.LocationReport{}, &Error{Kind: ErrMalformedLRR}
	}
	ev := all(m, AVPLocationEvent, VendorID)
	if len(ev) != 1 {
		return domain.LocationReport{}, &Error{Kind: ErrMalformedLRR}
	}
	v, ok := ev[0].Data.(datatype.Enumerated)
	if !ok {
		return domain.LocationReport{}, &Error{Kind: ErrMalformedLRR}
	}
	out := domain.LocationReport{LocationEvent: uint32(v)}
	for _, a := range all(m, avp.UserName, 0) {
		s, ok := a.Data.(datatype.UTF8String)
		if !ok {
			return domain.LocationReport{}, &Error{Kind: ErrMalformedLRR}
		}
		out.Target.IMSI = string(s)
	}
	for _, a := range all(m, avpMSISDN, VendorID) {
		b, ok := a.Data.(datatype.OctetString)
		if !ok {
			return domain.LocationReport{}, &Error{Kind: ErrMalformedLRR}
		}
		msisdn, e := slh.DecodeMSISDN([]byte(b))
		if e != nil {
			return domain.LocationReport{}, &Error{Kind: ErrMalformedLRR}
		}
		out.Target.MSISDN = msisdn
	}
	for _, a := range all(m, AVPClientName, VendorID) {
		g, ok := a.Data.(*diam.GroupedAVP)
		if !ok {
			return domain.LocationReport{}, &Error{Kind: ErrMalformedLRR}
		}
		if n := allG(g, AVPLCSNameString, VendorID); len(n) == 1 {
			if s, ok := n[0].Data.(datatype.UTF8String); ok {
				out.ClientName = string(s)
			}
		}
	}
	for _, a := range all(m, AVPLCSReferenceNumber, VendorID) {
		b, ok := a.Data.(datatype.OctetString)
		if !ok || len(b) != 1 {
			return domain.LocationReport{}, &Error{Kind: ErrMalformedLRR}
		}
		ref := b[0]
		out.LCSReferenceNumber = &ref
	}
	pp, malformed, contradiction := decodePositioningPayload(m)
	if malformed || contradiction {
		return domain.LocationReport{}, &Error{Kind: ErrMalformedLRR}
	}
	out.RawLocationEstimate, out.ECGI, out.RawVelocityEstimate, out.EUTRANPositioningData = pp.raw, pp.ecgi, pp.velocity, pp.eutran
	out.AccuracyFulfilment, out.AgeOfLocationEstimate = pp.accuracyFulfilment, pp.ageOfLocationEstimate
	if len(out.RawLocationEstimate) > 0 {
		g, err := gad.Decode(out.RawLocationEstimate)
		if err != nil {
			return domain.LocationReport{}, &Error{Kind: ErrMalformedLRR}
		}
		out.Position = positionFromGAD(g)
	}
	return out, nil
}

// BuildLRA builds the Location-Report-Answer (7.3.4) for an inbound LRR.
// resultCode is the caller's choice (diam.Success, or a failure code if
// decoding req itself failed enough to still identify it as an LRR but not
// enough to trust its content) — GMLC never fails an LRR for reasons within
// the target UE/report's own content (there is no requester waiting on a
// synchronous result the way PLR/RIR callers are), only for structurally
// unusable requests.
func (p *Provider) BuildLRA(req *diam.Message, resultCode uint32) (*diam.Message, *Error) {
	if req == nil || req.Header.ApplicationID != ApplicationID || req.Header.CommandCode != CommandLocationReport || req.Header.CommandFlags&diam.RequestFlag == 0 {
		return nil, &Error{Kind: ErrMalformedLRR}
	}
	sess := all(req, avp.SessionID, 0)
	if len(sess) != 1 {
		return nil, &Error{Kind: ErrMalformedLRR}
	}
	a := req.Answer(resultCode)
	a.NewAVP(avp.SessionID, avp.Mbit, 0, sess[0].Data)
	a.NewAVP(avp.AuthSessionState, avp.Mbit, 0, datatype.Enumerated(1))
	a.NewAVP(avp.OriginHost, avp.Mbit, 0, datatype.DiameterIdentity(p.cfg.OriginHost))
	a.NewAVP(avp.OriginRealm, avp.Mbit, 0, datatype.DiameterIdentity(p.cfg.OriginRealm))
	return a, nil
}
