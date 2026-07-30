package slh

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/fiorix/go-diameter/v4/diam"
	"github.com/fiorix/go-diameter/v4/diam/avp"
	"github.com/fiorix/go-diameter/v4/diam/datatype"
	"github.com/fiorix/go-diameter/v4/diam/dict"
	"github.com/vectorcore/gmlc/internal/domain"
)

type fakeRT struct {
	ans *diam.Message
	err error
}

func (f fakeRT) RoundTrip(context.Context, *diam.Message) (*diam.Message, error) { return f.ans, f.err }
func resolver(t *testing.T, rt fakeRT) *Resolver {
	t.Helper()
	r, e := New(Config{OriginHost: "gmlc.example", OriginRealm: "example", DestinationRealm: "hss.example", RequestTimeout: time.Second}, rt)
	if e != nil {
		t.Fatal(e)
	}
	return r
}
func TestRIRIdentitiesAndTBCD(t *testing.T) {
	r := resolver(t, fakeRT{})
	for _, tc := range []domain.Target{{IMSI: "001010123456789"}, {MSISDN: "15551234567"}, {IMSI: "001010123456789", MSISDN: "12345"}} {
		m, e := r.BuildRIR(tc)
		if e != nil {
			t.Fatal(e)
		}
		if m.Header.ApplicationID != ApplicationID || m.Header.CommandCode != CommandRoutingInfo {
			t.Fatal("bad RIR header")
		}
		if _, e = m.FindAVP(avp.SessionID, 0); e != nil {
			t.Fatal("session absent")
		}
	}
	b, e := EncodeMSISDN("12345")
	if e != nil || len(b) != 3 || b[2] != 0xf5 {
		t.Fatalf("odd TBCD: %x %v", b, e)
	}
	if _, e = r.BuildRIR(domain.Target{IMSI: "x"}); e == nil {
		t.Fatal("invalid IMSI accepted")
	}
}
func TestRIAResultAndServingNode(t *testing.T) {
	r := resolver(t, fakeRT{})
	req, _ := r.BuildRIR(domain.Target{IMSI: "001010123456789"})
	ans := req.Answer(diam.Success)
	ans.NewAVP(avp.ServingNode, avp.Mbit|avp.Vbit, Vendor3GPP, &diam.GroupedAVP{AVP: []*diam.AVP{diam.NewAVP(avpMMEName, avp.Mbit|avp.Vbit, Vendor3GPP, datatype.DiameterIdentity("mme.example")), diam.NewAVP(avpMMERealm, avp.Vbit, Vendor3GPP, datatype.DiameterIdentity("mme.realm"))}})
	n, e := r.DecodeRIA(req, ans)
	if e != nil || n.MMEHost != "mme.example" || n.MMERealm != "mme.realm" {
		t.Fatalf("node=%+v err=%v", n, e)
	}
	bad := req.Answer(0)
	bad.NewAVP(avp.ExperimentalResult, avp.Mbit, 0, &diam.GroupedAVP{AVP: []*diam.AVP{diam.NewAVP(avp.ExperimentalResultCode, avp.Mbit, 0, datatype.Unsigned32(5001))}})
	_, e = r.DecodeRIA(req, bad)
	if !errors.Is(e, ErrUnknownSubscriber) {
		t.Fatalf("expected unknown, %v", e)
	}
}
func TestResolverCancellationAndMalformed(t *testing.T) {
	r := resolver(t, fakeRT{err: context.Canceled})
	_, e := r.ResolveServingNode(context.Background(), domain.Target{IMSI: "001010123456789"})
	if !errors.Is(e, ErrCancelled) {
		t.Fatal(e)
	}
	m := diam.NewRequest(CommandRoutingInfo, ApplicationID, dict.Default)
	_, e = r.DecodeRIA(m, nil)
	if !errors.Is(e, ErrMalformed) {
		t.Fatal(e)
	}
}
