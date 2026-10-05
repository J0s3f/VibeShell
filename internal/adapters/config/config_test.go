package config

import (
	"strings"
	"testing"
)

// minimalDoc is the smallest configuration document that passes every
// semantic rule, so each test can break exactly one rule.
const minimalDoc = `{
  "version": 1,
  "identity": {"system_name": "VibeOS", "shell_name": "VibeShell", "hostname": "testhost"},
  "ssh": {"listen_port": 2222, "host_key_file": "/etc/vibeshell/host_key"},
  "auth": {"mode": "public"},
  "sharing": {"enabled": false},
  "routes": [
    {"id": "rte_0123456789ABCDEFGHJKMNPQRS", "provider": "opencode", "product": "console", "model": "m1"}
  ],
  "tiers": [
    {"name": "t1", "routes": ["rte_0123456789ABCDEFGHJKMNPQRS"]}
  ],
  "persistence": {"database_path": "/var/lib/vibeshell/world.db"}
}`

func parseProblems(doc string) []Problem {
	_, err := Parse([]byte(doc), Options{})
	if err == nil {
		return nil
	}
	ve, ok := err.(*ValidationError)
	if !ok {
		return []Problem{{Message: err.Error()}}
	}
	return ve.Problems
}

func requireProblem(t *testing.T, doc, wantSub string) {
	t.Helper()
	problems := parseProblems(doc)
	if len(problems) == 0 {
		t.Fatalf("expected a validation problem containing %q, got none", wantSub)
	}
	for _, p := range problems {
		if strings.Contains(p.Message, wantSub) || strings.Contains(p.String(), wantSub) {
			return
		}
	}
	t.Fatalf("expected a problem containing %q, got %v", wantSub, problems)
}

func TestParseAcceptsMinimal(t *testing.T) {
	if _, err := Parse([]byte(minimalDoc), Options{}); err != nil {
		t.Fatalf("minimal document rejected: %v", err)
	}
}

func TestParseRejectsUnknownField(t *testing.T) {
	doc := strings.Replace(minimalDoc, `"auth":`, `"bogus": 1, "auth":`, 1)
	requireProblem(t, doc, `unknown field "bogus"`)
}

func TestParseRejectsDuplicateKey(t *testing.T) {
	doc := strings.Replace(minimalDoc, `"version": 1,`, `"version": 1, "version": 1,`, 1)
	requireProblem(t, doc, `duplicate key "version"`)
}

func TestParseRejectsBadEnum(t *testing.T) {
	doc := strings.Replace(minimalDoc, `"mode": "public"`, `"mode": "yolo"`, 1)
	requireProblem(t, doc, `must be one of`)
}

func TestParseRejectsDuplicateRouteIDs(t *testing.T) {
	doc := strings.Replace(minimalDoc,
		`{"id": "rte_0123456789ABCDEFGHJKMNPQRS", "provider": "opencode", "product": "console", "model": "m1"}`,
		`{"id": "rte_0123456789ABCDEFGHJKMNPQRS", "provider": "opencode", "product": "console", "model": "m1"},
     {"id": "rte_0123456789ABCDEFGHJKMNPQRS", "provider": "opencode", "product": "console", "model": "m2"}`, 1)
	requireProblem(t, doc, `duplicate route id`)
}

func TestParseRejectsUnknownRoutePurpose(t *testing.T) {
	doc := strings.Replace(minimalDoc,
		`{"id": "rte_0123456789ABCDEFGHJKMNPQRS", "provider": "opencode", "product": "console", "model": "m1"}`,
		`{"id": "rte_0123456789ABCDEFGHJKMNPQRS", "provider": "opencode", "product": "console", "model": "m1", "purposes": ["bogus"]}`, 1)
	requireProblem(t, doc, `purposes[0]`)
}

func TestParseAcceptsRoutePurpose(t *testing.T) {
	doc := strings.Replace(minimalDoc,
		`{"id": "rte_0123456789ABCDEFGHJKMNPQRS", "provider": "opencode", "product": "console", "model": "m1"}`,
		`{"id": "rte_0123456789ABCDEFGHJKMNPQRS", "provider": "opencode", "product": "console", "model": "m1", "purposes": ["motd"]}`, 1)
	cfg, err := Parse([]byte(doc), Options{})
	if err != nil {
		t.Fatalf("valid purpose rejected: %v", err)
	}
	if len(cfg.Routes) != 1 || len(cfg.Routes[0].Purposes) != 1 || cfg.Routes[0].Purposes[0] != "motd" {
		t.Fatalf("purposes not loaded: %+v", cfg.Routes)
	}
}

func TestParseRejectsDuplicateAccountIDs(t *testing.T) {
	doc := strings.Replace(minimalDoc, `"persistence":`, `"accounts": [
    {"id": "acc_0123456789ABCDEFGHJKMNPQRS", "quota_group": "g", "secret_ref": "{env:K1}"},
    {"id": "acc_0123456789ABCDEFGHJKMNPQRS", "quota_group": "g", "secret_ref": "{env:K2}"}
  ], "persistence":`, 1)
	requireProblem(t, doc, `duplicate account id`)
}

