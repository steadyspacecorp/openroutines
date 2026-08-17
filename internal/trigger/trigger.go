// Implements event-driven routine wake-ups: a cheap,
// outbound change-detection poll evaluated on the supervisor's tick. A
// trigger carries no payload -- on change the routine simply becomes due and
// pulls its actual work through its own skills. The poll response is opaque:
// compared and stored, never logged raw or shown to the model.
package trigger

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"time"
)

const (
	// The poll cadence when the frontmatter doesn't set
	// one. The interval bounds both request rate and fire rate (a poll is the
	// only fire opportunity), so one knob covers courtesy and spend alike.
	DefaultInterval = 5 * time.Minute

	// Bounds one poll; polls run serially on the tick, so a
	// hung endpoint must not eat the minute.
	RequestTimeout = 10 * time.Second

	// Bounds a body that must be held in knowledge for JSON
	// extraction. hashBodyCap bounds how much of a raw body is streamed into
	// the comparison hash -- hashing needs no buffer, so it can be generous.
	selectBodyCap = 256 << 10
	hashBodyCap   = 4 << 20
)

const StateDirName = "triggers"

var credentialReferencePattern = regexp.MustCompile(`\$[A-Z][A-Z0-9_]*`)

func CredentialReference(name string) string {
	return "$" + strings.ToUpper(name)
}

type State struct {
	Routine      string `json:"routine"`
	Value        string `json:"value"`
	ETag         string `json:"etag,omitempty"`
	LastModified string `json:"last_modified,omitempty"`
}

func statePath(stateDir, name string) string {
	return filepath.Join(stateDir, StateDirName, name+".json")
}

func Load(stateDir, name string) (*State, error) {
	raw, err := os.ReadFile(statePath(stateDir, name))
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, err
	}
	var s State
	if err := json.Unmarshal(raw, &s); err != nil {
		return nil, fmt.Errorf("trigger state %s: %w", name, err)
	}
	return &s, nil
}

func (s *State) Save(stateDir string) error {
	if err := os.MkdirAll(filepath.Join(stateDir, StateDirName), 0o755); err != nil {
		return err
	}
	raw, err := json.MarshalIndent(s, "", "  ")
	if err != nil {
		return err
	}
	return os.WriteFile(statePath(stateDir, s.Routine), append(raw, '\n'), 0o644)
}

type Spec struct {
	Poll       string `yaml:"poll"`
	Credential string `yaml:"credential,omitempty"`
	Select     string `yaml:"select,omitempty"`
	Interval   string `yaml:"interval,omitempty"`
}

func (t Spec) IntervalDuration() (time.Duration, error) {
	if t.Interval == "" {
		return DefaultInterval, nil
	}
	return time.ParseDuration(t.Interval)
}

func (t Spec) Validate() error {
	if t.Poll == "" {
		return errors.New("trigger: missing poll URL")
	}
	u, err := url.Parse(t.Poll)
	if err != nil || u.Scheme != "http" && u.Scheme != "https" || u.Host == "" {
		return fmt.Errorf("trigger: poll %q is not an http(s) URL", t.Poll)
	}
	references := credentialReferencePattern.FindAllString(t.Poll, -1)
	for _, reference := range references {
		if strings.Contains(u.Host, reference) {
			return fmt.Errorf("trigger: %s cannot appear in the poll host -- the destination must be reviewable", reference)
		}
		if t.Credential == "" {
			return fmt.Errorf("trigger: poll has a %s reference but no credential", reference)
		}
		if want := CredentialReference(t.Credential); reference != want {
			return fmt.Errorf("trigger: poll credential reference %s does not match %s", reference, want)
		}
	}
	if t.Select != "" {
		if err := validatePointer(t.Select); err != nil {
			return fmt.Errorf("trigger: select: %w", err)
		}
	}
	if _, err := t.IntervalDuration(); err != nil {
		return fmt.Errorf("trigger: interval %q is not a valid duration", t.Interval)
	}
	return nil
}

// The poll HTTP client: short timeout, and no redirects -- a
// declared URL is the reviewed grant, and a redirect is a different URL.
var Client = &http.Client{
	Timeout: RequestTimeout,
	CheckRedirect: func(*http.Request, []*http.Request) error {
		return errors.New("redirects are not followed")
	},
}

type Result struct {
	Changed bool
	Next    State
}

