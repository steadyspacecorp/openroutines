package runner

import "testing"

// The precedence is a security claim SECURITY.md makes out loud: a deployed
// agent cannot be talked out of its sandbox by an environment variable,
// however it got set.
func TestTheProductionSandboxOutranksTheContributorOptOut(t *testing.T) {
	for _, tc := range []struct {
		name                string
		inContainer, native string
		want                Isolation
	}{
		{"a developer's machine", "", "", Containerized},
		{"a contributor opting out", "", "1", Unconfined},
		{"the production image", "1", "", Sandboxed},
		{"the production image, opt-out attempted", "1", "1", Sandboxed},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Setenv("OPENROUTINES_IN_CONTAINER", tc.inContainer)
			t.Setenv("OPENROUTINES_NATIVE", tc.native)
			if got := Confinement(); got != tc.want {
				t.Errorf("Confinement() = %v, want %v", got, tc.want)
			}
		})
	}
}
