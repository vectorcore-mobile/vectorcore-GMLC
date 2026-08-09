package slg

import (
	"github.com/fiorix/go-diameter/v4/diam"
	"github.com/fiorix/go-diameter/v4/diam/avp"
	"github.com/fiorix/go-diameter/v4/diam/datatype"
	"github.com/fiorix/go-diameter/v4/diam/dict"
	"testing"
)

func FuzzDecodePLA(f *testing.F) {
	for _, b := range [][]byte{{0, 0, 0, 0, 0, 0, 0}, nil, {0}, {1, 2, 3}} {
		f.Add(b)
	}
	p := &Provider{}
	req := diam.NewRequest(CommandProvideLocation, ApplicationID, dict.Default)
	f.Fuzz(func(t *testing.T, b []byte) {
		a := req.Answer(diam.Success)
		a.NewAVP(avp.LocationEstimate, avp.Mbit, 0, datatype.OctetString(b))
		_, _ = p.DecodePLA(req, a)
	})
}

// FuzzDecodeLRR fuzzes the Location-Estimate payload of an otherwise
// well-formed LRR (valid Location-Event, Session-Id present) — the same
// shape FuzzDecodePLA uses for PLA, since Location-Estimate goes through the
// same internal/gad decode path in both. b also stands in for
// LCS-Reference-Number (truncated/expanded — DecodeLRR requires exactly one
// octet, so most fuzzed lengths exercise the malformed-AVP rejection path).
func FuzzDecodeLRR(f *testing.F) {
	for _, b := range [][]byte{{0, 0, 0, 0, 0, 0, 0}, nil, {0}, {1, 2, 3}, {7}} {
		f.Add(b)
	}
	p := &Provider{}
	f.Fuzz(func(t *testing.T, b []byte) {
		m := diam.NewRequest(CommandLocationReport, ApplicationID, dict.Default)
		m.NewAVP(avp.SessionID, avp.Mbit, 0, datatype.UTF8String("mme.example;lrr;fuzz"))
		m.NewAVP(AVPLocationEvent, avp.Mbit|avp.Vbit, VendorID, datatype.Enumerated(LocationEventEmergencyCallOrigination))
		m.NewAVP(avp.LocationEstimate, avp.Mbit, 0, datatype.OctetString(b))
		m.NewAVP(AVPLCSReferenceNumber, avp.Vbit|avp.Mbit, VendorID, datatype.OctetString(b))
		_, _ = p.DecodeLRR(m)
	})
}
