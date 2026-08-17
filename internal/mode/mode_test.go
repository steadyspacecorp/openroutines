package mode

import "testing"

func TestCurrent(t *testing.T) {
	t.Setenv(EnvContainer, "1")
	t.Setenv(EnvNative, "")
	if got := Current(); got != DeployedContainer {
		t.Fatalf("Current() = %v, want %v", got, DeployedContainer)
	}
	t.Setenv(EnvContainer, "true")
	t.Setenv(EnvNative, "1")
	if got := Current(); got != LocalNative {
		t.Fatalf("Current() = %v, want %v", got, LocalNative)
	}
	t.Setenv(EnvContainer, "")
	t.Setenv(EnvNative, "")
	if got := Current(); got != LocalContainer {
		t.Fatalf("Current() = %v, want %v", got, LocalContainer)
	}

	t.Setenv(EnvContainer, "1")
	t.Setenv(EnvNative, "1")
	if got := Current(); got != DeployedContainer {
		t.Fatalf("Current() = %v, want %v", got, DeployedContainer)
	}
}
