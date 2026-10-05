package export

import (
	"bytes"
	"context"
	"path/filepath"
	"testing"

	"j0s.at/vibeshell/internal/domain"
)

func TestRedactionPreservesExactPayloadWhenNothingMatches(t *testing.T) {
	rd := newRedactor(domain.DefaultRedactionPolicy())
	payload := []byte(`{"command":"ls -la","count":3}`)
	rec := record{
		env:     domain.EventEnvelope{Payload: domain.PayloadRef{Inline: append([]byte(nil), payload...)}},
		payload: payload,
	}
	got := rd.apply(rec)
	if !bytes.Equal(got.payload, payload) {
		t.Fatalf("payload changed without a secret: %q", got.payload)
	}
	if !bytes.Equal(got.env.Payload.Inline, payload) {
		t.Fatalf("inline envelope payload changed without a secret: %q", got.env.Payload.Inline)
	}
}

func TestRedactionScrubsTheEnvelopeInlinePayload(t *testing.T) {
	const secret = "sk-abcdefghijklmnopqrstuvwxyz012345"
	rd := newRedactor(domain.DefaultRedactionPolicy())
	inline := []byte(`{"action":"command","command":"export TOKEN=` + secret + `"}`)
	rec := record{
		env:     domain.EventEnvelope{Kind: domain.EventKindInputAccepted, Payload: domain.PayloadRef{Inline: append([]byte(nil), inline...)}},
		payload: inline,
	}
	got := rd.apply(rec)
	if bytes.Contains(got.env.Payload.Inline, []byte(secret)) {
		t.Fatal("secret survived in the envelope inline payload")
	}
	if bytes.Contains(got.payload, []byte(secret)) {
		t.Fatal("secret survived in the redacted payload")
	}
	if !bytes.Contains(got.payload, []byte("[REDACTED]")) {
		t.Fatal("expected a redaction marker")
	}
}

func TestRedactionPolicyCanReplaceModelAndUserPayloads(t *testing.T) {
	rd := newRedactor(domain.RedactionPolicy{RedactPayloads: true, RedactUserInput: true, ReplacementText: "REDACTED-X"})
	model := record{env: domain.EventEnvelope{Kind: domain.EventKindModelResponse}, payload: []byte(`{"text":"hello"}`)}
	if got := rd.apply(model); !bytes.Contains(got.payload, []byte("REDACTED-X")) {
		t.Fatalf("model payload not replaced: %q", got.payload)
	}
	input := record{env: domain.EventEnvelope{Kind: domain.EventKindInputAccepted}, payload: []byte(`{"command":"hello"}`)}
	if got := rd.apply(input); !bytes.Contains(got.payload, []byte("REDACTED-X")) {
		t.Fatalf("user input not replaced: %q", got.payload)
	}
}

func TestExportFailsWhenReferencedBlobIsMissing(t *testing.T) {
	f := canonicalFixture(t, true, "")
	f.content.mu.Lock()
	f.content.blobs = map[string][]byte{}
	f.content.mu.Unlock()
	_, err := f.service(t, Options{}).Export(context.Background(),
		sessionQuery(FormatJSONL, filepath.Join(t.TempDir(), "bundle"), f.session))
	if err == nil {
		t.Fatal("expected an error for a missing content blob")
	}
	if !domain.IsNotFoundError(err) {
		t.Fatalf("err = %v, want not-found", err)
	}
}
