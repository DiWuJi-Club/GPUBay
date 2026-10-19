package sshgw_test

import (
	"testing"

	"spark-cloud/internal/rental"
	"spark-cloud/internal/sshgw"
)

func TestTerminalStatesIncludeCancelled(t *testing.T) {
	want := map[string]bool{
		rental.StateReserved: false, rental.StateCreating: false, rental.StateRunning: false,
		rental.StateStopped: true, rental.StateWiping: true, rental.StateReleased: true, rental.StateFailed: true, rental.StateCancelled: true,
	}
	for state, terminal := range want {
		if got := sshgw.TerminalStateForTest(state); got != terminal {
			t.Fatalf("terminalState(%s) = %v, want %v", state, got, terminal)
		}
	}
}
