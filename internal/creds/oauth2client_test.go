package creds

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/steadyspacecorp/openroutines/internal/scrub"
)

func TestDeriveOAuth2Client(t *testing.T) {
	var gotContentType, gotGrant, gotID, gotSecret string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotContentType = r.Header.Get("Content-Type")
		_ = r.ParseForm()
		gotGrant = r.PostForm.Get("grant_type")
		gotID = r.PostForm.Get("client_id")
		gotSecret = r.PostForm.Get("client_secret")
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"token_type":"bearer","access_token":"minted-bearer-123","expires_in":172800}`)) // gitleaks:allow -- synthetic fixture
	}))
	defer server.Close()

	spec := Spec{Type: "oauth2_client", TokenURL: server.URL, ClientID: "client-abc", InjectAs: "support_desk_token"}
	d, err := Derive("support_desk_secret", spec, "root-secret")
	if err != nil {
		t.Fatal(err)
	}
	if gotContentType != "application/x-www-form-urlencoded" || gotGrant != "client_credentials" ||
		gotID != "client-abc" || gotSecret != "root-secret" {
		t.Fatalf("token request wrong: ct=%q grant=%q id=%q secret=%q", gotContentType, gotGrant, gotID, gotSecret)
	}
	if d.Env["SUPPORT_DESK_TOKEN"] != "minted-bearer-123" || len(d.Env) != 1 {
		t.Fatalf("env wrong: %v", d.Env)
	}
	if d.Bearer != "minted-bearer-123" {
		t.Fatalf("bearer = %q, want minted access token", d.Bearer)
	}
	if got := scrub.Redacted("minted-bearer-123"); strings.Contains(got, "minted-bearer-123") {
		t.Fatalf("minting must register the bearer for redaction, got %q", got)
	}
	d.Cleanup()
}

func TestDeriveOAuth2ClientErrors(t *testing.T) {
	denied := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusUnauthorized)
		_, _ = w.Write([]byte(`{"error":"invalid_client","error_description":"bad secret"}`))
	}))
	defer denied.Close()
	if _, err := Derive("x", Spec{Type: "oauth2_client", TokenURL: denied.URL, ClientID: "c", InjectAs: "t"}, "s"); err == nil ||
		!strings.Contains(err.Error(), "invalid_client") {
		t.Fatalf("expected invalid_client error, got %v", err)
	}

	empty := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(`{"token_type":"bearer"}`))
	}))
	defer empty.Close()
	if _, err := Derive("x", Spec{Type: "oauth2_client", TokenURL: empty.URL, ClientID: "c", InjectAs: "t"}, "s"); err == nil ||
		!strings.Contains(err.Error(), "no access_token") {
		t.Fatalf("expected no-access-token error, got %v", err)
	}
}

func TestOAuth2ClientSpecProblems(t *testing.T) {
	valid := Spec{Type: "oauth2_client", TokenURL: "https://auth.example.com/oauth2/token", ClientID: "c", InjectAs: "desk_token"}
	if p := SpecProblems("s", valid); len(p) != 0 {
		t.Fatalf("valid spec flagged: %v", p)
	}
	for name, tc := range map[string]struct {
		mutate  func(*Spec)
		wantErr string
	}{
		"http token_url":   {func(s *Spec) { s.TokenURL = "http://auth.example.com/token" }, "https token_url"},
		"missing url":      {func(s *Spec) { s.TokenURL = "" }, "https token_url"},
		"missing client":   {func(s *Spec) { s.ClientID = "" }, "client_id"},
		"missing inject":   {func(s *Spec) { s.InjectAs = "" }, "inject_as"},
		"uppercase inject": {func(s *Spec) { s.InjectAs = "Desk_Token" }, "inject_as"},
		"reserved inject":  {func(s *Spec) { s.InjectAs = "path" }, "reserved"},
		"stray app_id":     {func(s *Spec) { s.AppID = "123" }, "not part of type oauth2_client"},
	} {
		s := valid
		tc.mutate(&s)
		p := SpecProblems("s", s)
		if len(p) != 1 || !strings.Contains(p[0], tc.wantErr) {
			t.Fatalf("%s: want one problem containing %q, got %v", name, tc.wantErr, p)
		}
	}
	if p := SpecProblems("g", Spec{Type: "github_app", AppID: "1", TokenURL: "https://x.example.com"}); len(p) != 1 ||
		!strings.Contains(p[0], "not part of type github_app") {
		t.Fatalf("github_app with stray oauth2 field: %v", p)
	}
}
