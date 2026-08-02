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
func TestPLAUncertaintyCircle(t *testing.T) {
	p := provider(t)
	req, _ := p.BuildPLR(node(), request())
	ans := req.Answer(diam.Success)
	raw := []byte{1, 0x40, 0, 0, 0x40, 0, 0, 10}
	ans.NewAVP(avp.LocationEstimate, avp.Mbit, 0, datatype.OctetString(raw))
	out, e := p.DecodePLA(req, ans)
	if e != nil || out.Position == nil || out.Position.Shape != "ellipsoid_point_uncertainty_circle" || out.Position.UncertaintyMeters == nil {
		t.Fatalf("%+v %v", out, e)
	}
}
func TestPLRClientNameIsEncoded(t *testing.T) {
	p := provider(t)
	r := request()
	r.ClientName = "acme-corp"
	r.RequestorName = "jane"
	m, e := p.BuildPLR(node(), r)
	if e != nil {
		t.Fatal(e)
	}
	clientName := all(m, AVPClientName, VendorID)
	if len(clientName) != 1 {
		t.Fatalf("expected one LCS-EPS-Client-Name AVP, got %d", len(clientName))
	}
	g, ok := clientName[0].Data.(*diam.GroupedAVP)
	if !ok || len(g.AVP) == 0 {
		t.Fatalf("LCS-EPS-Client-Name has no content: %+v", clientName[0])
	}
	found := false
	for _, c := range g.AVP {
		if c.Code == AVPLCSNameString {
			if s, ok := c.Data.(datatype.UTF8String); !ok || string(s) != "acme-corp" {
				t.Fatalf("LCS-Name-String = %v, want acme-corp", c.Data)
			}
			found = true
		}
	}
	if !found {
		t.Fatal("LCS-Name-String not present")
	}
	// TS 29.172 V16.1.0 7.4.3: LCS-EPS-Client-Name ::= <AVP header: 2501
	// 10415> [LCS-Name-String] [LCS-Format-Indicator] — exactly two children,
	// no LCS-Data-Coding-Scheme.
	if len(g.AVP) != 2 {
		t.Fatalf("LCS-EPS-Client-Name should have exactly 2 children per TS 29.172 7.4.3, got %d: %+v", len(g.AVP), g.AVP)
	}
	req := all(m, AVPRequestorName, VendorID)
	if len(req) != 1 {
		t.Fatal("LCS-Requestor-Name not present despite RequestorName being set")
	}
	rg, ok := req[0].Data.(*diam.GroupedAVP)
	if !ok || len(rg.AVP) != 2 {
		t.Fatalf("LCS-Requestor-Name should have exactly 2 children per TS 29.172 7.4.4, got %+v", req[0])
	}
}
func TestPLROptionalAVPsWiredWithVerifiedCodes(t *testing.T) {
	p := provider(t)
	r := request()
	prio, svc := uint32(0), uint32(42)
	r.Priority = &prio
	r.ServiceTypeID = &svc
	r.VelocityRequested = true
	m, e := p.BuildPLR(node(), r)
	if e != nil {
		t.Fatal(e)
	}
	pa := all(m, AVPPriority, VendorID)
	if len(pa) != 1 || pa[0].Data.(datatype.Unsigned32) != 0 {
		t.Fatalf("LCS-Priority not encoded: %+v", pa)
	}
	va := all(m, AVPVelocityRequested, VendorID)
	if len(va) != 1 || va[0].Data.(datatype.Enumerated) != datatype.Enumerated(VelocityIsRequested) {
		t.Fatalf("Velocity-Requested not encoded: %+v", va)
	}
	sa := all(m, AVPServiceTypeID, VendorID)
	if len(sa) != 1 || sa[0].Data.(datatype.Unsigned32) != 42 {
		t.Fatalf("LCS-Service-Type-ID not encoded: %+v", sa)
	}
	ga := all(m, AVPSupportedGADShapes, VendorID)
	if len(ga) != 1 || ga[0].Data.(datatype.Unsigned32) != 0b1111 {
		t.Fatalf("Supported-GAD-Shapes not encoded as bits 0-3 (point/circle/ellipse/polygon): %+v", ga)
	}
	// Absent when unset.
	m2, e := p.BuildPLR(node(), request())
	if e != nil {
		t.Fatal(e)
	}
	if len(all(m2, AVPPriority, VendorID)) != 0 || len(all(m2, AVPVelocityRequested, VendorID)) != 0 || len(all(m2, AVPServiceTypeID, VendorID)) != 0 {
		t.Fatal("optional AVPs should be absent when not set on the request")
	}
}
func TestPLADecodesExtendedMetadata(t *testing.T) {
	p := provider(t)
	req, _ := p.BuildPLR(node(), request())
	ans := req.Answer(diam.Success)
	ans.NewAVP(avp.LocationEstimate, avp.Mbit, 0, datatype.OctetString([]byte{0, 0, 0, 0, 0, 0, 0}))
	ans.NewAVP(AVPAccuracyFulfilmentInd, avp.Mbit|avp.Vbit, VendorID, datatype.Enumerated(AccuracyNotFulfilled))
	ans.NewAVP(AVPAgeOfLocationEstimate, avp.Mbit|avp.Vbit, VendorID, datatype.Unsigned32(5))
	ans.NewAVP(AVPVelocityEstimate, avp.Mbit|avp.Vbit, VendorID, datatype.OctetString([]byte{1, 2, 3, 4}))
	ans.NewAVP(AVPEUTRANPositioningData, avp.Mbit|avp.Vbit, VendorID, datatype.OctetString([]byte{5, 6, 7}))
	out, e := p.DecodePLA(req, ans)
	if e != nil {
		t.Fatal(e)
	}
	if out.AccuracyFulfilment == nil || *out.AccuracyFulfilment != AccuracyNotFulfilled {
		t.Fatalf("Accuracy-Fulfilment-Indicator not decoded: %+v", out.AccuracyFulfilment)
	}
	if out.AgeOfLocationEstimate == nil || *out.AgeOfLocationEstimate != 5 {
		t.Fatalf("Age-Of-Location-Estimate not decoded: %+v", out.AgeOfLocationEstimate)
	}
	if string(out.RawVelocityEstimate) != "\x01\x02\x03\x04" {
		t.Fatalf("Velocity-Estimate not decoded: %x", out.RawVelocityEstimate)
	}
	if string(out.EUTRANPositioningData) != "\x05\x06\x07" {
		t.Fatalf("EUTRAN-Positioning-Data not decoded: %x", out.EUTRANPositioningData)
	}
}
func TestPLADecodesUncertaintyEllipse(t *testing.T) {
	p := provider(t)
	req, _ := p.BuildPLR(node(), request())
	ans := req.Answer(diam.Success)
	raw := []byte{0x03, 0x40, 0, 0, 0x40, 0, 0, 1, 2, 90, 50}
	ans.NewAVP(avp.LocationEstimate, avp.Mbit, 0, datatype.OctetString(raw))
	out, e := p.DecodePLA(req, ans)
	if e != nil || out.Position == nil || out.Position.Shape != "ellipsoid_point_uncertainty_ellipse" {
		t.Fatalf("%+v %v", out, e)
	}
	pos := out.Position
	if pos.Latitude != 45 || pos.Longitude != 90 {
		t.Fatalf("bad center point: %+v", pos)
	}
	if pos.SemiMajorMeters == nil || pos.SemiMinorMeters == nil || pos.OrientationDegrees == nil || pos.ConfidencePercent == nil {
		t.Fatalf("ellipse fields not populated: %+v", pos)
	}
	if *pos.OrientationDegrees != 90 || *pos.ConfidencePercent != 50 {
		t.Fatalf("bad orientation/confidence: %+v", pos)
	}
}
func TestPLADecodesPolygonWithoutFabricatingAPoint(t *testing.T) {
	p := provider(t)
	req, _ := p.BuildPLR(node(), request())
	ans := req.Answer(diam.Success)
	raw := []byte{0x35,
		0, 0, 0, 0, 0, 0,
		0x40, 0, 0, 0x40, 0, 0,
		0xc0, 0, 0, 0xc0, 0, 0,
	}
	ans.NewAVP(avp.LocationEstimate, avp.Mbit, 0, datatype.OctetString(raw))
	out, e := p.DecodePLA(req, ans)
	if e != nil {
		t.Fatal(e)
	}
	// Polygon has no single center point; the raw estimate is retained but
	// Position must stay nil rather than fabricate a misleading coordinate.
	if out.Position != nil {
		t.Fatalf("expected nil Position for polygon, got %+v", out.Position)
	}
	if out.Kind != "location_estimate" || string(out.RawLocationEstimate) != string(raw) {
		t.Fatalf("polygon raw estimate not retained: %+v", out)
	}
}
