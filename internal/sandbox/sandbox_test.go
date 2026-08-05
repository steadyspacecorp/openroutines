package sandbox

import (
	"os"
	"strings"
	"testing"
)

func TestEnsureAttemptGroupsAssertsTheCapabilityEvenWithNothingMissing(t *testing.T) {
	// A stale image can deliver every membership (an initgroups-calling init)
	// while its binary lacks cap_setgid, and every later credential
	// transition would fail. The assertion must therefore call setgroups
	// unconditionally -- with zero identities requested nothing is missing,
	// so only an unconditional call can produce this refusal.
	if os.Getuid() == 0 {
		t.Skip("running as root: setgroups would succeed")
	}
	err := EnsureAttemptGroups(0)
	if err == nil || !strings.Contains(err.Error(), "cap_setgid") {
		t.Fatalf("EnsureAttemptGroups(0) = %v, want the cap_setgid contract error", err)
	}
}
