// Package slg owns the TS 29.172 Release-16 PLR/PLA client wire boundary.
package slg

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"strings"

	"github.com/fiorix/go-diameter/v4/diam"
	"github.com/fiorix/go-diameter/v4/diam/avp"
	"github.com/fiorix/go-diameter/v4/diam/datatype"
	"github.com/fiorix/go-diameter/v4/diam/dict"
	vcdiam "github.com/vectorcore/gmlc/internal/diameter"
	"github.com/vectorcore/gmlc/internal/domain"
	"github.com/vectorcore/gmlc/internal/gad"
	"github.com/vectorcore/gmlc/internal/slh"
)

const (
	ApplicationID              uint32 = 16777255
	CommandProvideLocation     uint32 = 8388620
	VendorID                   uint32 = 10415
	AVPLocationType            uint32 = 2500
	AVPClientName              uint32 = 2501
	AVPECGI                    uint32 = 2517
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

type Config struct{ OriginHost, OriginRealm string }
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
	return &Provider{cfg: c, transport: t}, nil
}
func NewWithRegistry(c Config, r interface {
	RoundTripperFor(context.Context, vcdiam.RouteRequest) (vcdiam.Requester, error)
}) (*Provider, error) {
	if c.OriginHost == "" || c.OriginRealm == "" || r == nil {
		return nil, fmt.Errorf("slg: origin identity and registry required")
	}
	return &Provider{cfg: c, registry: r}, nil
}
func (p *Provider) ProvideLocation(ctx context.Context, node domain.ServingNode, r domain.LocationRequest) (domain.PositioningResult, error) {
	m, e := p.BuildPLR(node, r)
	if e != nil {
		return domain.PositioningResult{}, e
	}
	t := p.transport
	if p.registry != nil {
		var err error
		t, err = p.registry.RoundTripperFor(ctx, vcdiam.RouteRequest{ApplicationID: ApplicationID, DestinationHost: node.MMEHost, DestinationRealm: node.MMERealm})
		if err != nil {
			return domain.PositioningResult{}, &Error{Kind: ErrRouteUnavailable}
		}
	}
	a, e := t.RoundTrip(ctx, m)
	if e != nil {
		return domain.PositioningResult{}, e
	}
	return p.DecodePLA(m, a)
}
func (p *Provider) BuildPLR(n domain.ServingNode, r domain.LocationRequest) (*diam.Message, error) {
	if n.Type != "mme" || strings.TrimSpace(n.MMEHost) == "" || strings.TrimSpace(n.MMERealm) == "" {
		return nil, &Error{Kind: ErrRouteUnavailable}
	}
	if e := r.Target.Validate(); e != nil || r.ClientType == 0 || strings.TrimSpace(r.ClientName) == "" {
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
	m.NewAVP(AVPClientName, avp.Vbit|avp.Mbit, VendorID, &diam.GroupedAVP{})
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
	if len(out.RawLocationEstimate) > 0 {
		g, err := gad.Decode(out.RawLocationEstimate)
		if err != nil {
			return out, &Error{Kind: ErrMalformedPLA}
		}
		out.Position = &domain.GeographicPosition{Shape: "ellipsoid_point", Latitude: g.Point.LatitudeDegrees, Longitude: g.Point.LongitudeDegrees}
		out.Kind = "location_estimate"
	} else if len(out.ECGI) > 0 {
		out.Kind = "additional_information"
	} else {
		out.Kind = "no_immediate_result"
	}
	return out, nil
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
