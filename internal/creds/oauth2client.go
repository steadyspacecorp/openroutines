package creds

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"
)

// Never follows redirects: a redirect would carry the client secret or
// minted bearer toward a location the framework did not choose.
var oauth2HTTP = &http.Client{
	Timeout: 30 * time.Second,
	CheckRedirect: func(*http.Request, []*http.Request) error {
		return http.ErrUseLastResponse
	},
}

func deriveOAuth2Client(s Spec, stored string) (*Derived, error) {
	form := url.Values{
		"grant_type":    {"client_credentials"},
		"client_id":     {s.ClientID},
		"client_secret": {stored},
	}
	req, err := http.NewRequest("POST", s.TokenURL, strings.NewReader(form.Encode()))
	if err != nil {
		return nil, fmt.Errorf("oauth2_client: token_url: %w", err)
	}
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.Header.Set("Accept", "application/json")
	req.Header.Set("User-Agent", "openroutines")
	resp, err := oauth2HTTP.Do(req)
	if err != nil {
		return nil, fmt.Errorf("oauth2_client: could not reach the token endpoint: %w", err)
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if resp.StatusCode < 200 || resp.StatusCode > 299 {
		// RFC 6749 section 5.2 error shape; detail helps a human fix the
		// entry without the framework ever logging the secret itself.
		var apiErr struct {
			Error       string `json:"error"`
			Description string `json:"error_description"`
		}
		_ = json.Unmarshal(raw, &apiErr)
		detail := ""
		if apiErr.Error != "" {
			detail = ": " + apiErr.Error
		}
		if apiErr.Description != "" {
			detail += " (" + apiErr.Description + ")"
		}
		return nil, fmt.Errorf("oauth2_client: token endpoint returned %d%s", resp.StatusCode, detail)
	}
	var token struct {
		AccessToken string `json:"access_token"`
	}
	if err := json.Unmarshal(raw, &token); err != nil {
		return nil, fmt.Errorf("oauth2_client: unexpected token response: %w", err)
	}
	if token.AccessToken == "" {
		return nil, fmt.Errorf("oauth2_client: token endpoint returned no access_token")
	}

	return &Derived{
		Env:     map[string]string{strings.ToUpper(s.InjectAs): token.AccessToken},
		Bearer:  token.AccessToken,
		Cleanup: func() {},
	}, nil
}
