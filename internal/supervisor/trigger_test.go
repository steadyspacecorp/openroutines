package supervisor

import (
	"context"
	"crypto/rand"
	"crypto/rsa"
	"crypto/x509"
	"encoding/pem"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/steadyspacecorp/openroutines/internal/creds"
	"github.com/steadyspacecorp/openroutines/internal/scrub"
)

type roundTripFunc func(*http.Request) (*http.Response, error)

func (f roundTripFunc) RoundTrip(r *http.Request) (*http.Response, error) {
	return f(r)
}

type triggerServer struct {
	*httptest.Server
	mu    sync.Mutex
	value string
	polls int
}

func newTriggerServer() *triggerServer {
	ts := &triggerServer{value: "v1"}
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
	return strings.Count(readFile(t, filepath.Join(dir, "knowledge", "ledgers", "fake.md")), "ran run_")
}

func TestTriggerBaselineThenFiresOnChange(t *testing.T) {
	srv := newTriggerServer()
	defer srv.Close()
	dir := fixture(t, "ok")
	writeTriggerRoutine(t, dir, srv.URL, "  interval: 1m\n")
	s := newSupervisor(t, dir)
	ctx := context.Background()
	t0 := time.Now().Truncate(time.Minute)

	s.tickWait(ctx, t0)
	s.tickWait(ctx, t0.Add(1*time.Minute))
	if got := runCount(t, dir); got != 0 {
		t.Fatalf("baseline observation must not fire, got %d runs", got)
	}

	s.tickWait(ctx, t0.Add(2*time.Minute))
	if got := runCount(t, dir); got != 0 {
		t.Fatalf("unchanged value must not fire, got %d runs", got)
	}

	srv.set("v2")
	s.tickWait(ctx, t0.Add(3*time.Minute))
	if got := runCount(t, dir); got != 1 {
		t.Fatalf("change should fire exactly one run, got %d", got)
	}
	s.tickWait(ctx, t0.Add(4*time.Minute))
	if got := runCount(t, dir); got != 1 {
		t.Fatalf("already-handled change must not re-fire, got %d runs", got)
	}
}

func TestTriggerIntervalPacesFires(t *testing.T) {
	srv := newTriggerServer()
	defer srv.Close()
	dir := fixture(t, "ok")
	writeTriggerRoutine(t, dir, srv.URL, "  interval: 3m\n")
	s := newSupervisor(t, dir)
	ctx := context.Background()
	t0 := time.Now().Truncate(time.Minute)

	s.tickWait(ctx, t0)
	s.tickWait(ctx, t0.Add(1*time.Minute))
	srv.set("v2")

	s.tickWait(ctx, t0.Add(2*time.Minute))
	s.tickWait(ctx, t0.Add(3*time.Minute))
	if srv.pollCount() != 1 || runCount(t, dir) != 0 {
		t.Fatalf("interval violated: %d polls, %d runs", srv.pollCount(), runCount(t, dir))
	}

	s.tickWait(ctx, t0.Add(4*time.Minute).Add(time.Second))
	if got := runCount(t, dir); got != 1 {
		t.Fatalf("expected the delayed change to fire, got %d runs", got)
	}

	srv.set("v3")
	s.tickWait(ctx, t0.Add(5*time.Minute))
	if got := runCount(t, dir); got != 1 {
		t.Fatalf("fire pacing violated: %d runs", got)
	}
	s.tickWait(ctx, t0.Add(7*time.Minute).Add(2*time.Second))
	if got := runCount(t, dir); got != 2 {
		t.Fatalf("second change should fire after the interval, got %d runs", got)
	}
}

func TestTriggerIntervalThrottlesPolls(t *testing.T) {
	srv := newTriggerServer()
	defer srv.Close()
	dir := fixture(t, "ok")
	writeTriggerRoutine(t, dir, srv.URL, "  interval: 2m\n")
	s := newSupervisor(t, dir)
	ctx := context.Background()
	t0 := time.Now().Truncate(time.Minute)

	s.tickWait(ctx, t0)
	s.tickWait(ctx, t0.Add(1*time.Minute))
	s.tickWait(ctx, t0.Add(2*time.Minute))
	if srv.pollCount() != 1 {
		t.Fatalf("interval violated: %d polls", srv.pollCount())
	}
	s.tickWait(ctx, t0.Add(3*time.Minute).Add(time.Second))
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

	s.tickWait(ctx, t0)
	s.tickWait(ctx, t0.Add(1*time.Minute))
	s.tickWait(ctx, t0.Add(2*time.Minute))
	if got := runCount(t, dir); got != 0 {
		t.Fatalf("erroring endpoint must never fire, got %d runs", got)
	}
}