// Performs one change-detection request. prior may be nil (first
// sight): the first observation establishes the baseline and never reports a
// change. The credential, when present, is sent as a bearer token -- or
// substituted for its run-environment reference in the URL instead -- and never
// appears in errors.
func Poll(client *http.Client, spec Spec, credential string, name string, prior *State) (Result, error) {
	pollURL := spec.Poll
	reference := CredentialReference(spec.Credential)
	substituted := spec.Credential != "" && strings.Contains(pollURL, reference)
	if substituted {
		if credential == "" {
			return Result{}, fmt.Errorf("poll has a %s reference but no credential value", reference)
		}
		pollURL = strings.ReplaceAll(pollURL, reference, credential)
	}
	req, err := http.NewRequest(http.MethodGet, pollURL, nil)
	if err != nil {
		// The substituted URL may carry the credential; the error would echo it.
		return Result{}, errors.New("poll URL does not form a valid request")
	}
	req.Header.Set("User-Agent", "openroutines-trigger")
	req.Header.Set("Accept", "*/*")
	if credential != "" && !substituted {
		req.Header.Set("Authorization", "Bearer "+credential)
	}
	if prior != nil {
		if prior.ETag != "" {
			req.Header.Set("If-None-Match", prior.ETag)
		}
		if prior.LastModified != "" {
			req.Header.Set("If-Modified-Since", prior.LastModified)
		}
	}
	resp, err := client.Do(req)
	if err != nil {
		return Result{}, redactURL(err)
	}
	defer resp.Body.Close()

	if resp.StatusCode == http.StatusNotModified {
		if prior == nil {
			return Result{}, errors.New("poll returned 304 Not Modified before a baseline was established")
		}
		return Result{Changed: false, Next: *prior}, nil
	}
	if resp.StatusCode < 200 || resp.StatusCode > 299 {
		return Result{}, fmt.Errorf("poll returned %s", resp.Status)
	}

	value, err := observe(resp.Body, spec.Select)
	if err != nil {
		return Result{}, err
	}
	next := State{
		Routine:      name,
		Value:        value,
		ETag:         resp.Header.Get("ETag"),
		LastModified: resp.Header.Get("Last-Modified"),
	}
	return Result{Changed: prior != nil && value != prior.Value, Next: next}, nil
}

func observe(body io.Reader, selector string) (string, error) {
	if selector == "" {
		h := sha256.New()
		n, err := io.Copy(h, io.LimitReader(body, hashBodyCap+1))
		if err != nil {
			return "", err
		}
		if n > hashBodyCap {
			return "", fmt.Errorf("response exceeds %d bytes -- use a smaller endpoint or select one JSON value", hashBodyCap)
		}
		return "sha256:" + hex.EncodeToString(h.Sum(nil)), nil
	}
	raw, err := io.ReadAll(io.LimitReader(body, selectBodyCap+1))
	if err != nil {
		return "", err
	}
	if len(raw) > selectBodyCap {
		return "", fmt.Errorf("response exceeds %d bytes -- select needs a smaller endpoint", selectBodyCap)
	}
	dec := json.NewDecoder(strings.NewReader(string(raw)))
	dec.UseNumber()
	var doc any
	if err := dec.Decode(&doc); err != nil {
		return "", fmt.Errorf("response is not JSON: %w", err)
	}
	v, ok := resolvePointer(doc, selector)
	if !ok {
		// An absent path is a legitimate observation (an empty channel has no
		// newest message), not an error to spam the logs with.
		return "", nil
	}
	switch t := v.(type) {
	case string:
		return t, nil
	case json.Number:
		return t.String(), nil
	case bool:
		return strconv.FormatBool(t), nil
	case nil:
		return "null", nil
	default:
		return "", fmt.Errorf("select %s resolves to a %T, not a scalar", selector, v)
	}
}

func redactURL(err error) error {
	var u *url.Error
	if errors.As(err, &u) {
		return fmt.Errorf("poll %s: %w", u.Op, u.Err)
	}
	return err
}

func validatePointer(p string) error {
	if !strings.HasPrefix(p, "/") {
		return fmt.Errorf("pointer %q must start with /", p)
	}
	for _, tok := range strings.Split(p[1:], "/") {
		for i := 0; i < len(tok); i++ {
			if tok[i] == '~' && (i+1 >= len(tok) || tok[i+1] != '0' && tok[i+1] != '1') {
				return fmt.Errorf("pointer %q has an invalid ~ escape", p)
			}
		}
	}
	return nil
}

func resolvePointer(doc any, pointer string) (any, bool) {
	cur := doc
	for _, tok := range strings.Split(pointer[1:], "/") {
		tok = strings.ReplaceAll(strings.ReplaceAll(tok, "~1", "/"), "~0", "~")
		switch node := cur.(type) {
		case map[string]any:
			v, ok := node[tok]
			if !ok {
				return nil, false
			}
			cur = v
		case []any:
			i, err := strconv.Atoi(tok)
			if err != nil || i < 0 || i >= len(node) {
				return nil, false
			}
			cur = node[i]
		default:
			return nil, false
		}
	}
	return cur, true
}
