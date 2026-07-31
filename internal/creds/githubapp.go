package creds

import (
	"bytes"
	"crypto"
	"crypto/rand"
	"crypto/rsa"
	"crypto/sha256"
	"crypto/x509"
	"encoding/base64"
	"encoding/json"
	"encoding/pem"
	"fmt"
	"io"
	"net/http"
	"strconv"
	"strings"
	"time"
)

// The github_app derived type: the stored value is a GitHub App private key,
// and the run receives a one-hour installation token in its place. The
// installation is the source of truth for access -- the token is minted
// unscoped and inherits the installation's repository selection and
// permissions, managed on GitHub's installation page like a person's own
// access. Nothing here narrows per routine; access bounds are per-agent.
const (
	githubAPIBase    = "https://api.github.com"
	githubAPIVersion = "2026-03-10"
)

// githubHTTP never follows redirects: a redirect would carry the App JWT or
// installation token toward a location the framework did not choose.
var githubHTTP = &http.Client{
	Timeout: 30 * time.Second,
	CheckRedirect: func(*http.Request, []*http.Request) error {
		return http.ErrUseLastResponse
	},
}

func deriveGitHubApp(s Spec, stored, apiBase string) (*Derived, error) {
	jwt, err := githubAppJWT(s.AppID, stored)
	if err != nil {
		return nil, err
	}

	// The App must have exactly one installation: with no configured
	// installation ID, plurality would make the grant ambiguous.
	var installations []struct {
		ID      int64  `json:"id"`
		AppID   int64  `json:"app_id"`
		AppSlug string `json:"app_slug"`
	}
	if err := githubRequest(apiBase, "GET", "/app/installations?per_page=2", jwt, nil, &installations); err != nil {
		return nil, err
	}
	if len(installations) != 1 {
		return nil, fmt.Errorf("github_app: App %s must have exactly one installation, found %d", s.AppID, len(installations))
	}
	inst := installations[0]
	if strconv.FormatInt(inst.AppID, 10) != s.AppID {
		return nil, fmt.Errorf("github_app: the discovered installation belongs to App %d, not configured app_id %s", inst.AppID, s.AppID)
	}
	if inst.AppSlug == "" {
		return nil, fmt.Errorf("github_app: GitHub did not return the App slug")
	}

	var token struct {
		Token     string `json:"token"`
		ExpiresAt string `json:"expires_at"`
	}
	if err := githubRequest(apiBase, "POST", fmt.Sprintf("/app/installations/%d/access_tokens", inst.ID), jwt, map[string]any{}, &token); err != nil {
		return nil, err
	}
	if token.Token == "" {
		return nil, fmt.Errorf("github_app: GitHub did not return an installation token")
	}
	// From here the token is live; a failure must not leave it valid until
	// its natural expiry.
	revoke := func() {
		_ = githubRequest(apiBase, "DELETE", "/installation/token", token.Token, nil, nil)
	}

	var bot struct {
		ID int64 `json:"id"`
	}
	botName := inst.AppSlug + "[bot]"
	if err := githubRequest(apiBase, "GET", "/users/"+inst.AppSlug+"%5Bbot%5D", token.Token, nil, &bot); err != nil {
		revoke()
		return nil, err
	}
	if bot.ID == 0 {
		revoke()
		return nil, fmt.Errorf("github_app: GitHub did not return the App bot identity")
	}
	botEmail := fmt.Sprintf("%d+%s@users.noreply.github.com", bot.ID, botName)

	return &Derived{
		Env: map[string]string{
			"GITHUB_TOKEN":        token.Token,
			"GH_TOKEN":            token.Token,
			"GITHUB_APP_SLUG":     inst.AppSlug,
			"GIT_AUTHOR_NAME":     botName,
			"GIT_AUTHOR_EMAIL":    botEmail,
			"GIT_COMMITTER_NAME":  botName,
			"GIT_COMMITTER_EMAIL": botEmail,
		},
		Bearer:  token.Token,
		Cleanup: revoke,
	}, nil
}

// parseAppKey decodes the stored App private key. The stored value may carry
// real PEM newlines or escaped \n sequences (the one-line form keeps the
// encrypted value scrubbable as an exact string).
func parseAppKey(stored string) (*rsa.PrivateKey, error) {
	block, _ := pem.Decode([]byte(strings.ReplaceAll(stored, `\n`, "\n")))
	if block == nil {
		return nil, fmt.Errorf("github_app: stored value is not a PEM private key")
	}
	if parsed, err := x509.ParsePKCS8PrivateKey(block.Bytes); err == nil {
		rsaKey, isRSA := parsed.(*rsa.PrivateKey)
		if !isRSA {
			return nil, fmt.Errorf("github_app: private key is not RSA")
		}
		return rsaKey, nil
	}
	if parsed, err := x509.ParsePKCS1PrivateKey(block.Bytes); err == nil {
		return parsed, nil
	}
	return nil, fmt.Errorf("github_app: cannot parse the stored private key")
}

// githubAppJWT signs the 9-minute RS256 App JWT.
func githubAppJWT(appID, stored string) (string, error) {
	key, err := parseAppKey(stored)
	if err != nil {
		return "", err
	}

	b64 := func(b []byte) string { return base64.RawURLEncoding.EncodeToString(b) }
	now := time.Now().Unix()
	unsigned := b64([]byte(`{"alg":"RS256","typ":"JWT"}`)) + "." +
		b64(fmt.Appendf(nil, `{"iat":%d,"exp":%d,"iss":"%s"}`, now-60, now+540, appID))
	digest := sha256.Sum256([]byte(unsigned))
	signature, err := rsa.SignPKCS1v15(rand.Reader, key, crypto.SHA256, digest[:])
	if err != nil {
		return "", fmt.Errorf("github_app: signing failed: %w", err)
	}
	return unsigned + "." + b64(signature), nil
}

func githubRequest(base, method, path, bearer string, body, out any) error {
	var payload io.Reader
	if body != nil {
		raw, err := json.Marshal(body)
		if err != nil {
			return err
		}
		payload = bytes.NewReader(raw)
	}
	req, err := http.NewRequest(method, base+path, payload)
	if err != nil {
		return err
	}
	req.Header.Set("Authorization", "Bearer "+bearer)
	req.Header.Set("Accept", "application/vnd.github+json")
	req.Header.Set("X-GitHub-Api-Version", githubAPIVersion)
	req.Header.Set("User-Agent", "openroutines")
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	resp, err := githubHTTP.Do(req)
	if err != nil {
		return fmt.Errorf("github_app: could not reach the GitHub API: %w", err)
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if resp.StatusCode < 200 || resp.StatusCode > 299 {
		var apiErr struct {
			Message string `json:"message"`
		}
		_ = json.Unmarshal(raw, &apiErr)
		detail := ""
		if apiErr.Message != "" {
			detail = ": " + apiErr.Message
		}
		return fmt.Errorf("github_app: GitHub API %s %s returned %d%s", method, path, resp.StatusCode, detail)
	}
	if out != nil {
		if err := json.Unmarshal(raw, out); err != nil {
			return fmt.Errorf("github_app: unexpected GitHub API response: %w", err)
		}
	}
	return nil
}
