// Package slh implements the GMLC side of TS 29.173 RIR/RIA. It is deliberately
// isolated from service/domain orchestration and from persistent transport state.
package slh

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
)

const (
	ApplicationID      uint32 = 16777291
	CommandRoutingInfo uint32 = 8388622
	Vendor3GPP         uint32 = 10415
	avpMSISDN          uint32 = 701
	avpServingNode     uint32 = 2401
	avpMMEName         uint32 = 2402
	avpMMERealm        uint32 = 2408
)

var (
	ErrInvalidTarget     = errors.New("invalid target identity")
	ErrUnknownSubscriber = errors.New("unknown subscriber")
	ErrNoServingNode     = errors.New("no serving MME")
	ErrUnauthorized      = errors.New("unauthorized request")
	ErrMalformed         = errors.New("malformed HSS response")
	ErrContradictory     = errors.New("contradictory routing information")
	ErrUnsupportedNode   = errors.New("unsupported serving-node type")
	ErrTimeout           = errors.New("SLh request timeout")
	ErrCancelled         = errors.New("SLh request cancelled")
)

type Error struct {
	Kind                         error
	ResultCode, ExperimentalCode uint32
}

func (e *Error) Error() string { return e.Kind.Error() }
func (e *Error) Unwrap() error { return e.Kind }

type Config struct {
	OriginHost, OriginRealm, DestinationRealm, DestinationHost string
	RequestTimeout                                             time.Duration
	Source                                                     string
}
type Resolver struct {
	cfg       Config
	transport interface {
		RoundTrip(context.Context, *diam.Message) (*diam.Message, error)
	}
	registry interface {
		RoundTripperFor(context.Context, vcdiam.RouteRequest) (vcdiam.Requester, error)
	}
}

func NewWithRegistry(cfg Config, r interface {
	RoundTripperFor(context.Context, vcdiam.RouteRequest) (vcdiam.Requester, error)
}) (*Resolver, error) {
	if r == nil {
		return nil, fmt.Errorf("slh: registry required")
	}
	x, err := New(cfg, nil)
	if err != nil {
		return nil, err
	}
	x.registry = r
	return x, nil
}

