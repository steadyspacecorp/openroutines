package scrub

import (
	"bytes"
	"strings"
	"testing"
)

func TestRedactsSecretsAcrossWrites(t *testing.T) {
	var out bytes.Buffer
	w := NewWriter(&out, map[string]string{"api_token": "s3cr3t-value"})
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

func TestEmptySecretNeverRedacts(t *testing.T) {
	var out bytes.Buffer
	w := NewWriter(&out, map[string]string{"empty": ""})
	w.Write([]byte("hello\n"))
	w.Flush()
	if out.String() != "hello\n" {
		t.Fatalf("unexpected rewrite: %q", out.String())
	}
}
