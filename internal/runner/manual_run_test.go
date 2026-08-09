package runner

import (
	"errors"
	"strings"
	"testing"
)

func TestManualRunInContainerRequiresTheManualIdentity(t *testing.T) {
	// Outside the real image the process is not in the attempt groups and
	// has no cap_setgid to join them, so the reservation must refuse with
	// the image contract named -- the same refusal an operator sees on a
	// stale deploy image. The working path runs in bin/smoke's container
	// stage.
	t.Setenv("OPENROUTINES_IN_CONTAINER", "1")
	_, err := RunManual(t.TempDir(), "daily", ManualOptions{})
	if !errors.Is(err, ErrFatal) || !strings.Contains(err.Error(), "cap_setgid") {
		t.Fatalf("manual run error = %v, want fatal manual-identity contract error", err)
	}
}
