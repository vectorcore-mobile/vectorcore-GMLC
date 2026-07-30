package slg

import (
	"context"
	"errors"
	"testing"

	"github.com/fiorix/go-diameter/v4/diam"
	"github.com/fiorix/go-diameter/v4/diam/avp"
	"github.com/fiorix/go-diameter/v4/diam/datatype"
	"github.com/vectorcore/gmlc/internal/domain"
)

type fake struct{ answer *diam.Message }

func (f fake) RoundTrip(_ context.Context, m *diam.Message) (*diam.Message, error) {
	return f.answer, nil
}
func provider(t *testing.T) *Provider {
	t.Helper()
	p, e := New(Config{OriginHost: "gmlc.example", OriginRealm: "example"}, fake{})
	if e != nil {
		t.Fatal(e)
	}
	return p
}
func request() domain.LocationRequest {
	return domain.LocationRequest{Target: domain.Target{IMSI: "001010123456789"}, LocationType: LocationCurrent, ClientType: 1, ClientName: "client"}
}
func node() domain.ServingNode {
	return domain.ServingNode{Type: "mme", MMEHost: "mme.example", MMERealm: "example"}
}
func TestPLRRoutingAndIdentities(t *testing.T) {
	p := provider(t)
	m, e := p.BuildPLR(node(), request())
	if e != nil {
		t.Fatal(e)
	}
	if m.Header.ApplicationID != ApplicationID || m.Header.CommandCode != CommandProvideLocation || m.Header.CommandFlags&diam.ProxiableFlag == 0 {
		t.Fatal("bad PLR header")
	}
	if _, e = m.FindAVP(avp.DestinationHost, 0); e != nil {
		t.Fatal(e)
	}
	r := request()
	r.Target = domain.Target{IMSI: "001010123456789", MSISDN: "15551234567"}
	if _, e = p.BuildPLR(node(), r); e != nil {
		t.Fatal(e)
	}
	r.LocationType = 2
	if _, e = p.BuildPLR(node(), r); !errors.Is(e, ErrInvalidRequest) {
		t.Fatal(e)
	}
}
func TestPLASuccessAndFailures(t *testing.T) {
	p := provider(t)
	req, _ := p.BuildPLR(node(), request())
	ans := req.Answer(diam.Success)
	raw := []byte{0, 0x40, 0, 0, 0x40, 0, 0}
	ans.NewAVP(avp.LocationEstimate, avp.Mbit, 0, datatype.OctetString(raw))
	ans.NewAVP(AVPECGI, avp.Mbit|avp.Vbit, VendorID, datatype.OctetString([]byte{0, 1, 2, 3, 4, 5, 6}))
	out, e := p.DecodePLA(req, ans)
	if e != nil || out.Kind != "location_estimate" || string(out.RawLocationEstimate) != string(raw) || out.Position == nil || out.Position.Latitude != 45 || out.Position.Longitude != 90 {
		t.Fatalf("%+v %v", out, e)
	}
	bad := req.Answer(0)
	bad.NewAVP(avp.ExperimentalResult, avp.Mbit, 0, &diam.GroupedAVP{AVP: []*diam.AVP{diam.NewAVP(avp.ExperimentalResultCode, avp.Mbit, 0, datatype.Unsigned32(ExperimentalDenied))}})
	_, e = p.DecodePLA(req, bad)
	if !errors.Is(e, ErrPositioningDenied) {
		t.Fatal(e)
	}
}
func TestPLAInvalidGAD(t *testing.T) {
	p := provider(t)
	req, _ := p.BuildPLR(node(), request())
	ans := req.Answer(diam.Success)
	ans.NewAVP(avp.LocationEstimate, avp.Mbit, 0, datatype.OctetString([]byte{0}))
	if _, e := p.DecodePLA(req, ans); !errors.Is(e, ErrMalformedPLA) {
		t.Fatal(e)
	}
}