func TestParseRejectsMissingAuthMode(t *testing.T) {
	doc := strings.Replace(minimalDoc, `"mode": "public"`, ``, 1)
	requireProblem(t, doc, `auth.mode`)
}

// --- provider kinds and credential-less accounts ---------------------------

// withProviders injects a providers block before minimalDoc's routes (which
// reference provider "opencode"), so each test can declare exactly the
// provider shapes it needs while keeping the rest of the document valid.
func withProviders(doc, providers string) string {
	return strings.Replace(doc, `"routes":`, `"providers": `+providers+`, "routes":`, 1)
}

// withAccounts injects an accounts block into minimalDoc.
func withAccounts(doc, accounts string) string {
	return strings.Replace(doc, `"persistence":`, `"accounts": `+accounts+`, "persistence":`, 1)
}

const cliProviders = `[
  {"name": "opencode", "kind": "cli", "products": [
    {"name": "console", "protocols": ["chat"]}
  ]}
]`

const httpProviders = `[
  {"name": "opencode", "products": [
    {"name": "console", "base_url": "https://console.example.com/v1", "protocols": ["chat"]}
  ]}
]`

func TestParseAcceptsCLIProviderWithoutBaseURL(t *testing.T) {
	doc := withProviders(minimalDoc, cliProviders)
	if _, err := Parse([]byte(doc), Options{}); err != nil {
		t.Fatalf("cli provider without base_url rejected: %v", err)
	}
}

func TestParseRejectsUnknownProviderKind(t *testing.T) {
	providers := `[
  {"name": "opencode", "kind": "grpc", "products": [
    {"name": "console", "base_url": "https://console.example.com/v1", "protocols": ["chat"]}
  ]}
]`
	requireProblem(t, withProviders(minimalDoc, providers), `providers[0].kind`)
}

func TestParseValidatesBaseURLByProviderKind(t *testing.T) {
	t.Run("cli base_url is a binary path", func(t *testing.T) {
		providers := `[
  {"name": "opencode", "kind": "cli", "products": [
    {"name": "console", "base_url": "not a url", "protocols": ["chat"]}
  ]}
]`
		if _, err := Parse([]byte(withProviders(minimalDoc, providers)), Options{}); err != nil {
			t.Fatalf("cli binary path rejected: %v", err)
		}
	})
	t.Run("http base_url must be an https URL", func(t *testing.T) {
		providers := `[
  {"name": "opencode", "products": [
    {"name": "console", "base_url": "not a url", "protocols": ["chat"]}
  ]}
]`
		requireProblem(t, withProviders(minimalDoc, providers), `providers[0].products[0].base_url`)
	})
}

func TestParseRejectsCLIProviderWithNonChatProtocol(t *testing.T) {
	providers := `[
  {"name": "opencode", "kind": "cli", "products": [
    {"name": "console", "protocols": ["responses"]}
  ]}
]`
	requireProblem(t, withProviders(minimalDoc, providers), `providers[0].products[0].protocols`)
}

func TestParseAcceptsCredentialLessAccountForCLIProduct(t *testing.T) {
	doc := withAccounts(withProviders(minimalDoc, cliProviders),
		`[{"id": "acc_0123456789ABCDEFGHJKMNPQRS", "quota_group": "g", "permitted_products": ["console"]}]`)
	if _, err := Parse([]byte(doc), Options{}); err != nil {
		t.Fatalf("credential-less account for a cli product rejected: %v", err)
	}
}

func TestParseRejectsCredentialLessAccountForHTTPProduct(t *testing.T) {
	doc := withAccounts(withProviders(minimalDoc, httpProviders),
		`[{"id": "acc_0123456789ABCDEFGHJKMNPQRS", "quota_group": "g", "permitted_products": ["opencode/console"]}]`)
	requireProblem(t, doc, `accounts[0].secret_ref`)
}

func TestParseRejectsCredentialLessAccountWhenProductIsAlsoHTTP(t *testing.T) {
	// A bare product name is credential-less-eligible only when every
	// provider declaring that name runs a CLI.
	providers := `[
  {"name": "opencode", "kind": "cli", "products": [
    {"name": "console", "protocols": ["chat"]}
  ]},
  {"name": "opencode-go", "products": [
    {"name": "console", "base_url": "https://go.example.com/v1", "protocols": ["chat"]}
  ]}
]`
	doc := withAccounts(withProviders(minimalDoc, providers),
		`[{"id": "acc_0123456789ABCDEFGHJKMNPQRS", "quota_group": "g", "permitted_products": ["console"]}]`)
	requireProblem(t, doc, `accounts[0].secret_ref`)
}

func TestParseRejectsCredentialLessAccountWithoutProducts(t *testing.T) {
	doc := withAccounts(minimalDoc,
		`[{"id": "acc_0123456789ABCDEFGHJKMNPQRS", "quota_group": "g"}]`)
	requireProblem(t, doc, `accounts[0].secret_ref`)
}
