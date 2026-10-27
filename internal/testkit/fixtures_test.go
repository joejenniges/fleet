package testkit

import "testing"

func TestIsFixtureArgv(t *testing.T) {
	if !isFixtureArgv([]string{"/x/fleet", "test", "serve-port", "--port=0", FixtureFlag + "e2e-kill-1"}, "e2e-kill") {
		t.Error("fixture not matched")
	}
	// A shell that launched a fixture must never match.
	if isFixtureArgv([]string{"/bin/bash", "-c", "fleet test serve-port " + FixtureFlag + "e2e-kill-1"}, "e2e-kill") {
		t.Error("launching shell matched")
	}
	if isFixtureArgv([]string{"/x/fleet", "test", "serve-port", FixtureFlag + "other"}, "e2e-") {
		t.Error("wrong prefix matched")
	}
}
