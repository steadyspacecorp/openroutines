package creds

import "testing"

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
