package domain

import "testing"

func TestTransitions(t *testing.T) {
	if !CanTransition(StateQueued, StateCancelled) || !CanTransition(StateLocating, StateCompleted) || CanTransition(StateCancelled, StateCompleted) {
		t.Fatal("unexpected state transition behavior")
	}
}
func TestTargetValidation(t *testing.T) {
	for _, v := range []Target{{IMSI: "00101"}, {MSISDN: "15551234567"}} {
		if v.Validate() != nil {
			t.Fatal("valid target rejected")
		}
	}
	if (Target{IMSI: "1", MSISDN: "2"}).Validate() != nil {
		t.Fatal("both identities rejected")
	}
}
