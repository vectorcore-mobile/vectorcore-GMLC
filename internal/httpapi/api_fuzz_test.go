package httpapi

import (
	"bytes"
	"encoding/json"
	"testing"
)

func FuzzLocationRequestJSON(f *testing.F) {
	for _, b := range [][]byte{[]byte(`{"target":{"imsi":"001010123456789"}}`), nil, []byte(`{`), []byte(`{"target":1}`)} {
		f.Add(b)
	}
	f.Fuzz(func(t *testing.T, b []byte) {
		var in submitJSON
		d := json.NewDecoder(bytes.NewReader(b))
		d.DisallowUnknownFields()
		_ = d.Decode(&in)
	})
}
