package creds

import (
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/steadyspacecorp/openroutines/internal/scrub"
)

// Two run slots minting the same credential hold distinct bearers at once;
// minting the second must not stop redacting the first, and releasing one
// run's material must not stop redacting the other's.
func TestOverlappingMintsOfOneCredentialAllRedact(t *testing.T) {
	mints := 0
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		mints++
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprintf(w, `{"token_type":"bearer","access_token":"overlap-bearer-%d"}`, mints)
	}))
	defer server.Close()

	spec := Spec{Type: "oauth2_client", TokenURL: server.URL, ClientID: "c", InjectAs: "desk_token"}
	first, err := Derive("desk", spec, "root")
	if err != nil {
		t.Fatal(err)
	}
	second, err := Derive("desk", spec, "root")
	if err != nil {
		t.Fatal(err)
	}
	if first.Bearer == second.Bearer {
		t.Fatalf("fixture must mint distinct bearers, got %q twice", first.Bearer)
	}
	for _, b := range []string{first.Bearer, second.Bearer} {
		if got := scrub.Redacted(b); strings.Contains(got, b) {
			t.Fatalf("still-active bearer %q must redact, got %q", b, got)
		}
	}
	first.Cleanup()
	if got := scrub.Redacted(second.Bearer); strings.Contains(got, second.Bearer) {
		t.Fatalf("releasing one run's material must not stop redacting another's still-active bearer, got %q", got)
	}
}

// InjectionDescription must state what a run actually receives -- a typed
// credential's stored value is never injected, so describing it as if it
// were raw sends operators to write an env var that is always empty (#66).
func TestInjectionDescription(t *testing.T) {
	for _, c := range []struct {
		name string
		spec Spec
		want string
	}{
		{
			name: "raw",
			spec: Spec{},
			want: "-- routines that declare it receive it as GITHUB_APP_PRIVATE_KEY",
		},
		{
			name: "github_app",
			spec: Spec{Type: "github_app", AppID: "12345"},
			want: "(type: github_app) -- routines that declare it receive GITHUB_TOKEN, GH_TOKEN, GITHUB_APP_SLUG, and the App's git identity. The stored key is never injected.",
		},
		{
			name: "oauth2_client",
			spec: Spec{Type: "oauth2_client", TokenURL: "https://example.invalid/token", ClientID: "abc", InjectAs: "support_desk_token"},
			want: "(type: oauth2_client) -- routines that declare it receive the minted bearer as SUPPORT_DESK_TOKEN. The stored client secret is never injected.",
		},
	} {
		t.Run(c.name, func(t *testing.T) {
			if got := InjectionDescription("github_app_private_key", c.spec); got != c.want {
				t.Errorf("InjectionDescription = %q, want %q", got, c.want)
			}
		})
	}
}
