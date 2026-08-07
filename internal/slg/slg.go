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
	VendorID               uint32 = 10415

	AVPLocationType          uint32 = 2500 // SLg-Location-Type, TS 29.172 7.4.2
	AVPClientName            uint32 = 2501 // LCS-EPS-Client-Name, 7.4.3
	AVPRequestorName         uint32 = 2502 // LCS-Requestor-Name, 7.4.4
	AVPPriority              uint32 = 2503 // LCS-Priority, 7.4.5
	AVPVelocityRequested     uint32 = 2508 // Velocity-Requested, 7.4.10
	AVPSupportedGADShapes    uint32 = 2510 // Supported-GAD-Shapes, 7.4.12
	AVPAccuracyFulfilmentInd uint32 = 2513 // Accuracy-Fulfilment-Indicator, 7.4.15
	AVPAgeOfLocationEstimate uint32 = 2514 // Age-Of-Location-Estimate, 7.4.16
	AVPVelocityEstimate      uint32 = 2515 // Velocity-Estimate, 7.4.17
	AVPEUTRANPositioningData uint32 = 2516 // EUTRAN-Positioning-Data, 7.4.18
	AVPECGI                  uint32 = 2517 // ECGI, 7.4.19
	AVPServiceTypeID         uint32 = 2520 // LCS-Service-Type-ID, 7.4.22

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

	LocationCurrent            uint32 = 0
	LocationCurrentOrLastKnown uint32 = 1
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
		m.NewAVP(701, avp.Vbit|avp.Mbit, VendorID, datatype.OctetString(b))
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
	for _, a := range all(ans, avp.LocationEstimate, 0) {
		v, ok := a.Data.(datatype.OctetString)
		if !ok || len(v) > maxRaw {
			return out, &Error{Kind: ErrMalformedPLA}
		}
		if out.RawLocationEstimate != nil {
			return out, &Error{Kind: ErrContradictoryPLA}
		}
		out.RawLocationEstimate = append([]byte(nil), v...)
	}
	for _, a := range all(ans, AVPECGI, VendorID) {
		v, ok := a.Data.(datatype.OctetString)
		if !ok || len(v) != 7 {
			return out, &Error{Kind: ErrMalformedPLA}
		}
		out.ECGI = append([]byte(nil), v...)
	}
	for _, a := range all(ans, AVPVelocityEstimate, VendorID) {
		v, ok := a.Data.(datatype.OctetString)
		if !ok {
			return out, &Error{Kind: ErrMalformedPLA}
		}
		out.RawVelocityEstimate = append([]byte(nil), v...)
	}
	for _, a := range all(ans, AVPEUTRANPositioningData, VendorID) {
		v, ok := a.Data.(datatype.OctetString)
		if !ok {
			return out, &Error{Kind: ErrMalformedPLA}
		}
		out.EUTRANPositioningData = append([]byte(nil), v...)
	}
	for _, a := range all(ans, AVPAccuracyFulfilmentInd, VendorID) {
		v, ok := a.Data.(datatype.Enumerated)
		if !ok {
			return out, &Error{Kind: ErrMalformedPLA}
		}
		u := uint32(v)
		out.AccuracyFulfilment = &u
	}
	for _, a := range all(ans, AVPAgeOfLocationEstimate, VendorID) {
		v, ok := a.Data.(datatype.Unsigned32)
		if !ok {
			return out, &Error{Kind: ErrMalformedPLA}
		}
		u := uint32(v)
		out.AgeOfLocationEstimate = &u
	}
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
