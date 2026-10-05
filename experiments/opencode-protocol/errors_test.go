package gateway

import (
	"errors"
	"os"
	"strings"
	"testing"
)

func TestStatusErrorMapping(t *testing.T) {
	cases := []struct {
		name     string
		status   int
		fixture  string
		category ErrorCategory
		retry    bool
	}{
		{"openai rate limit", 429, "error-openai-429.json", CategoryRateLimit, true},
		{"anthropic bad request", 400, "error-anthropic-400.json", CategoryBadRequest, false},
		{"gemini auth failure", 400, "error-gemini-400.json", CategoryAuthentication, false},
		{"quota envelope", 402, "error-200-envelope.json", CategoryQuota, false},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			body, err := os.ReadFile("testdata/" + c.fixture)
			if err != nil {
				t.Fatal(err)
			}
			err = CheckStatus(c.status, body)
			var perr *ProviderError
			if !errors.As(err, &perr) {
				t.Fatalf("expected *ProviderError, got %v", err)
			}
			if perr.Category != c.category {
				t.Fatalf("category = %s, want %s (%s)", perr.Category, c.category, perr.Message)
			}
			if perr.Retryable != c.retry {
				t.Fatalf("retryable = %t, want %t", perr.Retryable, c.retry)
			}
		})
	}
}

func TestBareStatusMappingWithoutBody(t *testing.T) {
	cases := []struct {
		status   int
		category ErrorCategory
	}{
		{401, CategoryAuthentication},
		{404, CategoryModelNotFound},
		{429, CategoryRateLimit},
		{500, CategoryProviderOutage},
		{503, CategoryProviderOutage},
		{418, CategoryBadRequest},
	}
	for _, c := range cases {
		perr := NormalizeError(c.status, nil)
		if perr.Category != c.category {
			t.Fatalf("status %d: category = %s, want %s", c.status, perr.Category, c.category)
		}
	}
}

func TestContextLengthFromFields(t *testing.T) {
	body := `{"error":{"message":"This model's maximum context length is 200000 tokens.","type":"invalid_request_error","code":"context_length_exceeded"}}`
	perr := NormalizeError(400, []byte(body))
	if perr.Category != CategoryContextLength {
		t.Fatalf("category = %s", perr.Category)
	}
}

func TestNoCredentialMaterialInErrorsOrLogs(t *testing.T) {
	secrets := []string{
		"sk-ant-testsecretkey1234567890",
		"sk-test-openai-secret-abcdef123456",
		"Bearer opencode-test-token-xyz987654321",
		"api_key=testsecretvalue123",
		"x-api-key: test-header-secret-999",
		"https://example.test/v1/chat?key=testquerysecret123",
	}
	for _, secret := range secrets {
		body := `{"error":{"message":"request failed with credential ` + secret + ` rejected","type":"authentication_error"}}`
		perr := NormalizeError(401, []byte(body))
		rendered := perr.Error() + "|" + perr.Message + "|" + perr.ProviderCode
		for _, frag := range []string{"testsecretkey1234567890", "testsecretvalue123", "test-header-secret-999", "testquerysecret123", "test-token-xyz987654321", "abcdef123456"} {
			if strings.Contains(rendered, frag) {
				t.Fatalf("credential fragment leaked for input %q: %q", secret, rendered)
			}
		}
		if !strings.Contains(rendered, "[REDACTED]") {
			t.Fatalf("expected redaction marker for input %q: %q", secret, rendered)
		}
	}
	// Plain diagnostic text without credential shapes survives redaction.
	if got := Redact("model_not_found: no such model gpt-9"); got != "model_not_found: no such model gpt-9" {
		t.Fatalf("over-redaction: %q", got)
	}
}
