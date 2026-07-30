package domain

import "time"

type ServingNode struct {
	Type       string
	MMEHost    string
	MMERealm   string
	Source     string
	ResolvedAt time.Time
}
