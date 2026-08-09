package mode

import "testing"

func TestCurrent(t *testing.T) {
	t.Setenv(EnvContainer, "1")
	t.Setenv(EnvNative, "")
	if got := Current(); !got.Container || got.Native {
		t.Fatalf("Current() = %#v", got)
	}
	t.Setenv(EnvContainer, "true")
	t.Setenv(EnvNative, "1")
	if got := Current(); got.Container || !got.Native {
		t.Fatalf("Current() = %#v", got)
	}
}
