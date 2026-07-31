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

func TestEmptySecretNeverRedacts(t *testing.T) {
	Register(map[string]string{"empty": ""})
	if got := Redacted("hello"); got != "hello" {
		t.Fatalf("unexpected rewrite: %q", got)
	}
}
