package slh

import "testing"

func FuzzEncodeMSISDN(f *testing.F) {
	for _, s := range []string{"15551234567", "", "1234567890123456", "12a", "1"} {
		f.Add(s)
	}
	f.Fuzz(func(t *testing.T, s string) {
		b, e := EncodeMSISDN(s)
		if e == nil && (len(s) == 0 || len(s) > 15 || len(b) != (len(s)+1)/2) {
			t.Fatalf("bad encoding %q %x", s, b)
		}
	})
}
