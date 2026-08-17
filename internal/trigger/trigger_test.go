package trigger

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

func TestResolvePointer(t *testing.T) {
	doc := map[string]any{
		"messages": []any{
			map[string]any{"ts": "1721.001", "a/b": "slash", "t~e": "tilde"},
		},
		"cursor": "abc",
	}
	cases := []struct {
		pointer string
		want    string
		ok      bool
	}{
		{"/cursor", "abc", true},
		{"/messages/0/ts", "1721.001", true},
		{"/messages/0/a~1b", "slash", true},
		{"/messages/0/t~0e", "tilde", true},
		{"/messages/1/ts", "", false},
		{"/missing", "", false},
		{"/cursor/deeper", "", false},
	}
	for _, c := range cases {
		got, ok := resolvePointer(doc, c.pointer)
		if ok != c.ok || (ok && got != c.want) {
			t.Errorf("%s: got %v, %v; want %v, %v", c.pointer, got, ok, c.want, c.ok)
		}
	}
}

func TestValidate(t *testing.T) {
	good := Spec{Poll: "https://example.com/x", Select: "/a/0/b", Interval: "2m"}
	if err := good.Validate(); err != nil {
		t.Fatalf("valid spec rejected: %v", err)
	}
	reference := Spec{Poll: "https://api.telegram.org/bot$TELEGRAM_BOT_TOKEN/getUpdates", Credential: "telegram_bot_token"}
	if err := reference.Validate(); err != nil {
		t.Fatalf("credential reference spec rejected: %v", err)
	}
	bad := []Spec{
		{},
		{Poll: "ftp://example.com"},
		{Poll: "not a url"},
		{Poll: "https://example.com", Select: "no-slash"},
		{Poll: "https://example.com", Select: "/x~2y"},
		{Poll: "https://example.com", Interval: "often"},
		{Poll: "https://api.telegram.org/bot$TELEGRAM_BOT_TOKEN/getUpdates"},
		{Poll: "https://$TOKEN.example.com/x", Credential: "token"},
		{Poll: "https://api.telegram.org/bot$WRONG_TOKEN/getUpdates", Credential: "telegram_bot_token"},
	}
	for i, s := range bad {
		if err := s.Validate(); err == nil {
			t.Errorf("bad spec %d accepted: %+v", i, s)
		}
	}
}

type pollServer struct {
	body   string
	etag   string
	status int
	got304 int
	auth   string
	path   string
}

func (p *pollServer) handler(w http.ResponseWriter, r *http.Request) {
	p.auth = r.Header.Get("Authorization")
	p.path = r.URL.Path
	if p.etag != "" && r.Header.Get("If-None-Match") == p.etag {
		p.got304++
		w.WriteHeader(http.StatusNotModified)
		return
	}
	if p.etag != "" {
		w.Header().Set("ETag", p.etag)
	}
	if p.status != 0 {
		w.WriteHeader(p.status)
	}
	w.Write([]byte(p.body))
}

func TestPollBaselineChangeAndSelect(t *testing.T) {
	ps := &pollServer{body: `{"messages":[{"ts":"100.1"}]}`}
	srv := httptest.NewServer(http.HandlerFunc(ps.handler))
	defer srv.Close()
	spec := Spec{Poll: srv.URL, Select: "/messages/0/ts"}

	res, err := Poll(srv.Client(), spec, "tok-123", "r", nil)
	if err != nil || res.Changed {
		t.Fatalf("baseline poll: changed=%v err=%v", res.Changed, err)
	}
	if res.Next.Value != "100.1" {
		t.Fatalf("selected value: %q", res.Next.Value)
	}
	if ps.auth != "Bearer tok-123" {
		t.Fatalf("credential not sent as bearer: %q", ps.auth)
	}

	prior := res.Next
	res, err = Poll(srv.Client(), spec, "", "r", &prior)
	if err != nil || res.Changed {
		t.Fatalf("unchanged poll: changed=%v err=%v", res.Changed, err)
	}

	ps.body = `{"messages":[{"ts":"200.2"}]}`
	res, err = Poll(srv.Client(), spec, "", "r", &prior)
	if err != nil || !res.Changed || res.Next.Value != "200.2" {
		t.Fatalf("changed poll: %+v err=%v", res, err)
	}

	ps.body = `{"messages":[]}`
	p2 := res.Next
	res, err = Poll(srv.Client(), spec, "", "r", &p2)
	if err != nil || !res.Changed || res.Next.Value != "" {
		t.Fatalf("absent pointer: %+v err=%v", res, err)
	}
}

