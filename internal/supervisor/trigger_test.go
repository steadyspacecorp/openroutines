package supervisor

import (
	"context"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/steadyspacecorp/openroutines/internal/creds"
)

// triggerServer is a mutable poll target: set() changes the served value,
// which is what a trigger observes.
type triggerServer struct {
	*httptest.Server
	mu    sync.Mutex
	value string
	polls int
}

func newTriggerServer(value string) *triggerServer {
	ts := &triggerServer{value: value}
	ts.Server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		ts.mu.Lock()
		defer ts.mu.Unlock()
		ts.polls++
		w.Write([]byte(ts.value))
	}))
	return ts
}

func (ts *triggerServer) set(v string) {
	ts.mu.Lock()
	defer ts.mu.Unlock()
	ts.value = v
}

func (ts *triggerServer) pollCount() int {
	ts.mu.Lock()
	defer ts.mu.Unlock()
	return ts.polls
}

func writeTriggerRoutine(t *testing.T, dir, url, extra string) {
	t.Helper()
	fm := "---\ntrigger:\n  poll: " + url + "\n" + extra + "---\nDo the fake thing.\n"
	if err := os.WriteFile(filepath.Join(dir, "routines", "every-minute.md"), []byte(fm), 0o644); err != nil {
		t.Fatal(err)
	}
}

func runCount(t *testing.T, dir string) int {
	t.Helper()
	return strings.Count(readFile(t, filepath.Join(dir, "memory", "ledgers", "fake.md")), "ran run_")
}

func TestTriggerBaselineThenFiresOnChange(t *testing.T) {
	srv := newTriggerServer("v1")
	defer srv.Close()
	dir := fixture(t, "ok")
	writeTriggerRoutine(t, dir, srv.URL, "  interval: 1m\n")
	s := newSupervisor(t, dir)
	ctx := context.Background()
	t0 := time.Now().Truncate(time.Minute)

	s.Tick(ctx, t0) // register only
	s.Tick(ctx, t0.Add(1*time.Minute))
	if got := runCount(t, dir); got != 0 {
		t.Fatalf("baseline observation must not fire, got %d runs", got)
	}

	// Unchanged value: quiet.
	s.Tick(ctx, t0.Add(2*time.Minute))
	if got := runCount(t, dir); got != 0 {
		t.Fatalf("unchanged value must not fire, got %d runs", got)
	}

	// Changed value: exactly one run, and the observed value is now stored.
	srv.set("v2")
	s.Tick(ctx, t0.Add(3*time.Minute))
	if got := runCount(t, dir); got != 1 {
		t.Fatalf("change should fire exactly one run, got %d", got)
	}
	s.Tick(ctx, t0.Add(4*time.Minute))
	if got := runCount(t, dir); got != 1 {
		t.Fatalf("already-handled change must not re-fire, got %d runs", got)
	}
}

// One interval governs polls and fires alike: a change that lands between
// polls is caught on the next one -- delayed by at most the interval, never
// lost -- and consecutive changes produce a steady one-run-per-interval beat.
func TestTriggerIntervalPacesFires(t *testing.T) {
	srv := newTriggerServer("v1")
	defer srv.Close()
	dir := fixture(t, "ok")
	writeTriggerRoutine(t, dir, srv.URL, "  interval: 3m\n")
	s := newSupervisor(t, dir)
	ctx := context.Background()
	t0 := time.Now().Truncate(time.Minute)

	s.Tick(ctx, t0)                    // register
	s.Tick(ctx, t0.Add(1*time.Minute)) // baseline (poll 1)
	srv.set("v2")

	// Inside the interval: no polls, no fires.
	s.Tick(ctx, t0.Add(2*time.Minute))
	s.Tick(ctx, t0.Add(3*time.Minute))
	if srv.pollCount() != 1 || runCount(t, dir) != 0 {
		t.Fatalf("interval violated: %d polls, %d runs", srv.pollCount(), runCount(t, dir))
	}

	// Interval elapsed: the change is caught and fires.
	s.Tick(ctx, t0.Add(4*time.Minute).Add(time.Second))
	if got := runCount(t, dir); got != 1 {
		t.Fatalf("expected the delayed change to fire, got %d runs", got)
	}

	// A further change waits out the next interval, then fires again.
	srv.set("v3")
	s.Tick(ctx, t0.Add(5*time.Minute))
	if got := runCount(t, dir); got != 1 {
		t.Fatalf("fire pacing violated: %d runs", got)
	}
	s.Tick(ctx, t0.Add(7*time.Minute).Add(2*time.Second))
	if got := runCount(t, dir); got != 2 {
		t.Fatalf("second change should fire after the interval, got %d runs", got)
	}
}

