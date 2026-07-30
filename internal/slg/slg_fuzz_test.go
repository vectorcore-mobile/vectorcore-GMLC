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
