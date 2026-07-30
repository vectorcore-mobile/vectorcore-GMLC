// Package logging contains shared redaction helpers for sensitive identifiers.
package logging

import "strings"

func Identifier(v string) string {
	v = strings.TrimSpace(v)
	if len(v) < 5 {
		return "***"
	}
	return v[:3] + "…" + v[len(v)-2:]
}
