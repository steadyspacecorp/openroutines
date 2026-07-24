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
	bad := []Spec{
		{},
		{Poll: "ftp://example.com"},
		{Poll: "not a url"},
		{Poll: "https://example.com", Select: "no-slash"},
		{Poll: "https://example.com", Select: "/x~2y"},
		{Poll: "https://example.com", Interval: "often"},
	}
	for i, s := range bad {
		if err := s.Validate(); err == nil {
			t.Errorf("bad spec %d accepted: %+v", i, s)
		}
	}
}

// pollServer serves a body (mutable) and counts conditional hits.
type pollServer struct {
	body   string
	etag   string
	status int
	got304 int
	auth   string
}

func (p *pollServer) handler(w http.ResponseWriter, r *http.Request) {
	p.auth = r.Header.Get("Authorization")
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

	// First sight: baseline, never a change.
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

	// Same value: no change.
	prior := res.Next
	res, err = Poll(srv.Client(), spec, "", "r", &prior)
	if err != nil || res.Changed {
		t.Fatalf("unchanged poll: changed=%v err=%v", res.Changed, err)
	}

	// New message: change.
	ps.body = `{"messages":[{"ts":"200.2"}]}`
	res, err = Poll(srv.Client(), spec, "", "r", &prior)
	if err != nil || !res.Changed || res.Next.Value != "200.2" {
		t.Fatalf("changed poll: %+v err=%v", res, err)
	}

	// Empty channel: absent pointer observes "", a change from "200.2".
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

	// Same ETag: served as 304, no change, no body compare.
	res, err = Poll(srv.Client(), spec, "", "r", &prior)
	if err != nil || res.Changed || ps.got304 != 1 {
		t.Fatalf("304 path: changed=%v got304=%d err=%v", res.Changed, ps.got304, err)
	}

	// Content and ETag change together: fire.
	ps.body, ps.etag = "v2", `"e2"`
	res, err = Poll(srv.Client(), spec, "", "r", &prior)
	if err != nil || !res.Changed {
		t.Fatalf("change path: changed=%v err=%v", res.Changed, err)
	}
	if res.Next.Value == prior.Value {
		t.Fatal("hash should differ for new body")
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
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte(big))
	}))
	defer srv.Close()
	if _, err := Poll(srv.Client(), Spec{Poll: srv.URL, Select: "/pad"}, "", "r", nil); err == nil {
		t.Fatal("oversized select body should be an error")
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