func TestPollRawBodyHashAndETag(t *testing.T) {
	ps := &pollServer{body: "v1", etag: `"e1"`}
	srv := httptest.NewServer(http.HandlerFunc(ps.handler))
	defer srv.Close()
	spec := Spec{Poll: srv.URL}

	res, err := Poll(srv.Client(), spec, "", "r", nil)
	if err != nil || !strings.HasPrefix(res.Next.Value, "sha256:") || res.Next.ETag != `"e1"` {
		t.Fatalf("baseline: %+v err=%v", res, err)
	}
	prior := res.Next

	res, err = Poll(srv.Client(), spec, "", "r", &prior)
	if err != nil || res.Changed || ps.got304 != 1 {
		t.Fatalf("304 path: changed=%v got304=%d err=%v", res.Changed, ps.got304, err)
	}

	ps.body, ps.etag = "v2", `"e2"`
	res, err = Poll(srv.Client(), spec, "", "r", &prior)
	if err != nil || !res.Changed {
		t.Fatalf("change path: changed=%v err=%v", res.Changed, err)
	}
	if res.Next.Value == prior.Value {
		t.Fatal("hash should differ for new body")
	}
}

func TestPollCredentialReference(t *testing.T) {
	ps := &pollServer{body: `{"ok":true,"result":[]}`}
	srv := httptest.NewServer(http.HandlerFunc(ps.handler))
	defer srv.Close()
	spec := Spec{Poll: srv.URL + "/bot$TELEGRAM_BOT_TOKEN/getUpdates", Credential: "telegram_bot_token"}

	res, err := Poll(srv.Client(), spec, "12345:secret", "r", nil)
	if err != nil || res.Changed {
		t.Fatalf("baseline poll: changed=%v err=%v", res.Changed, err)
	}
	if ps.path != "/bot12345:secret/getUpdates" {
		t.Fatalf("credential not substituted into the URL: %q", ps.path)
	}
	if ps.auth != "" {
		t.Fatalf("substituted credential must not also travel as a bearer: %q", ps.auth)
	}

	if _, err := Poll(srv.Client(), spec, "", "r", nil); err == nil {
		t.Fatal("credential reference with no value should be an error")
	} else if strings.Contains(err.Error(), "secret") {
		t.Fatalf("error leaks material: %v", err)
	}
}

func TestPollRefuses304BeforeBaseline(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusNotModified)
	}))
	defer srv.Close()
	if _, err := Poll(srv.Client(), Spec{Poll: srv.URL}, "", "r", nil); err == nil {
		t.Fatal("304 before a baseline should be an error")
	}
}

func TestPollRefusesRedirectsAndErrors(t *testing.T) {
	redirect := httptest.NewServer(http.RedirectHandler("https://elsewhere.invalid/", http.StatusFound))
	defer redirect.Close()
	if _, err := Poll(redirect.Client(), Spec{Poll: redirect.URL}, "", "r", nil); err == nil {
		t.Fatal("redirect should be refused")
	} else if strings.Contains(err.Error(), redirect.URL) {
		t.Fatalf("error leaks the poll URL: %v", err)
	}

	ps := &pollServer{body: "nope", status: http.StatusForbidden}
	srv := httptest.NewServer(http.HandlerFunc(ps.handler))
	defer srv.Close()
	if _, err := Poll(srv.Client(), Spec{Poll: srv.URL}, "", "r", nil); err == nil {
		t.Fatal("non-2xx should be an error")
	}
}

func TestPollSelectBodyCap(t *testing.T) {
	big := `{"pad":"` + strings.Repeat("x", selectBodyCap) + `"}`
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Write([]byte(big))
	}))
	defer srv.Close()
	if _, err := Poll(srv.Client(), Spec{Poll: srv.URL, Select: "/pad"}, "", "r", nil); err == nil {
		t.Fatal("oversized select body should be an error")
	}
}

func TestPollRawBodyCap(t *testing.T) {
	big := strings.Repeat("x", hashBodyCap+1)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Write([]byte(big))
	}))
	defer srv.Close()
	if _, err := Poll(srv.Client(), Spec{Poll: srv.URL}, "", "r", nil); err == nil {
		t.Fatal("oversized raw body should be an error")
	}
}

func TestStateRoundTripAndDurations(t *testing.T) {
	dir := t.TempDir()
	st := &State{Routine: "inbox", Value: "abc", ETag: `"e"`}
	if err := st.Save(dir); err != nil {
		t.Fatal(err)
	}
	got, err := Load(dir, "inbox")
	if err != nil || got == nil || got.Value != "abc" || got.ETag != `"e"` {
		t.Fatalf("round trip: %+v err=%v", got, err)
	}
	if none, err := Load(dir, "missing"); err != nil || none != nil {
		t.Fatalf("missing state should be nil, nil: %+v, %v", none, err)
	}

	if d, _ := (Spec{}).IntervalDuration(); d != DefaultInterval {
		t.Fatalf("default interval: %v", d)
	}
	if d, _ := (Spec{Interval: "90s"}).IntervalDuration(); d != 90*time.Second {
		t.Fatalf("explicit interval: %v", d)
	}
}
