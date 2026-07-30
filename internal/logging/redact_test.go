package logging

import "testing"

func TestIdentifierRedaction(t *testing.T) {
	if got := Identifier("001010123456789"); got != "001…89" {
		t.Fatalf("unexpected redaction %q", got)
	}
}
