// Package observability provides operational logging, metrics, and status reporting
// for VibeShell.
package observability

import (
	"fmt"
	"log/slog"
	"testing"
)

func TestRedactor_BearerToken(t *testing.T) {
	r := NewRedactor()
	testCases := []struct {
		input    string
		expected string
	}{
		{"Authorization: Bearer abc123", "Authorization: Bearer [REDACTED]"},
		{"Bearer sk-abc123def456", "Bearer [REDACTED]"},
		{"bearer token123", "bearer [REDACTED]"},
		{"BEARER secret", "BEARER [REDACTED]"},
		// Single-quoted curl command: pattern matches up to whitespace, so quote remains
		{"curl -H 'Authorization: Bearer mytoken'", "curl -H 'Authorization: Bearer [REDACTED]"},
	}

	for _, tc := range testCases {
		t.Run(tc.input, func(t *testing.T) {
			result := r.RedactString(tc.input)
			if result != tc.expected {
				t.Errorf("got %q, expected %q", result, tc.expected)
			}
		})
	}
}

func TestRedactor_SkKeys(t *testing.T) {
	r := NewRedactor()
	testCases := []struct {
		input    string
		expected string
	}{
		{"sk-abc123def456ghi789jkl", "sk-[REDACTED]"},  // 24 chars after sk-
		{"sk-short", "sk-short"},                       // too short
		{"sk-abc123def456ghi789jkl0", "sk-[REDACTED]"}, // 25 chars
		{"My key is sk-abc123def456ghi789jkl here", "My key is sk-[REDACTED] here"},
		{"sk_abc123def456ghi789jkl", "sk_abc123def456ghi789jkl"}, // underscore not matched
	}

	for _, tc := range testCases {
		t.Run(tc.input, func(t *testing.T) {
			result := r.RedactString(tc.input)
			if result != tc.expected {
				t.Errorf("got %q, expected %q", result, tc.expected)
			}
		})
	}
}

func TestRedactor_QueryParams(t *testing.T) {
	r := NewRedactor()
	testCases := []struct {
		input    string
		expected string
	}{
		{"https://api.example.com?api_key=secret123", "https://api.example.com?api_key=[REDACTED]"},
		{"https://api.example.com?key=secret123", "https://api.example.com?key=[REDACTED]"},
		{"https://api.example.com?foo=bar&api_key=secret&baz=qux", "https://api.example.com?foo=bar&api_key=[REDACTED]&baz=qux"},
		{"https://api.example.com?password=hunter2", "https://api.example.com?password=[REDACTED]"},
		{"https://api.example.com?secret=mysecret", "https://api.example.com?secret=[REDACTED]"},
		{"https://api.example.com?access_token=atok&refresh_token=rtok", "https://api.example.com?access_token=[REDACTED]&refresh_token=[REDACTED]"},
		{"https://api.example.com?client_secret=csec", "https://api.example.com?client_secret=[REDACTED]"},
	}

	for _, tc := range testCases {
		t.Run(tc.input, func(t *testing.T) {
			result := r.RedactString(tc.input)
			if result != tc.expected {
				t.Errorf("got %q, expected %q", result, tc.expected)
			}
		})
	}
}

func TestRedactor_Headers(t *testing.T) {
	r := NewRedactor()
	testCases := []struct {
		input    string
		expected string
	}{
		{"X-Api-Key: secret123", "X-Api-Key: [REDACTED]"},
		{"x-api-key: secret123", "x-api-key: [REDACTED]"},
		// Basic auth not currently redacted (no pattern for it)
		{"Authorization: Basic dXNlcjpwYXNz", "Authorization: Basic dXNlcjpwYXNz"},
		// Bearer tokens show "Bearer [REDACTED]" prefix
		{"Authorization: Bearer token123", "Authorization: Bearer [REDACTED]"},
	}

	for _, tc := range testCases {
		t.Run(tc.input, func(t *testing.T) {
			result := r.RedactString(tc.input)
			if result != tc.expected {
				t.Errorf("got %q, expected %q", result, tc.expected)
			}
		})
	}
}

func TestRedactor_AWSKeys(t *testing.T) {
	r := NewRedactor()
	testCases := []struct {
		input    string
		expected string
	}{
		{"AKIAIOSFODNN7EXAMPLE", "AKIA[REDACTED]"},
		{"AKIA1234567890123456", "AKIA[REDACTED]"},
	}

	for _, tc := range testCases {
		t.Run(tc.input, func(t *testing.T) {
			result := r.RedactString(tc.input)
			if result != tc.expected {
				t.Errorf("got %q, expected %q", result, tc.expected)
			}
		})
	}
}

func TestRedactor_LongTokens(t *testing.T) {
	r := NewRedactor()
	// 40+ char alphanum tokens get redacted with 4-char prefix preserved
	longToken := "aBcDeFgHiJkLmNoPqRsTuVwXyZ1234567890+/_-"
	testCases := []struct {
		input    string
		expected string
	}{
		// First 4 chars "aBcD" preserved
		{longToken, "aBcD[REDACTED]"},
		// Pattern matches "pref" from "prefix_" (first 4 chars of 40+ sequence)
		{"prefix_" + longToken + "_suffix", "pref[REDACTED]"},
		{"short", "short"}, // 5 chars - not redacted
		// "exactly40chars12345678901234567890" is only 33 chars, not 40+
		{"exactly40chars12345678901234567890", "exactly40chars12345678901234567890"},
		// A truly 40+ char alphanumeric string (44 chars)
		{"abcdefghijklmnopqrstuvwxyz12345678901234", "abcd[REDACTED]"},
	}

	for _, tc := range testCases {
		t.Run(tc.input, func(t *testing.T) {
			result := r.RedactString(tc.input)
			if result != tc.expected {
				t.Errorf("got %q, expected %q", result, tc.expected)
			}
		})
	}
}