func TestTriggerIntervalThrottlesPolls(t *testing.T) {
	srv := newTriggerServer("v1")
	defer srv.Close()
	dir := fixture(t, "ok")
	writeTriggerRoutine(t, dir, srv.URL, "  interval: 2m\n")
	s := newSupervisor(t, dir)
	ctx := context.Background()
	t0 := time.Now().Truncate(time.Minute)

	s.Tick(ctx, t0)                    // register
	s.Tick(ctx, t0.Add(1*time.Minute)) // poll 1 (baseline)
	s.Tick(ctx, t0.Add(2*time.Minute)) // within interval: no poll
	if srv.pollCount() != 1 {
		t.Fatalf("interval violated: %d polls", srv.pollCount())
	}
	s.Tick(ctx, t0.Add(3*time.Minute).Add(time.Second)) // interval elapsed: poll 2
	if srv.pollCount() != 2 {
		t.Fatalf("expected second poll after interval: %d", srv.pollCount())
	}
}

func TestTriggerPollErrorsDoNotFire(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
	}))
	defer srv.Close()
	dir := fixture(t, "ok")
	writeTriggerRoutine(t, dir, srv.URL, "")
	s := newSupervisor(t, dir)
	ctx := context.Background()
	t0 := time.Now().Truncate(time.Minute)

	s.Tick(ctx, t0)
	s.Tick(ctx, t0.Add(1*time.Minute))
	s.Tick(ctx, t0.Add(2*time.Minute))
	if got := runCount(t, dir); got != 0 {
		t.Fatalf("erroring endpoint must never fire, got %d runs", got)
	}
}

// A routine with both a schedule and a trigger: when the cron firing mints
// the run, the trigger baseline refreshes so the same news does not fire a
// second, redundant run right after.
func TestScheduledRunRefreshesTriggerBaseline(t *testing.T) {
	srv := newTriggerServer("v1")
	defer srv.Close()
	dir := fixture(t, "ok")
	fm := "---\nschedule: \"*/5 * * * *\"\ntrigger:\n  poll: " + srv.URL + "\n  interval: 1m\n---\nDo the fake thing.\n"
	os.WriteFile(filepath.Join(dir, "routines", "every-minute.md"), []byte(fm), 0o644)
	s := newSupervisor(t, dir)
	ctx := context.Background()
	t0 := time.Now().Truncate(10 * time.Minute) // align to a */5 boundary

	s.Tick(ctx, t0.Add(30*time.Second)) // register between firings
	s.Tick(ctx, t0.Add(1*time.Minute))  // trigger baseline (no cron due)
	srv.set("v2")                       // news arrives...
	s.Tick(ctx, t0.Add(5*time.Minute))  // ...cron firing runs, baseline refreshes to v2
	if got := runCount(t, dir); got != 1 {
		t.Fatalf("expected the scheduled run, got %d", got)
	}
	s.Tick(ctx, t0.Add(6*time.Minute)) // trigger sees v2 == stored v2: quiet
	if got := runCount(t, dir); got != 1 {
		t.Fatalf("refreshed baseline should prevent a redundant trigger run, got %d", got)
	}
}

// A typed credential's stored value is a root secret (derivation exists so
// runs never see it); the poll refuses to send it as a bearer token, so the
// trigger never reaches the wire and never fires.
func TestTriggerRefusesTypedCredential(t *testing.T) {
	srv := newTriggerServer("v1")
	defer srv.Close()
	dir := fixture(t, "ok")
	typedYAML := agentYAML("UTC") + "credentials:\n  gh_key:\n    type: github_app\n    app_id: \"1\"\n"
	if err := os.WriteFile(filepath.Join(dir, "openroutines.yml"), []byte(typedYAML), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, creds.KeyFileName), []byte(creds.GenerateKey()), 0o600); err != nil {
		t.Fatal(err)
	}
	key, err := creds.LoadKey(dir)
	if err != nil {
		t.Fatal(err)
	}
	if err := creds.Write(dir, key, map[string]string{"gh_key": "a-root-secret"}); err != nil {
		t.Fatal(err)
	}
	writeTriggerRoutine(t, dir, srv.URL, "  credential: gh_key\n  interval: 1m\ncredentials: [gh_key]\n")
	s := newSupervisor(t, dir)
	ctx := context.Background()
	t0 := time.Now().Truncate(time.Minute)

	s.Tick(ctx, t0)
	s.Tick(ctx, t0.Add(1*time.Minute))
	s.Tick(ctx, t0.Add(2*time.Minute))
	if srv.pollCount() != 0 {
		t.Fatalf("typed credential must never reach the wire, got %d polls", srv.pollCount())
	}
	if got := runCount(t, dir); got != 0 {
		t.Fatalf("refused poll must not fire, got %d runs", got)
	}
}
