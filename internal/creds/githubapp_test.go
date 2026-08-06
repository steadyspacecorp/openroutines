package creds

import (
	"crypto/rand"
	"crypto/rsa"
	"crypto/x509"
	"encoding/json"
	"encoding/pem"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/steadyspacecorp/openroutines/internal/logging/logtest"
)

func testKeyPEM(t *testing.T) string {
	t.Helper()
	key, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatal(err)
	}
	der, err := x509.MarshalPKCS8PrivateKey(key)
	if err != nil {
		t.Fatal(err)
	}
	return string(pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: der}))
}

// githubStub serves the three-call mint flow plus revocation. installations
// is mutable so tests can present zero, one, or two installations;
// failBotLookup makes the post-mint identity call fail.
type githubStub struct {
	installations []map[string]any
	revocations   int
	mintBodies    []string
	failBotLookup bool
	failRevoke    bool
}

func (g *githubStub) server(t *testing.T) *httptest.Server {
	t.Helper()
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") == "" {
			t.Errorf("%s %s arrived without authorization", r.Method, r.URL.Path)
		}
		var payload any
		switch {
		case r.URL.Path == "/app/installations":
			payload = g.installations
		case r.URL.Path == "/app/installations/123/access_tokens" && r.Method == "POST":
			body := make([]byte, 1024)
			n, _ := r.Body.Read(body)
			g.mintBodies = append(g.mintBodies, string(body[:n]))
			payload = map[string]any{"token": "test-installation-token", "expires_at": "2099-01-01T00:00:00Z"}
		case r.URL.Path == "/users/test-app[bot]":
			if g.failBotLookup {
				w.WriteHeader(500)
				_, _ = w.Write([]byte(`{"message":"boom"}`))
				return
			}
			payload = map[string]any{"id": 4242}
		case r.URL.Path == "/installation/token" && r.Method == "DELETE":
			g.revocations++
			if g.failRevoke {
				// The error message quotes the presented bearer, the way a
				// hostile or echoing endpoint might: what reaches the log
				// must arrive redacted, not depend on GitHub's manners.
				w.WriteHeader(500)
				bearer := strings.TrimPrefix(r.Header.Get("Authorization"), "Bearer ")
				_, _ = w.Write([]byte(`{"message":"cannot revoke ` + bearer + `"}`))
				return
			}
			w.WriteHeader(204)
			return
		default:
			w.WriteHeader(404)
			_, _ = w.Write([]byte(`{"message":"unexpected test path"}`))
			return
		}
		_ = json.NewEncoder(w).Encode(payload)
	}))
}

func oneInstallation() []map[string]any {
	return []map[string]any{{"id": 123, "app_id": 456, "app_slug": "test-app"}}
}

func TestDeriveGitHubApp(t *testing.T) {
	stub := &githubStub{installations: oneInstallation()}
	srv := stub.server(t)
	defer srv.Close()

	// The one-line escaped key form must work: it is the recommended storage
	// format (exact-value log scrubbing).
	escaped := strings.ReplaceAll(testKeyPEM(t), "\n", `\n`)
	d, err := deriveGitHubApp("gh", Spec{Type: "github_app", AppID: "456"}, escaped, srv.URL)
	if err != nil {
		t.Fatal(err)
	}
	for k, want := range map[string]string{
		"GITHUB_TOKEN":        "test-installation-token",
		"GH_TOKEN":            "test-installation-token",
		"GITHUB_APP_SLUG":     "test-app",
		"GIT_AUTHOR_NAME":     "test-app[bot]",
		"GIT_AUTHOR_EMAIL":    "4242+test-app[bot]@users.noreply.github.com",
		"GIT_COMMITTER_NAME":  "test-app[bot]",
		"GIT_COMMITTER_EMAIL": "4242+test-app[bot]@users.noreply.github.com",
	} {
		if d.Env[k] != want {
			t.Fatalf("env %s = %q, want %q", k, d.Env[k], want)
		}
	}
	if d.Bearer != "test-installation-token" {
		t.Fatalf("bearer = %q, want installation token", d.Bearer)
	}
	for _, body := range stub.mintBodies {
		if strings.TrimSpace(body) != "{}" {
			t.Fatalf("token mint must be unscoped (the installation governs access), sent %q", body)
		}
	}
	d.Cleanup()
	if stub.revocations != 1 {
		t.Fatalf("expected 1 revocation after cleanup, got %d", stub.revocations)
	}
}

// A revocation that fails leaves the installation token live until it
// expires, so the operator needs the warning even though the run itself
// proceeds unaffected.
func TestDeriveGitHubAppLogsFailedRevocation(t *testing.T) {
	stub := &githubStub{installations: oneInstallation(), failRevoke: true}
	srv := stub.server(t)
	defer srv.Close()

	logs := logtest.Capture(t)

	d, err := deriveGitHubApp("gh", Spec{Type: "github_app", AppID: "456"}, testKeyPEM(t), srv.URL)
	if err != nil {
		t.Fatal(err)
	}
	d.Cleanup()

	logs.Expect("revocation failed", "credential=gh")
}