func TestScheduledRunRefreshesTriggerBaseline(t *testing.T) {
	srv := newTriggerServer()
	defer srv.Close()
	dir := fixture(t, "ok")
	fm := "---\nschedule: \"*/5 * * * *\"\ntrigger:\n  poll: " + srv.URL + "\n  interval: 1m\n---\nDo the fake thing.\n"
	os.WriteFile(filepath.Join(dir, "routines", "every-minute.md"), []byte(fm), 0o644)
	s := newSupervisor(t, dir)
	ctx := context.Background()
	t0 := time.Now().Truncate(10 * time.Minute)

	s.tickWait(ctx, t0.Add(30*time.Second))
	s.tickWait(ctx, t0.Add(1*time.Minute))
	srv.set("v2")
	s.tickWait(ctx, t0.Add(5*time.Minute))
	if got := runCount(t, dir); got != 1 {
		t.Fatalf("expected the scheduled run, got %d", got)
	}
	s.tickWait(ctx, t0.Add(6*time.Minute))
	if got := runCount(t, dir); got != 1 {
		t.Fatalf("refreshed baseline should prevent a redundant trigger run, got %d", got)
	}
}

func TestTriggerDerivesTypedCredential(t *testing.T) {
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
	appKey, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatal(err)
	}
	der, err := x509.MarshalPKCS8PrivateKey(appKey)
	if err != nil {
		t.Fatal(err)
	}
	stored := strings.ReplaceAll(string(pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: der})), "\n", `\n`)
	if err := creds.Write(dir, key, map[string]string{"gh_key": stored}); err != nil {
		t.Fatal(err)
	}

	var mints, polls, revocations int
	transport := roundTripFunc(func(r *http.Request) (*http.Response, error) {
		status := http.StatusOK
		body := ""
		switch {
		case r.URL.Host == "api.github.com" && r.Method == http.MethodGet && r.URL.Path == "/app/installations":
			body = `[{"id":123,"app_id":1,"app_slug":"test-app"}]`
		case r.URL.Host == "api.github.com" && r.Method == http.MethodPost && r.URL.Path == "/app/installations/123/access_tokens":
			mints++
			body = `{"token":"derived-installation-token","expires_at":"2099-01-01T00:00:00Z"}`
		case r.URL.Host == "api.github.com" && r.Method == http.MethodGet && r.URL.Path == "/users/test-app[bot]":
			body = `{"id":4242}`
		case r.URL.Host == "api.github.com" && r.Method == http.MethodDelete && r.URL.Path == "/installation/token":
			revocations++
			status = http.StatusNoContent
		case r.URL.Host == "trigger.example.invalid" && r.Method == http.MethodGet:
			polls++
			if got := r.Header.Get("Authorization"); got != "Bearer derived-installation-token" {
				return nil, fmt.Errorf("trigger authorization = %q, want derived installation token", got)
			}
			body = "v1"
		default:
			return nil, fmt.Errorf("unexpected request: %s %s", r.Method, r.URL)
		}
		if r.Header.Get("Authorization") == "Bearer "+stored {
			return nil, fmt.Errorf("stored root secret reached %s", r.URL.Host)
		}
		return &http.Response{
			StatusCode: status,
			Status:     fmt.Sprintf("%d %s", status, http.StatusText(status)),
			Header:     make(http.Header),
			Body:       io.NopCloser(strings.NewReader(body)),
			Request:    r,
		}, nil
	})
	previousTransport := http.DefaultTransport
	http.DefaultTransport = transport
	t.Cleanup(func() { http.DefaultTransport = previousTransport })

	writeTriggerRoutine(t, dir, "https://trigger.example.invalid/actions", "  credential: gh_key\n  interval: 1m\ncredentials: [gh_key]\n")
	s := newSupervisor(t, dir)
	ctx := context.Background()
	t0 := time.Now().Truncate(time.Minute)

	s.tickWait(ctx, t0)
	s.tickWait(ctx, t0.Add(1*time.Minute))
	s.tickWait(ctx, t0.Add(2*time.Minute))
	if polls != 2 || mints != 2 || revocations != 2 {
		t.Fatalf("poll lifecycle = %d polls, %d mints, %d revocations; want 2 each", polls, mints, revocations)
	}
	if got := runCount(t, dir); got != 0 {
		t.Fatalf("unchanged polls must not fire, got %d runs", got)
	}
	if got := scrub.Redacted("derived-installation-token"); got != "derived-installation-token" {
		t.Fatalf("released bearer must leave the scrub registry, got %q", got)
	}
}

func TestTriggerRefusesUnlistedCredential(t *testing.T) {
	srv := newTriggerServer()
	defer srv.Close()
	dir := fixture(t, "ok")
	if err := os.WriteFile(filepath.Join(dir, creds.KeyFileName), []byte(creds.GenerateKey()), 0o600); err != nil {
		t.Fatal(err)
	}
	key, err := creds.LoadKey(dir)
	if err != nil {
		t.Fatal(err)
	}
	if err := creds.Write(dir, key, map[string]string{"gh_token": "a-raw-secret"}); err != nil {
		t.Fatal(err)
	}
	writeTriggerRoutine(t, dir, srv.URL, "  credential: gh_token\n  interval: 1m\n")
	s := newSupervisor(t, dir)
	ctx := context.Background()
	t0 := time.Now().Truncate(time.Minute)

	s.tickWait(ctx, t0)
	s.tickWait(ctx, t0.Add(1*time.Minute))
	s.tickWait(ctx, t0.Add(2*time.Minute))
	if srv.pollCount() != 0 {
		t.Fatalf("an unlisted trigger credential must never reach the wire, got %d polls", srv.pollCount())
	}
	if got := runCount(t, dir); got != 0 {
		t.Fatalf("refused poll must not fire, got %d runs", got)
	}
}
