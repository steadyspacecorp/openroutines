package runner

import (
	"github.com/steadyspacecorp/openroutines/internal/config"
	"github.com/steadyspacecorp/openroutines/internal/routine"
	"strings"
	"testing"
)

func TestAttemptEnvIncludesEffectiveRoutineURL(t *testing.T) {
	agent := &config.Agent{Timezone: "America/New_York"}
	attempt := Attempt{RunID: "run_t", Number: 1}
	r := &routine.Routine{Frontmatter: routine.Frontmatter{}}
	if got := strings.Join(attemptEnv(agent, r, attempt, &runSecrets{}), "\n"); !strings.Contains(got, "OPENROUTINES_URL=https://openroutines.dev") {
		t.Fatalf("default framework env missing URL:\n%s", got)
	}
	r.Frontmatter.URL = "https://example.com/agent"
	if got := strings.Join(attemptEnv(agent, r, attempt, &runSecrets{}), "\n"); !strings.Contains(got, "OPENROUTINES_URL=https://example.com/agent") {
		t.Fatalf("declared framework env missing URL:\n%s", got)
	}
}