func TestDeriveGitHubAppRefusesAmbiguity(t *testing.T) {
	key := testKeyPEM(t)

	stub := &githubStub{installations: append(oneInstallation(), map[string]any{"id": 124, "app_id": 456, "app_slug": "test-app"})}
	srv := stub.server(t)
	defer srv.Close()
	if _, err := deriveGitHubApp("gh", Spec{Type: "github_app", AppID: "456"}, key, srv.URL); err == nil || !strings.Contains(err.Error(), "exactly one installation") {
		t.Fatalf("two installations must be refused, got %v", err)
	}

	stub2 := &githubStub{installations: oneInstallation()}
	srv2 := stub2.server(t)
	defer srv2.Close()
	if _, err := deriveGitHubApp("gh", Spec{Type: "github_app", AppID: "999"}, key, srv2.URL); err == nil || !strings.Contains(err.Error(), "belongs to App") {
		t.Fatalf("an installation of a different App must be refused, got %v", err)
	}
	if stub.revocations+stub2.revocations != 0 {
		t.Fatal("no token should have been minted, so nothing should revoke")
	}
}

func TestDeriveGitHubAppRevokesOnPartialFailure(t *testing.T) {
	stub := &githubStub{installations: oneInstallation(), failBotLookup: true}
	srv := stub.server(t)
	defer srv.Close()
	if _, err := deriveGitHubApp("gh", Spec{Type: "github_app", AppID: "456"}, testKeyPEM(t), srv.URL); err == nil {
		t.Fatal("a failed bot lookup must fail derivation")
	}
	if stub.revocations != 1 {
		t.Fatalf("a token minted by a failed derivation must be revoked, got %d revocations", stub.revocations)
	}
}

// The token is live -- and loggable -- from the moment GitHub returns it,
// so it registers with the scrub registry right there, not only when
// Derive returns: a failed revocation's warning during the mint window
// must not publish it.
func TestDeriveGitHubAppRedactsTokenInMintWindow(t *testing.T) {
	stub := &githubStub{installations: oneInstallation(), failBotLookup: true, failRevoke: true}
	srv := stub.server(t)
	defer srv.Close()

	logs := logtest.Capture(t)

	if _, err := deriveGitHubApp("gh", Spec{Type: "github_app", AppID: "456"}, testKeyPEM(t), srv.URL); err == nil {
		t.Fatal("a failed bot lookup must fail derivation")
	}
	logs.Reject("test-installation-token")
	logs.Expect("[REDACTED:")
}

func TestDeriveGitHubAppRejectsBadKey(t *testing.T) {
	if _, err := deriveGitHubApp("gh", Spec{Type: "github_app", AppID: "456"}, "not a key", "http://127.0.0.1:0"); err == nil {
		t.Fatal("a non-PEM stored value must be rejected before any request")
	}
}

func TestSpecProblems(t *testing.T) {
	if p := SpecProblems("x", Spec{Type: "github_app", AppID: "123"}); len(p) != 0 {
		t.Fatalf("valid spec flagged: %v", p)
	}
	for name, spec := range map[string]Spec{
		"missing type":   {},
		"unknown type":   {Type: "aws_sts"},
		"missing app id": {Type: "github_app"},
		"bad app id":     {Type: "github_app", AppID: "abc"},
	} {
		if p := SpecProblems("x", spec); len(p) == 0 {
			t.Fatalf("%s not flagged", name)
		}
	}
}

// ValidateStored judges a stored github_app value offline: both stored
// forms parse, and a truncated first line -- the artifact `credentials set`
// used to store silently (#69) -- is rejected. Other types stay opaque.
func TestValidateStoredGitHubAppKey(t *testing.T) {
	spec := Spec{Type: "github_app", AppID: "1"}
	pemKey := testKeyPEM(t)
	if err := ValidateStored(spec, pemKey); err != nil {
		t.Fatalf("real-newline PEM should validate: %v", err)
	}
	if err := ValidateStored(spec, strings.ReplaceAll(pemKey, "\n", `\n`)); err != nil {
		t.Fatalf("escaped one-line PEM should validate: %v", err)
	}
	if err := ValidateStored(spec, "-----BEGIN PRIVATE KEY-----"); err == nil {
		t.Fatal("a truncated PEM must not validate")
	}
	if err := ValidateStored(Spec{Type: "oauth2_client"}, "opaque"); err != nil {
		t.Fatalf("oauth2_client values are opaque bytes: %v", err)
	}
}