func New(cfg Config, t interface {
	RoundTrip(context.Context, *diam.Message) (*diam.Message, error)
}) (*Resolver, error) {
	for _, v := range []string{cfg.OriginHost, cfg.OriginRealm, cfg.DestinationRealm} {
		if strings.TrimSpace(v) == "" {
			return nil, fmt.Errorf("slh: required routing identity missing")
		}
	}
	if cfg.RequestTimeout <= 0 {
		cfg.RequestTimeout = 5 * time.Second
	}
	if cfg.Source == "" {
		cfg.Source = "slh"
	}
	return &Resolver{cfg: cfg, transport: t}, nil
}
func (r *Resolver) ResolveServingNode(ctx context.Context, target domain.Target) (domain.ServingNode, error) {
	m, err := r.BuildRIR(target)
	if err != nil {
		return domain.ServingNode{}, err
	}
	c, cancel := context.WithTimeout(ctx, r.cfg.RequestTimeout)
	defer cancel()
	t := r.transport
	if r.registry != nil {
		t, err = r.registry.RoundTripperFor(c, vcdiam.RouteRequest{ApplicationID: ApplicationID, DestinationHost: r.cfg.DestinationHost, DestinationRealm: r.cfg.DestinationRealm})
		if err != nil {
			return domain.ServingNode{}, err
		}
	}
	ans, err := t.RoundTrip(c, m)
	if err != nil {
		if errors.Is(c.Err(), context.DeadlineExceeded) || errors.Is(err, context.DeadlineExceeded) {
			return domain.ServingNode{}, &Error{Kind: ErrTimeout}
		}
		if errors.Is(c.Err(), context.Canceled) || errors.Is(err, context.Canceled) {
			return domain.ServingNode{}, &Error{Kind: ErrCancelled}
		}
		if errors.Is(err, vcdiam.ErrUnavailable) || errors.Is(err, vcdiam.ErrShutdown) {
			return domain.ServingNode{}, err
		}
		return domain.ServingNode{}, err
	}
	return r.DecodeRIA(m, ans)
}
func (r *Resolver) BuildRIR(t domain.Target) (*diam.Message, error) {
	if err := validateTarget(t); err != nil {
		return nil, err
	}
	m := diam.NewRequest(CommandRoutingInfo, ApplicationID, dict.Default)
	m.NewAVP(avp.SessionID, avp.Mbit, 0, datatype.UTF8String(sessionID(r.cfg.OriginHost)))
	m.NewAVP(avp.AuthSessionState, avp.Mbit, 0, datatype.Enumerated(1))
	m.NewAVP(avp.OriginHost, avp.Mbit, 0, datatype.DiameterIdentity(r.cfg.OriginHost))
	m.NewAVP(avp.OriginRealm, avp.Mbit, 0, datatype.DiameterIdentity(r.cfg.OriginRealm))
	m.NewAVP(avp.DestinationRealm, avp.Mbit, 0, datatype.DiameterIdentity(r.cfg.DestinationRealm))
	if r.cfg.DestinationHost != "" {
		m.NewAVP(avp.DestinationHost, avp.Mbit, 0, datatype.DiameterIdentity(r.cfg.DestinationHost))
	}
	if t.IMSI != "" {
		m.NewAVP(avp.UserName, avp.Mbit, 0, datatype.UTF8String(t.IMSI))
	}
	if t.MSISDN != "" {
		b, _ := EncodeMSISDN(t.MSISDN)
		m.NewAVP(avpMSISDN, avp.Vbit|avp.Mbit, Vendor3GPP, datatype.OctetString(b))
	}
	return m, nil
}
func validateTarget(t domain.Target) error {
	if err := t.Validate(); err != nil {
		return &Error{Kind: ErrInvalidTarget}
	}
	if t.IMSI != "" && (len(t.IMSI) < 6 || len(t.IMSI) > 15) {
		return &Error{Kind: ErrInvalidTarget}
	}
	return nil
}
func sessionID(host string) string {
	b := make([]byte, 12)
	_, _ = rand.Read(b)
	return host + ";" + hex.EncodeToString(b)
}
func EncodeMSISDN(s string) ([]byte, error) {
	if len(s) == 0 || len(s) > 15 {
		return nil, ErrInvalidTarget
	}
	out := make([]byte, (len(s)+1)/2)
	for i := range out {
		lo := s[2*i]
		if lo < '0' || lo > '9' {
			return nil, ErrInvalidTarget
		}
		v := lo - '0'
		hi := byte(0x0f)
		if 2*i+1 < len(s) {
			hi = s[2*i+1]
			if hi < '0' || hi > '9' {
				return nil, ErrInvalidTarget
			}
			hi -= '0'
		}
		out[i] = v | (hi << 4)
	}
	return out, nil
}

