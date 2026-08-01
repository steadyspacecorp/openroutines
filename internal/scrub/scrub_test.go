package scrub

import (
	"bytes"
	"strings"
	"testing"
)

func TestRedactsSecretsAcrossWrites(t *testing.T) {
	Register(map[string]string{"api_token": "s3cr3t-value"})
	var out bytes.Buffer
	w := NewWriter(&out)
	w.Write([]byte("token is s3cr3t-"))
	w.Write([]byte("value ok\npartial tail"))
	w.Flush()
	got := out.String()
	if strings.Contains(got, "s3cr3t-value") {
		t.Fatalf("secret leaked: %q", got)
	}
	if !strings.Contains(got, "[REDACTED:API_TOKEN]") {
		t.Fatalf("redaction marker missing: %q", got)
	}
	if !strings.Contains(got, "partial tail") {
		t.Fatalf("flushed tail missing: %q", got)
	}
}

// Newline-free output must not grow the buffer without bound: past the cap
// it flushes through redaction in chunks.
func TestBufferCapOnNewlineFreeOutput(t *testing.T) {
	var out bytes.Buffer
	w := NewWriter(&out)
	chunk := bytes.Repeat([]byte("x"), 256<<10)
	for i := 0; i < 8; i++ {
		w.Write(chunk)
	}
	if w.buf.Len() > maxBuffered {
		t.Fatalf("buffer grew past the cap: %d", w.buf.Len())
	}
	if out.Len() == 0 {
		t.Fatal("capped buffer should have flushed to the destination")
	}
}

// Ephemeral entries under one name coexist, and releasing one leaves the
// others redacting -- the concurrent-runs contract.
func TestEphemeralRegistrationsCoexistAndRelease(t *testing.T) {
	release1 := RegisterEphemeral("bearer (desk)", "eph-value-one")
	release2 := RegisterEphemeral("bearer (desk)", "eph-value-two")
	for _, v := range []string{"eph-value-one", "eph-value-two"} {
		if got := Redacted(v); got != "[REDACTED:BEARER (DESK)]" {
			t.Fatalf("live ephemeral %q must redact, got %q", v, got)
		}
	}
	release1()
	if got := Redacted("eph-value-one"); got != "eph-value-one" {
		t.Fatalf("released value must stop redacting, got %q", got)
	}
	if got := Redacted("eph-value-two"); got != "[REDACTED:BEARER (DESK)]" {
		t.Fatalf("release must remove only its own entry, got %q", got)
	}
	release2()
}

func TestEmptySecretNeverRedacts(t *testing.T) {
	Register(map[string]string{"empty": ""})
	if got := Redacted("hello"); got != "hello" {
		t.Fatalf("unexpected rewrite: %q", got)
	}
}