func TestRedactor_ZeroLeakage(t *testing.T) {
	r := NewRedactor()

	// Test that no secret fragments leak through
	secrets := []string{
		"sk-abc123def456ghi789jkl",
		"Bearer mysecrettoken",
		"api_key=supersecret",
		"key=myapikey123",
		"X-Api-Key: headersecret",
		"Authorization: Bearer token123",
		"password=hunter2",
		"secret=mysecret",
		"access_token=atok123",
		"refresh_token=rtok456",
		"client_secret=csec789",
		"AKIAIOSFODNN7EXAMPLE",
	}

	for _, secret := range secrets {
		t.Run(secret, func(t *testing.T) {
			// Embed secret in various contexts
			contexts := []string{
				secret,
				"prefix " + secret,
				secret + " suffix",
				"before " + secret + " after",
				"{\"key\": \"" + secret + "\"}", // JSON-like
				"key=" + secret + "&other=value",
			}

			for _, ctx := range contexts {
				redacted := r.RedactString(ctx)
				// Check that no part of the secret remains
				// (We check for characteristic fragments)
				forbidden := []string{
					"sk-abc123",
					"mysecrettoken",
					"supersecret",
					"myapikey123",
					"headersecret",
					"token123",
					"hunter2",
					"mysecret",
					"atok123",
					"rtok456",
					"csec789",
					"AKIAIOSFODNN7EXAMPLE",
				}
				for _, frag := range forbidden {
					if len(frag) > 5 && contains(redacted, frag) {
						t.Errorf("secret fragment %q leaked in %q -> %q", frag, ctx, redacted)
					}
				}
			}
		})
	}
}

func TestRedactor_RedactAttrs(t *testing.T) {
	r := NewRedactor()
	attrs := []slog.Attr{
		slog.String("url", "https://api.example.com?api_key=secret123"),
		slog.String("header", "Authorization: Bearer token"),
		slog.Int("count", 42),
		slog.Bool("enabled", true),
	}
	redacted := r.RedactAttrs(attrs)

	if len(redacted) != 4 {
		t.Fatalf("expected 4 attrs, got %d", len(redacted))
	}

	// String values should be redacted
	if redacted[0].Value.String() != "https://api.example.com?api_key=[REDACTED]" {
		t.Errorf("url not redacted: %s", redacted[0].Value.String())
	}
	// Bearer tokens show "Bearer [REDACTED]" prefix
	if redacted[1].Value.String() != "Authorization: Bearer [REDACTED]" {
		t.Errorf("header not redacted: %s", redacted[1].Value.String())
	}

	// Non-string values should be preserved
	if redacted[2].Value.Int64() != 42 {
		t.Errorf("int not preserved: %d", redacted[2].Value.Int64())
	}
	if redacted[3].Value.Bool() != true {
		t.Errorf("bool not preserved: %v", redacted[3].Value.Bool())
	}
}

func TestRedactor_RedactMap(t *testing.T) {
	r := NewRedactor()
	m := map[string]any{
		"url":     "https://api.example.com?api_key=secret123",
		"count":   42,
		"enabled": true,
	}
	redacted := r.RedactMap(m)

	if redacted["url"] != "https://api.example.com?api_key=[REDACTED]" {
		t.Errorf("url not redacted: %v", redacted["url"])
	}
	if redacted["count"] != 42 {
		t.Errorf("count not preserved: %v", redacted["count"])
	}
	if redacted["enabled"] != true {
		t.Errorf("enabled not preserved: %v", redacted["enabled"])
	}
}

func TestRedactor_RedactError(t *testing.T) {
	r := NewRedactor()
	err := r.RedactError(fmt.Errorf("failed: api_key=secret123"))
	if err == nil {
		t.Fatal("expected error")
	}
	if err.Error() != "failed: api_key=[REDACTED]" {
		t.Errorf("error not redacted: %s", err.Error())
	}

	// Nil error should return nil
	if r.RedactError(nil) != nil {
		t.Error("nil error should return nil")
	}
}

func TestRedactor_ContainsSecret(t *testing.T) {
	r := NewRedactor()
	testCases := []struct {
		input     string
		hasSecret bool
	}{
		{"hello world", false},
		{"api_key=secret", true},
		{"sk-abc123def456ghi789jkl", true},
		{"Bearer token", true},
		{"no secrets here", false},
	}

	for _, tc := range testCases {
		t.Run(tc.input, func(t *testing.T) {
			result := r.ContainsSecret(tc.input)
			if result != tc.hasSecret {
				t.Errorf("expected %v, got %v", tc.hasSecret, result)
			}
		})
	}
}

func TestRedactor_JoinRedacted(t *testing.T) {
	r := NewRedactor()
	result := r.JoinRedacted(" ",
		"url=https://api.example.com?key=secret1",
		"auth=Bearer token2",
		"normal=value",
	)
	expected := "url=https://api.example.com?key=[REDACTED] auth=Bearer [REDACTED] normal=value"
	if result != expected {
		t.Errorf("got %q, expected %q", result, expected)
	}
}

// contains is a helper for substring check.
func contains(s, substr string) bool {
	for i := 0; i+len(substr) <= len(s); i++ {
		if s[i:i+len(substr)] == substr {
			return true
		}
	}
	return false
}
