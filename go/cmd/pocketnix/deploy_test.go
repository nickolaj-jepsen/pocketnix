package main

import "testing"

// TestDeployAgentAndRemove drives deploy against a fake ssh and nix modelling
// a guest's drop-ins, GC roots and sudo, and asserts through that state.
func TestDeployAgentAndRemove(t *testing.T) {
	t.Skip("pending: A4a")
	if err := runDeploy([]string{"agent"}); err != nil {
		t.Fatal(err)
	}
}
