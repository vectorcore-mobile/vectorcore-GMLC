package slg

import (
	"bytes"
	"context"
	"encoding/hex"
	"errors"
	"testing"

	"github.com/fiorix/go-diameter/v4/diam"
	"github.com/fiorix/go-diameter/v4/diam/avp"
	"github.com/fiorix/go-diameter/v4/diam/datatype"
	"github.com/fiorix/go-diameter/v4/diam/dict"
	"github.com/vectorcore/gmlc/internal/domain"
	"github.com/vectorcore/gmlc/internal/slh"
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
// TestPLARealWireFixtureWithEUTRANPositioningData guards a real bug found
// live: this dictionary omitted Accuracy-Fulfilment-Indicator (2513),
// Age-Of-Location-Estimate (2514), Velocity-Estimate (2515), and
// EUTRAN-Positioning-Data (2516) entirely. TestPLASuccessAndFailures and its
// neighbors above never caught this because they build the *diam.Message
// in-process via NewAVP, which always carries a correctly-typed Go value
// regardless of dictionary registration — real wire bytes decoded via
// diam.ReadMessage go through the dictionary's type lookup instead, and an
// unregistered AVP's Data fails the datatype.OctetString type assertion in
// decodePositioningPayload, which decodePositioningPayload reports as
// malformed. This PLA is the exact bytes MME sent for a real, successful
// UE-based A-GNSS fix (IMSI ...070572) — captured directly off the SLg wire
// between MME and the DRA — and it carries EUTRAN-Positioning-Data (GNSS
// UE-Based, GPS, "attempted successfully"), which is what actually tripped
// the bug: earlier positioning methods in this codebase never populated
// that AVP.
func TestPLARealWireFixtureWithEUTRANPositioningData(t *testing.T) {
	p := provider(t)
	raw, err := hex.DecodeString("010000f44080000c010000276e5d28a71f836c72000001074000004b676d6c632e6570632e6d6e633433352e6d63633331312e336770706e6574776f726b2e6f72673b736c673b636537373937653935393766613237623138666465306330000000010c4000000c000007d1000001154000000c00000001000001084000002e6d6d65322e6570632e6d6e633433352e6d63633331312e336770706e6574776f726b2e6f7267000000000128400000296570632e6d6e633433352e6d63633331312e336770706e6574776f726b2e6f7267000000000004da40000010012e657dc2a2670f000009d4c000000e000028af20030000")
	if err != nil {
		t.Fatal(err)
	}
	ans, err := diam.ReadMessage(bytes.NewReader(raw), dict.Default)
	if err != nil {
		t.Fatal(err)
	}
	out, e := p.DecodePLA(nil, ans)
	if e != nil {
		t.Fatalf("real PLA wire fixture must decode, got: %v", e)
	}
	if out.Kind != "location_estimate" || out.Position == nil {
		t.Fatalf("unexpected result: %+v", out)
	}
	if diff := out.Position.Latitude - 32.622495889663696; diff < -1e-6 || diff > 1e-6 {
		t.Fatalf("unexpected latitude: %v", out.Position.Latitude)
	}
	if diff := out.Position.Longitude - (-86.29539728164673); diff < -1e-6 || diff > 1e-6 {
		t.Fatalf("unexpected longitude: %v", out.Position.Longitude)
	}
	if len(out.EUTRANPositioningData) == 0 {
		t.Fatal("expected EUTRANPositioningData to be captured, not dropped")
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
func TestPLRPrivacyCheckAVPWiredWithVerifiedCodes(t *testing.T) {
	p := provider(t)
	r := request()
	pc := domain.PrivacyAllowedWithNotification
	r.PrivacyCheck = &pc
	m, e := p.BuildPLR(node(), r)
	if e != nil {
		t.Fatal(e)
	}
	ga := all(m, AVPPrivacyCheckNonSess, VendorID)
	if len(ga) != 1 {
		t.Fatalf("LCS-Privacy-Check-Non-Session not encoded: %+v", ga)
	}
	g, ok := ga[0].Data.(*diam.GroupedAVP)
	if !ok || len(g.AVP) != 1 {
		t.Fatalf("LCS-Privacy-Check-Non-Session should have exactly 1 required child, got %+v", ga[0])
	}
	if g.AVP[0].Code != AVPPrivacyCheck || g.AVP[0].Data.(datatype.Enumerated) != datatype.Enumerated(domain.PrivacyAllowedWithNotification) {
		t.Fatalf("LCS-Privacy-Check not encoded correctly: %+v", g.AVP[0])
	}

	// Absent entirely when PrivacyCheck is nil — the AVP-absent spec default
	// (ALLOWED_WITHOUT_NOTIFICATION) applies without us sending anything.
	m2, e := p.BuildPLR(node(), request())
	if e != nil {
		t.Fatal(e)
	}
	if len(all(m2, AVPPrivacyCheckNonSess, VendorID)) != 0 {
		t.Fatal("LCS-Privacy-Check-Non-Session should be absent when PrivacyCheck is nil")
	}
}
func TestPLRQoSAVPWiredWithVerifiedCodes(t *testing.T) {
	p := provider(t)
	r := request()
	class, respTime := QoSClassAssured, ResponseTimeDelayTolerant
	hAcc, vAcc := 100.0, 50.0
	r.QoS = &domain.QoSRequest{Class: &class, HorizontalAccuracyMeters: &hAcc, VerticalAccuracyMeters: &vAcc, VerticalRequested: true, ResponseTimeClass: &respTime}
	m, e := p.BuildPLR(node(), r)
	if e != nil {
		t.Fatal(e)
	}
	qa := all(m, AVPQoS, VendorID)
	if len(qa) != 1 {
		t.Fatalf("LCS-QoS not encoded: %+v", qa)
	}
	g, ok := qa[0].Data.(*diam.GroupedAVP)
	if !ok || len(g.AVP) != 5 {
		t.Fatalf("LCS-QoS should have exactly 5 children when every field is set, got %+v", qa[0])
	}
	byCode := map[uint32]*diam.AVP{}
	for _, c := range g.AVP {
		byCode[c.Code] = c
	}
	if v, ok := byCode[AVPQoSClass]; !ok || v.Data.(datatype.Enumerated) != datatype.Enumerated(QoSClassAssured) {
		t.Fatalf("LCS-QoS-Class not encoded: %+v", byCode[AVPQoSClass])
	}
	if v, ok := byCode[AVPHorizontalAccuracy]; !ok || v.Data.(datatype.Unsigned32) == 0 {
		t.Fatalf("Horizontal-Accuracy not encoded: %+v", byCode[AVPHorizontalAccuracy])
	}
	if v, ok := byCode[AVPVerticalAccuracy]; !ok || v.Data.(datatype.Unsigned32) == 0 {
		t.Fatalf("Vertical-Accuracy not encoded: %+v", byCode[AVPVerticalAccuracy])
	}
	if v, ok := byCode[AVPVerticalRequested]; !ok || v.Data.(datatype.Enumerated) != datatype.Enumerated(VerticalIsRequested) {
		t.Fatalf("Vertical-Requested not encoded: %+v", byCode[AVPVerticalRequested])
	}
	if v, ok := byCode[AVPResponseTime]; !ok || v.Data.(datatype.Enumerated) != datatype.Enumerated(ResponseTimeDelayTolerant) {
		t.Fatalf("Response-Time not encoded: %+v", byCode[AVPResponseTime])
	}

	// Absent entirely when QoS is nil.
	m2, e := p.BuildPLR(node(), request())
	if e != nil {
		t.Fatal(e)
	}
	if len(all(m2, AVPQoS, VendorID)) != 0 {
		t.Fatal("LCS-QoS should be absent when QoS is nil")
	}

	// A non-nil but entirely-empty QoS struct must not produce an empty
	// grouped AVP — qosGroup should treat it exactly like nil.
	r3 := request()
	r3.QoS = &domain.QoSRequest{}
	m3, e := p.BuildPLR(node(), r3)
	if e != nil {
		t.Fatal(e)
	}
	if len(all(m3, AVPQoS, VendorID)) != 0 {
		t.Fatal("LCS-QoS should be absent when every QoS field is unset")
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
func lrrRequest(t *testing.T) *diam.Message {
	t.Helper()
	m := diam.NewRequest(CommandLocationReport, ApplicationID, dict.Default)
	m.NewAVP(avp.SessionID, avp.Mbit, 0, datatype.UTF8String("mme.example;lrr;test"))
	m.NewAVP(avp.AuthSessionState, avp.Mbit, 0, datatype.Enumerated(1))
	m.NewAVP(avp.OriginHost, avp.Mbit, 0, datatype.DiameterIdentity("mme.example"))
	m.NewAVP(avp.OriginRealm, avp.Mbit, 0, datatype.DiameterIdentity("example"))
	m.NewAVP(avp.DestinationHost, avp.Mbit, 0, datatype.DiameterIdentity("gmlc.example"))
	m.NewAVP(avp.DestinationRealm, avp.Mbit, 0, datatype.DiameterIdentity("example"))
	m.NewAVP(AVPLocationEvent, avp.Mbit|avp.Vbit, VendorID, datatype.Enumerated(LocationEventEmergencyCallOrigination))
	return m
}
func TestDecodeLRRRequiresLocationEvent(t *testing.T) {
	p := provider(t)
	m := diam.NewRequest(CommandLocationReport, ApplicationID, dict.Default)
	if _, e := p.DecodeLRR(m); !errors.Is(e, ErrMalformedLRR) {
		t.Fatal(e)
	}
}
func TestDecodeLRRRejectsAnswersAndWrongCommand(t *testing.T) {
	p := provider(t)
	m := lrrRequest(t)
	if _, e := p.DecodeLRR(m.Answer(diam.Success)); !errors.Is(e, ErrMalformedLRR) {
		t.Fatal("expected an answer to be rejected", e)
	}
	if _, e := p.DecodeLRR(nil); !errors.Is(e, ErrMalformedLRR) {
		t.Fatal("expected nil to be rejected", e)
	}
	other := diam.NewRequest(CommandProvideLocation, ApplicationID, dict.Default)
	other.NewAVP(AVPLocationEvent, avp.Mbit|avp.Vbit, VendorID, datatype.Enumerated(LocationEventEmergencyCallOrigination))
	if _, e := p.DecodeLRR(other); !errors.Is(e, ErrMalformedLRR) {
		t.Fatal("expected wrong command code to be rejected", e)
	}
}
func TestDecodeLRRDecodesEventIdentityAndReferenceNumber(t *testing.T) {
	p := provider(t)
	m := lrrRequest(t)
	m.NewAVP(avp.UserName, avp.Mbit, 0, datatype.UTF8String("001010123456789"))
	msisdn, e := slh.EncodeMSISDN("15551234567")
	if e != nil {
		t.Fatal(e)
	}
	m.NewAVP(avpMSISDN, avp.Vbit|avp.Mbit, VendorID, datatype.OctetString(msisdn))
	m.NewAVP(AVPLCSReferenceNumber, avp.Vbit|avp.Mbit, VendorID, datatype.OctetString([]byte{7}))
	m.NewAVP(AVPClientName, avp.Vbit|avp.Mbit, VendorID, &diam.GroupedAVP{AVP: []*diam.AVP{
		diam.NewAVP(AVPLCSNameString, avp.Vbit|avp.Mbit, VendorID, datatype.UTF8String("acme-corp")),
		diam.NewAVP(AVPLCSFormatIndicator, avp.Vbit|avp.Mbit, VendorID, datatype.Enumerated(LCSFormatLogicalName)),
	}})
	raw := []byte{0, 0x40, 0, 0, 0x40, 0, 0}
	m.NewAVP(avp.LocationEstimate, avp.Mbit, 0, datatype.OctetString(raw))
	out, decErr := p.DecodeLRR(m)
	if decErr != nil {
		t.Fatal(decErr)
	}
	if out.LocationEvent != LocationEventEmergencyCallOrigination {
		t.Fatalf("LocationEvent = %v", out.LocationEvent)
	}
	if out.Target.IMSI != "001010123456789" || out.Target.MSISDN != "15551234567" {
		t.Fatalf("target identity not decoded: %+v", out.Target)
	}
	if out.ClientName != "acme-corp" {
		t.Fatalf("ClientName = %q", out.ClientName)
	}
	if out.LCSReferenceNumber == nil || *out.LCSReferenceNumber != 7 {
		t.Fatalf("LCSReferenceNumber not decoded: %+v", out.LCSReferenceNumber)
	}
	if out.Position == nil || out.Position.Latitude != 45 || out.Position.Longitude != 90 {
		t.Fatalf("position not decoded: %+v", out.Position)
	}
}
func TestDecodeLRRWithoutTargetOrReferenceNumberIsUnsolicited(t *testing.T) {
	p := provider(t)
	out, e := p.DecodeLRR(lrrRequest(t))
	if e != nil {
		t.Fatal(e)
	}
	if out.Target.IMSI != "" || out.Target.MSISDN != "" || out.LCSReferenceNumber != nil {
		t.Fatalf("expected no identity/correlation for a bare unsolicited report: %+v", out)
	}
}
func TestBuildLRAEchoesSessionAndSucceeds(t *testing.T) {
	p := provider(t)
	req := lrrRequest(t)
	a, e := p.BuildLRA(req, diam.Success)
	if e != nil {
		t.Fatal(e)
	}
	if a.Header.CommandFlags&diam.RequestFlag != 0 || a.Header.CommandCode != CommandLocationReport || a.Header.ApplicationID != ApplicationID {
		t.Fatal("bad LRA header")
	}
	sid, err := a.FindAVP(avp.SessionID, 0)
	if err != nil || sid.Data.(datatype.UTF8String) != "mme.example;lrr;test" {
		t.Fatalf("Session-Id not echoed: %v %v", sid, err)
	}
	rc, err := a.FindAVP(avp.ResultCode, 0)
	if err != nil || rc.Data.(datatype.Unsigned32) != diam.Success {
		t.Fatalf("Result-Code not set: %v %v", rc, err)
	}
}
func TestBuildLRARequiresSessionID(t *testing.T) {
	p := provider(t)
	req := diam.NewRequest(CommandLocationReport, ApplicationID, dict.Default)
	if _, e := p.BuildLRA(req, diam.Success); !errors.Is(e, ErrMalformedLRR) {
		t.Fatal(e)
	}
}