// DecodeMSISDN reverses EncodeMSISDN's TBCD packing. 0xf in the high nibble
// of the final octet is the odd-length filler and is only valid there — a
// filler nibble anywhere else, or any non-BCD nibble, is malformed input.
func DecodeMSISDN(b []byte) (string, error) {
	var sb strings.Builder
	for i, o := range b {
		lo, hi := o&0x0f, o>>4
		if lo > 9 {
			return "", ErrInvalidTarget
		}
		sb.WriteByte('0' + lo)
		if hi == 0x0f {
			if i != len(b)-1 {
				return "", ErrInvalidTarget
			}
			break
		}
		if hi > 9 {
			return "", ErrInvalidTarget
		}
		sb.WriteByte('0' + hi)
	}
	return sb.String(), nil
}
func (r *Resolver) DecodeRIA(req, ans *diam.Message) (domain.ServingNode, error) {
	if ans == nil || ans.Header.ApplicationID != ApplicationID || ans.Header.CommandCode != CommandRoutingInfo || ans.Header.CommandFlags&diam.RequestFlag != 0 {
		return domain.ServingNode{}, &Error{Kind: ErrMalformed}
	}
	if req != nil && (ans.Header.HopByHopID != req.Header.HopByHopID || ans.Header.EndToEndID != req.Header.EndToEndID) {
		return domain.ServingNode{}, &Error{Kind: ErrMalformed}
	}
	if e := resultError(ans); e != nil {
		return domain.ServingNode{}, e
	}
	nodes := find(ans, avpServingNode, Vendor3GPP)
	if len(nodes) != 1 {
		return domain.ServingNode{}, &Error{Kind: ErrNoServingNode}
	}
	g, ok := nodes[0].Data.(*diam.GroupedAVP)
	if !ok {
		return domain.ServingNode{}, &Error{Kind: ErrMalformed}
	}
	host := findGrouped(g, avpMMEName, Vendor3GPP)
	realm := findGrouped(g, avpMMERealm, Vendor3GPP)
	if len(host) == 0 {
		return domain.ServingNode{}, &Error{Kind: ErrUnsupportedNode}
	}
	if len(host) != 1 || len(realm) > 1 {
		return domain.ServingNode{}, &Error{Kind: ErrContradictory}
	}
	h, ok := host[0].Data.(datatype.DiameterIdentity)
	if !ok || strings.TrimSpace(string(h)) == "" {
		return domain.ServingNode{}, &Error{Kind: ErrMalformed}
	}
	rr := ""
	if len(realm) == 1 {
		v, ok := realm[0].Data.(datatype.DiameterIdentity)
		if !ok || strings.TrimSpace(string(v)) == "" {
			return domain.ServingNode{}, &Error{Kind: ErrMalformed}
		}
		rr = string(v)
	}
	return domain.ServingNode{Type: "mme", MMEHost: string(h), MMERealm: rr, Source: r.cfg.Source, ResolvedAt: time.Now().UTC()}, nil
}
func find(m *diam.Message, code, vendor uint32) []*diam.AVP {
	var out []*diam.AVP
	for _, a := range m.AVP {
		if a.Code == code && a.VendorID == vendor {
			out = append(out, a)
		}
	}
	return out
}
func findGrouped(g *diam.GroupedAVP, code, vendor uint32) []*diam.AVP {
	var out []*diam.AVP
	for _, a := range g.AVP {
		if a.Code == code && a.VendorID == vendor {
			out = append(out, a)
		}
	}
	return out
}
func resultError(m *diam.Message) error {
	base := find(m, avp.ResultCode, 0)
	exp := find(m, avp.ExperimentalResult, 0)
	if len(base) > 1 || len(exp) > 1 || (len(base) > 0 && len(exp) > 0) {
		return &Error{Kind: ErrMalformed}
	}
	if len(base) == 1 {
		v, ok := base[0].Data.(datatype.Unsigned32)
		if !ok {
			return &Error{Kind: ErrMalformed}
		}
		if uint32(v) == diam.Success {
			return nil
		}
		return &Error{Kind: errors.New("Diameter base failure"), ResultCode: uint32(v)}
	}
	if len(exp) == 1 {
		g, ok := exp[0].Data.(*diam.GroupedAVP)
		if !ok {
			return &Error{Kind: ErrMalformed}
		}
		codes := findGrouped(g, avp.ExperimentalResultCode, 0)
		if len(codes) != 1 {
			return &Error{Kind: ErrMalformed}
		}
		v, ok := codes[0].Data.(datatype.Unsigned32)
		if !ok {
			return &Error{Kind: ErrMalformed}
		}
		switch uint32(v) {
		case 5001:
			return &Error{Kind: ErrUnknownSubscriber, ExperimentalCode: uint32(v)}
		case 4201:
			return &Error{Kind: ErrNoServingNode, ExperimentalCode: uint32(v)}
		case 5490:
			return &Error{Kind: ErrUnauthorized, ExperimentalCode: uint32(v)}
		default:
			return &Error{Kind: errors.New("3GPP experimental failure"), ExperimentalCode: uint32(v)}
		}
	}
	return &Error{Kind: ErrMalformed}
}
