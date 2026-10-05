package config

import (
	"fmt"
	"math"
	"net/url"
	"path"
	"regexp"
	"sort"
	"strings"
	"unicode"
	"unicode/utf8"

	"j0s.at/vibeshell/internal/domain"
)

// Parse strictly decodes one configuration document and validates it
// semantically. Structural problems (unknown fields, duplicate keys, wrong
// scalar kinds, trailing content) are reported before semantic ones, and no
// I/O happens here: referenced files and secret values are checked by
// Loader, so Parse is safe to call from tests and offline validation.
func Parse(raw []byte, opts Options) (*Config, error) {
	var cfg Config
	present, problems := decodeStrict(raw, &cfg)
	if len(problems) == 0 {
		problems = validateConfig(&cfg, present, opts)
	}
	if len(problems) > 0 {
		return nil, &ValidationError{Problems: problems}
	}
	return &cfg, nil
}

// Bounded values for the PLAN 11 groups. They are upper/lower validation
// bounds, not shipped defaults: default operational numbers are recorded in
// docs/configuration.md once the owning tasks calibrate them.
const (
	maxTiers         = 8
	maxSecretRefLen  = 4096
	maxPromptPathLen = 4096
	maxFilePathLen   = 4096
	maxModelIDLen    = 255
	maxDaysRetention = 3650
	oneDayMs         = 86_400_000
)

var (
	identifierPattern = regexp.MustCompile(`^[a-z][a-z0-9-]{0,31}$`)
	modelPattern      = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._:-]{0,254}$`)
	quotaGroupPattern = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._-]{0,63}$`)
	envNamePattern    = regexp.MustCompile(`^[A-Za-z_][A-Za-z0-9_]{0,127}$`)
)

var (
	supportedProviderKinds = []string{"http", "cli"}
	supportedProtocols     = []string{"chat", "responses", "messages", "gemini"}
	supportedPurposes      = []string{"motd", "generation"}
	supportedAuthModes     = []string{string(AuthModePublic), string(AuthModeSecure)}
	supportedDurability    = []string{"FULL", "NORMAL"}
	supportedCapabils      = []string{"tool_call", "text_input", "text_output"}
	supportedFormats       = []string{"jsonl", "transcript", "asciinema"}
	supportedLogLevels     = []string{"debug", "info", "warn", "error"}
	supportedInstrument    = []string{"off", "logs", "metrics"}
	supportedRetention     = []string{"permanent", "days"}
	supportedCostPolicy    = []string{"deny", "reserve", "allow"}
	supportedRedraw        = []string{"full", "diff"}
	supportedCache         = []string{"off", "session"}
)

// productInfo is the validated view of one declared provider product that
// route cross-references are checked against.
type productInfo struct {
	protocols       map[Protocol]bool
	protocolList    []Protocol
	defaultProtocol Protocol
	// cli marks a product of a cli-kind provider, whose runner ignores
	// secrets; only such products may back a credential-less account.
	cli bool
}

type validator struct {
	cfg     *Config
	present map[string]bool
	opts    Options

	problems []Problem

	providersDeclared bool
	products          map[string]*productInfo // "provider/product"
	productNames      map[string]bool         // bare product names
	cliProductNames   map[string]bool         // bare product names served only by cli providers
	routeIDs          map[string]int          // route ID → first declaring index
	accountIDs        map[string]int          // account ID → first declaring index
	pools             map[string]bool
}

// validateConfig applies every semantic rule of the PLAN 11 groups. Groups
// are visited in dependency order (providers before routes before tiers),
// so cross-reference problems name the referring side.
func validateConfig(cfg *Config, present map[string]bool, opts Options) []Problem {
	v := &validator{
		cfg:             cfg,
		present:         present,
		opts:            opts,
		products:        map[string]*productInfo{},
		productNames:    map[string]bool{},
		cliProductNames: map[string]bool{},
		routeIDs:        map[string]int{},
		accountIDs:      map[string]int{},
		pools:           map[string]bool{},
	}

	v.secretDirs()
	if v.require("version") && cfg.Version != SchemaVersion {
		v.addf("version", "unsupported configuration version %d (this build accepts %d)", cfg.Version, SchemaVersion)
	}
	v.identity()
	v.ssh()
	v.auth()
	v.sharing()
	v.providers()
	v.routes()
	v.accounts()
	v.accountPools()
	v.tiers()
	v.prompts()
	v.world()
	v.apps()
	v.terminal()
	v.discovery()
	v.health()
	v.limits()
	v.inference()
	v.spending()
	v.persistence()
	v.exports()
	v.operations()

	return v.problems
}

// --- validator primitives -------------------------------------------------

func (v *validator) add(path, message string) {
	v.problems = append(v.problems, Problem{Path: path, Message: message})
}

func (v *validator) addf(path, format string, args ...any) {
	v.add(path, fmt.Sprintf(format, args...))
}

func (v *validator) has(path string) bool { return v.present[path] }

// require reports whether path was present, adding a problem when it was
// not. "Missing" and "invalid" stay distinguishable in the diagnostics.
func (v *validator) require(path string) bool {
	if v.has(path) {
		return true
	}
	v.add(path, "required field is missing")
	return false
}

func (v *validator) rangeInt(path string, val, min, max int) {
	if val < min || val > max {
		v.addf(path, "must be between %d and %d (got %d)", min, max, val)
	}
}

func (v *validator) requireInt(path string, val, min, max int) {
	if v.require(path) {
		v.rangeInt(path, val, min, max)
	}
}

func (v *validator) optionalInt(path string, val, min, max int) {
	if v.has(path) {
		v.rangeInt(path, val, min, max)
	}
}

func (v *validator) optionalInt64(path string, val, min, max int64) {
	if v.has(path) {
		if val < min || val > max {
			v.addf(path, "must be between %d and %d (got %d)", min, max, val)
		}
	}
}

func (v *validator) length(path, val string, min, max int) {
	if n := len(val); n < min || n > max {
		v.addf(path, "must be between %d and %d bytes (got %d)", min, max, n)
	}
}

func (v *validator) requireString(path, val string, min, max int) {
	if v.require(path) {
		v.length(path, val, min, max)
	}
}

func (v *validator) optionalString(path, val string, min, max int) {
	if v.has(path) {
		v.length(path, val, min, max)
	}
}

func (v *validator) oneOf(path, val string, allowed []string) {
	for _, a := range allowed {
		if val == a {
			return
		}
	}
	v.addf(path, "must be one of %s (got %q)", quotedList(allowed), val)
}

func (v *validator) requireEnum(path, val string, allowed ...string) {
	if v.require(path) {
		v.oneOf(path, val, allowed)
	}
}

func (v *validator) optionalEnum(path, val string, allowed ...string) {
	if v.has(path) {
		v.oneOf(path, val, allowed)
	}
}

// requireFlag checks that a boolean field was written explicitly. Its zero
// value is a legitimate setting, so only presence can demand it.
func (v *validator) requireFlag(path string) {
	v.require(path)
}

func (v *validator) optionalFloat(path string, val, min, max float64) {
	if v.has(path) {
		if val < min || val > max {
			v.addf(path, "must be between %g and %g (got %g)", min, max, val)
		}
	}
}

func (v *validator) optionalPositive(path string, val, max float64) {
	if v.has(path) {
		if val <= 0 {
			v.addf(path, "must be greater than 0 (got %g)", val)
		} else if val > max {
			v.addf(path, "must be at most %g (got %g)", max, val)
		}
	}
}

// printable rejects names that could escape a display or a path context.
func (v *validator) printable(path, val string) {
	if !utf8.ValidString(val) {
		v.add(path, "must be valid UTF-8")
		return
	}
	for _, r := range val {
		if unicode.IsControl(r) || unicode.IsSpace(r) {
			v.add(path, "must not contain whitespace or control characters")
			return
		}
		if r == '/' || r == '\\' {
			v.add(path, "must not contain path separators")
			return
		}
	}
}

// absolute requires a cleaned absolute container path without NUL bytes.
// Configuration paths use POSIX rules: they name locations inside the Linux
// runtime container, never on the machine that edits the file.
func (v *validator) absolute(field, val string) {
	if strings.ContainsRune(val, 0) {
		v.add(field, "must not contain a NUL byte")
		return
	}
	if !path.IsAbs(val) {
		v.addf(field, "must be an absolute container path (got %q)", val)
	}
}

// relativeOrAbsolute allows a path that the loader resolves against the
// configuration directory; it only forbids NUL bytes at this stage.
func (v *validator) cleanPath(path, val string) {
	if strings.ContainsRune(val, 0) {
		v.add(path, "must not contain a NUL byte")
	}
}

func (v *validator) httpsURL(path, val string) {
	if strings.ContainsRune(val, 0) {
		v.add(path, "must not contain a NUL byte")
		return
	}
	u, err := url.Parse(val)
	if err != nil || u.Host == "" {
		v.addf(path, "must be an absolute https URL (got %q)", val)
		return
	}
	if u.Scheme != "https" {
		v.addf(path, "must use https (got %q)", u.Scheme)
	}
	if u.User != nil {
		v.add(path, "must not embed credentials; use a secret reference")
	}
	if u.RawQuery != "" {
		v.add(path, "must not carry a query string; use a secret reference")
	}
	if u.Fragment != "" {
		v.add(path, "must not carry a fragment")
	}
}

func (v *validator) identifier(path, val string) {
	if !identifierPattern.MatchString(val) {
		v.addf(path, "must match %s (got %q)", identifierPattern, val)
	}
}

// secretRef validates reference syntax and allowed-directory containment;
// reading the value is deferred to the SecretResolver.
func (v *validator) secretRef(path, raw string) {
	ref, err := ParseSecretRef(raw)
	if err != nil {
		v.add(path, err.Error())
		return
	}
	if ref.IsFile() && !fileRefWithinDirs(ref.FilePath(), v.opts.SecretDirs) {
		v.addf(path, "file secret reference %q is outside the allowed secret directories", ref.FilePath())
	}
}

func (v *validator) secretDirs() {
	for i, dir := range v.opts.SecretDirs {
		if !path.IsAbs(dir) || path.Clean(dir) != dir {
			v.addf(fmt.Sprintf("options.secret_dirs[%d]", i), "must be a cleaned absolute container path (got %q)", dir)
		}
	}
}

// --- root and required groups ---------------------------------------------

func (v *validator) identity() {
	c := v.cfg.Identity
	if c == nil {
		v.add("identity", "required group is missing")
		return
	}
	v.requireEnum("identity.system_name", c.SystemName, "VibeOS")
	v.requireEnum("identity.shell_name", c.ShellName, "VibeShell")
	if v.require("identity.hostname") {
		v.length("identity.hostname", c.Hostname, 1, 64)
		v.printable("identity.hostname", c.Hostname)
	}
	v.optionalInt64("identity.presentation_seed", c.PresentationSeed, 0, math.MaxInt64)
}

func (v *validator) ssh() {
	c := v.cfg.SSH
	if c == nil {
		v.add("ssh", "required group is missing")
		return
	}
	v.requireInt("ssh.listen_port", c.ListenPort, 1, 65535)
	if v.require("ssh.host_key_file") {
		v.length("ssh.host_key_file", c.HostKeyFile, 1, maxFilePathLen)
		v.absolute("ssh.host_key_file", c.HostKeyFile)
	}
	if v.has("ssh.listen_address") {
		v.length("ssh.listen_address", c.ListenAddress, 1, 255)
		v.printable("ssh.listen_address", c.ListenAddress)
		if strings.Contains(c.ListenAddress, "://") {
			v.add("ssh.listen_address", "must be a host or IP address, not a URL")
		}
	}
	v.optionalInt("ssh.max_connections", c.MaxConnections, 1, 10_000)
	v.optionalInt("ssh.idle_timeout_ms", c.IdleTimeoutMs, 0, oneDayMs)
	v.optionalInt("ssh.handshake_timeout_ms", c.HandshakeTimeoutMs, 1, 600_000)
	v.optionalInt("ssh.max_terminal_rows", c.MaxTerminalRows, 1, 10_000)
	v.optionalInt("ssh.max_terminal_cols", c.MaxTerminalCols, 1, 10_000)
	v.optionalInt("ssh.max_input_bytes", c.MaxInputBytes, 1, 16_777_216)
}

func (v *validator) auth() {
	c := v.cfg.Auth
	if c == nil {
		v.add("auth", "required group is missing")
		return
	}
	v.requireEnum("auth.mode", string(c.Mode), supportedAuthModes...)
	switch c.Mode {
	case AuthModeSecure:
		if !v.require("auth.password_file") {
			break
		}
		v.length("auth.password_file", c.PasswordFile, 1, maxFilePathLen)
		v.absolute("auth.password_file", c.PasswordFile)
	case AuthModePublic:
		if c.PasswordFile != "" {
			v.add("auth.password_file", "applies only to auth.mode \"secure\"")
		}
	}
	// Recommended admission profile: at most 2 concurrent authentication
	// attempts service-wide and at most 6 password attempts per connection.
	// Those are the values shipped in the example configuration; the bounds
	// here only keep operator-chosen values inside a sane envelope.
	v.optionalInt("auth.max_attempts_per_connection", c.MaxAttemptsPerConnection, 1, 10)
	v.optionalInt("auth.max_concurrent_auth", c.MaxConcurrentAuth, 1, 1024)
	v.optionalInt("auth.failed_attempts_per_minute", c.FailedAttemptsPerMinute, 1, 1_000_000)
}

func (v *validator) sharing() {
	c := v.cfg.Sharing
	if c == nil {
		v.add("sharing", "required group is missing")
		return
	}
	v.requireFlag("sharing.enabled")
	v.optionalInt("sharing.policy_revision", c.PolicyRevision, 1, math.MaxInt32)
}

// --- providers, routes, accounts, pools, tiers -----------------------------

func (v *validator) providers() {
	if !v.has("providers") {
		return
	}
	v.providersDeclared = true
	seenProviders := map[string]int{}
	for i, p := range v.cfg.Providers {
		pp := fmt.Sprintf("providers[%d]", i)
		// An unknown kind is already reported here; the product rules
		// below then fall back to the http defaults, so a typo cannot
		// silently loosen base_url or secret validation.
		v.optionalEnum(pp+".kind", p.Kind, supportedProviderKinds...)
		isCLI := p.Kind == "cli"
		if v.require(pp + ".name") {
			v.length(pp+".name", p.Name, 1, 32)
			v.identifier(pp+".name", p.Name)
			if first, dup := seenProviders[p.Name]; dup {
				v.addf(pp+".name", "duplicate provider name %q (first declared at providers[%d])", p.Name, first)
			} else {
				seenProviders[p.Name] = i
			}
		}
		if !v.require(pp + ".products") {
			continue
		}
		if len(p.Products) == 0 {
			v.add(pp+".products", "at least one product is required")
			continue
		}
		seenProducts := map[string]int{}
		for j, prod := range p.Products {
			pth := fmt.Sprintf("%s.products[%d]", pp, j)
			if v.require(pth + ".name") {
				v.length(pth+".name", prod.Name, 1, 32)
				v.identifier(pth+".name", prod.Name)
				if first, dup := seenProducts[prod.Name]; dup {
					v.addf(pth+".name", "duplicate product name %q (first declared at %s.products[%d])", prod.Name, pp, first)
				} else {
					seenProducts[prod.Name] = j
				}
				v.productNames[prod.Name] = true
				// A bare product name is credential-less-eligible only
				// when every provider declaring it runs a CLI.
				if isCLI {
					if _, declared := v.cliProductNames[prod.Name]; !declared {
						v.cliProductNames[prod.Name] = true
					}
				} else {
					v.cliProductNames[prod.Name] = false
				}
			}
			if isCLI {
				// base_url names the CLI binary path/name, so it is
				// optional (the adapter defaults to "opencode") and is
				// never checked as a URL.
				v.optionalString(pth+".base_url", prod.BaseURL, 1, 2083)
			} else if v.require(pth + ".base_url") {
				v.length(pth+".base_url", prod.BaseURL, 1, 2083)
				v.httpsURL(pth+".base_url", prod.BaseURL)
			}
			if !v.require(pth + ".protocols") {
				continue
			}
			if len(prod.Protocols) == 0 {
				v.add(pth+".protocols", "at least one protocol is required")
				continue
			}
			if isCLI && !cliProtocols(prod.Protocols) {
				v.addf(pth+".protocols", "a cli provider product must declare exactly [\"chat\"] (the CLI returns text only; got %s)",
					quotedList(protocolValues(prod.Protocols)))
			}
			info := &productInfo{protocols: map[Protocol]bool{}, cli: isCLI}
			for k, proto := range prod.Protocols {
				kpth := fmt.Sprintf("%s.protocols[%d]", pth, k)
				if !knownProtocol(proto) {
					v.addf(kpth, "unsupported protocol %q (supported: %s)", proto, quotedList(supportedProtocols))
					continue
				}
				if info.protocols[proto] {
					v.addf(kpth, "duplicate protocol %q", proto)
					continue
				}
				info.protocols[proto] = true
				info.protocolList = append(info.protocolList, proto)
			}
			if prod.DefaultProtocol != "" {
				if !info.protocols[prod.DefaultProtocol] {
					v.addf(pth+".default_protocol", "protocol %q is not supported by this product (supported: %s)",
						prod.DefaultProtocol, quotedList(protocolNames(info)))
				} else {
					info.defaultProtocol = prod.DefaultProtocol
				}
			}
			v.endpointOverrides(pth, prod, info)
			v.products[p.Name+"/"+prod.Name] = info
		}
	}
}

func (v *validator) endpointOverrides(pth string, prod Product, info *productInfo) {
	if !v.has(pth + ".endpoint_overrides") {
		return
	}
	for _, key := range sortedKeys(prod.EndpointOverrides) {
		opth := fmt.Sprintf("%s.endpoint_overrides.%s", pth, key)
		proto := Protocol(key)
		if !knownProtocol(proto) {
			v.addf(opth, "unknown protocol %q (supported: %s)", key, quotedList(supportedProtocols))
		} else if !info.protocols[proto] {
			v.addf(opth, "protocol %q is not in this product's protocols (%s)", key, quotedList(protocolNames(info)))
		}
		v.httpsURL(opth, prod.EndpointOverrides[key])
	}
}

func (v *validator) routes() {
	if !v.has("routes") {
		return
	}
	for i, r := range v.cfg.Routes {
		rp := fmt.Sprintf("routes[%d]", i)
		if v.require(rp + ".id") {
			v.length(rp+".id", r.ID, 1, 64)
			if _, err := domain.ParseRouteID(r.ID); err != nil {
				v.addf(rp+".id", "must be a route identity: %s", err)
			} else if first, dup := v.routeIDs[r.ID]; dup {
				v.addf(rp+".id", "duplicate route id %q (first declared at routes[%d])", r.ID, first)
			} else {
				v.routeIDs[r.ID] = i
			}
		}
		if v.require(rp + ".provider") {
			v.length(rp+".provider", r.Provider, 1, 32)
			v.identifier(rp+".provider", r.Provider)
		}
		if v.require(rp + ".product") {
			v.length(rp+".product", r.Product, 1, 32)
			v.identifier(rp+".product", r.Product)
		}
		if v.require(rp + ".model") {
			v.length(rp+".model", r.Model, 1, maxModelIDLen)
			if !modelPattern.MatchString(r.Model) {
				v.addf(rp+".model", "must match %s (got %q)", modelPattern, r.Model)
			}
		}
		for k, purpose := range r.Purposes {
			v.oneOf(fmt.Sprintf("%s.purposes[%d]", rp, k), purpose, supportedPurposes)
		}
		v.optionalEnum(rp+".protocol", string(r.Protocol), supportedProtocols...)
		v.checkRouteProduct(rp, r)
	}
}

// checkRouteProduct enforces the protocol/model combination rules: the
// route's provider and product must be declared, and the effective protocol
// (route, product default, or the product's only protocol) must be one the
// product supports. This reports unsupported combinations without any
// catalogue lookup or inference call.
func (v *validator) checkRouteProduct(rp string, r Route) {
	if !v.providersDeclared || r.Provider == "" || r.Product == "" {
		return
	}
	info, ok := v.products[r.Provider+"/"+r.Product]
	if !ok {
		v.addf(rp, "references undeclared provider/product \"%s/%s\" (declare it under providers)", r.Provider, r.Product)
		return
	}
	effective := r.Protocol
	if effective == "" {
		effective = info.defaultProtocol
	}
	if effective == "" && len(info.protocolList) == 1 {
		effective = info.protocolList[0]
	}
	if effective == "" {
		v.addf(rp, "must set protocol: product \"%s/%s\" supports several protocols and declares no default_protocol",
			r.Provider, r.Product)
		return
	}
	if !info.protocols[effective] {
		v.addf(rp, "unsupported protocol %q for model %q on product \"%s/%s\" (supported: %s)",
			effective, r.Model, r.Provider, r.Product, quotedList(protocolNames(info)))
	}
}

func (v *validator) accounts() {
	if !v.has("accounts") {
		return
	}
	for i, a := range v.cfg.Accounts {
		ap := fmt.Sprintf("accounts[%d]", i)
		if v.require(ap + ".id") {
			v.length(ap+".id", a.ID, 1, 64)
			if _, err := domain.ParseAccountID(a.ID); err != nil {
				v.addf(ap+".id", "must be an account identity: %s", err)
			} else if first, dup := v.accountIDs[a.ID]; dup {
				v.addf(ap+".id", "duplicate account id %q (first declared at accounts[%d])", a.ID, first)
			} else {
				v.accountIDs[a.ID] = i
			}
		}
		if v.require(ap + ".quota_group") {
			v.length(ap+".quota_group", a.QuotaGroup, 1, 64)
			if !quotaGroupPattern.MatchString(a.QuotaGroup) {
				v.addf(ap+".quota_group", "must match %s (got %q)", quotaGroupPattern, a.QuotaGroup)
			}
		}
		if v.has(ap + ".secret_ref") {
			v.length(ap+".secret_ref", a.SecretRef, 1, maxSecretRefLen)
			v.secretRef(ap+".secret_ref", a.SecretRef)
		} else {
			v.credentialLessAccount(ap, a)
		}
		v.permittedProducts(ap, a)
	}
}

// credentialLessAccount enforces when an account may omit secret_ref: only
// when it permits at least one product and every permitted product is
// served by a cli-kind provider, whose CLI runner ignores secrets. Every
// other account keeps the required reference, and the problem names the
// product that forces it.
func (v *validator) credentialLessAccount(ap string, a Account) {
	if len(a.PermittedProducts) == 0 {
		v.add(ap+".secret_ref", "required field is missing (only an account whose permitted_products are all served by a cli provider may omit it)")
		return
	}
	if !v.providersDeclared {
		v.add(ap+".secret_ref", "required field is missing (no providers are declared, so no product is cli-served)")
		return
	}
	for k, token := range a.PermittedProducts {
		if v.servedByCLI(token) {
			continue
		}
		v.addf(ap+".secret_ref",
			"required field is missing: %s.permitted_products[%d] (%q) is not served by a cli provider, so this account needs a secret reference",
			ap, k, token)
	}
}

// servedByCLI reports whether a permitted-product token resolves only to
// products of cli-kind providers. A bare name is cli-served only when every
// provider declaring that name is cli-kind; an undeclared token is not, so
// a credential-less account fails closed while permittedProducts reports
// the missing declaration itself.
func (v *validator) servedByCLI(token string) bool {
	if info, ok := v.products[token]; ok {
		return info.cli
	}
	return v.cliProductNames[token]
}

func (v *validator) permittedProducts(ap string, a Account) {
	if !v.has(ap + ".permitted_products") {
		return
	}
	seen := map[string]bool{}
	for k, token := range a.PermittedProducts {
		kpth := fmt.Sprintf("%s.permitted_products[%d]", ap, k)
		v.length(kpth, token, 1, 64)
		if strings.ContainsAny(token, " \t\r\n") {
			v.add(kpth, "must not contain whitespace")
			continue
		}
		if seen[token] {
			v.addf(kpth, "duplicate product %q", token)
			continue
		}
		seen[token] = true
		if !v.providersDeclared {
			continue
		}
		if _, declared := v.products[token]; declared {
			continue // fully qualified provider/product
		}
		if v.productNames[token] {
			continue // bare product name
		}
		v.addf(kpth, "references undeclared product %q (declare it under providers)", token)
	}
}

func (v *validator) accountPools() {
	if !v.has("account_pools") {
		return
	}
	for _, name := range sortedKeys(v.cfg.AccountPools) {
		pp := "account_pools." + name
		v.length(pp, name, 1, 64)
		v.identifier(pp, name)
		ids := v.cfg.AccountPools[name]
		if len(ids) == 0 {
			v.add(pp, "must list at least one account")
			continue
		}
		v.pools[name] = true
		seen := map[string]bool{}
		for k, id := range ids {
			kpth := fmt.Sprintf("%s[%d]", pp, k)
			v.length(kpth, id, 1, 64)
			if _, err := domain.ParseAccountID(id); err != nil {
				v.addf(kpth, "must be an account identity: %s", err)
				continue
			}
			if seen[id] {
				v.addf(kpth, "duplicate account %q in pool %q", id, name)
				continue
			}
			seen[id] = true
			if _, ok := v.accountIDs[id]; !ok {
				v.addf(kpth, "references account %q, which is not declared under accounts", id)
			}
		}
	}
}

func (v *validator) tiers() {
	if !v.require("tiers") {
		return
	}
	if len(v.cfg.Tiers) < 1 {
		v.add("tiers", "at least one tier is required")
	} else if len(v.cfg.Tiers) > maxTiers {
		v.addf("tiers", "at most %d tiers are allowed (got %d)", maxTiers, len(v.cfg.Tiers))
	}
	seenNames := map[string]int{}
	for i, t := range v.cfg.Tiers {
		tp := fmt.Sprintf("tiers[%d]", i)
		if v.require(tp + ".name") {
			v.length(tp+".name", t.Name, 1, 64)
			if first, dup := seenNames[t.Name]; dup {
				v.addf(tp+".name", "duplicate tier name %q (first declared at tiers[%d])", t.Name, first)
			} else {
				seenNames[t.Name] = i
			}
		}
		if !v.require(tp + ".routes") {
			continue
		}
		if len(t.Routes) == 0 {
			v.add(tp+".routes", "at least one explicit route is required (auto_free only expands this list)")
			continue
		}
		seen := map[string]bool{}
		for k, id := range t.Routes {
			kpth := fmt.Sprintf("%s.routes[%d]", tp, k)
			v.length(kpth, id, 1, 64)
			if _, err := domain.ParseRouteID(id); err != nil {
				v.addf(kpth, "must be a route identity: %s", err)
				continue
			}
			if seen[id] {
				v.addf(kpth, "duplicate route %q in tier %q", id, t.Name)
				continue
			}
			seen[id] = true
			if _, ok := v.routeIDs[id]; !ok {
				v.addf(kpth, "references route %q, which is not declared under routes", id)
			}
		}
		if t.AccountPool != "" && !v.pools[t.AccountPool] {
			v.addf(tp+".account_pool", "references account pool %q, which is not declared under account_pools", t.AccountPool)
		}
	}
}

// --- prompts and simulation groups ----------------------------------------

func (v *validator) prompts() {
	c := v.cfg.Prompts
	if c == nil {
		return
	}
	for _, pf := range promptFiles(c) {
		pp := "prompts." + pf.field
		if pf.field == "app_extension" { // optional: an empty path means unset
			if v.has(pp) && pf.file != "" {
				v.length(pp, pf.file, 1, maxPromptPathLen)
				v.cleanPath(pp, pf.file)
			}
			continue
		}
		if v.require(pp) {
			v.length(pp, pf.file, 1, maxPromptPathLen)
			v.cleanPath(pp, pf.file)
		}
	}
}

func (v *validator) world() {
	c := v.cfg.World
	if c == nil {
		return
	}
	v.optionalString("world.baseline_version", c.BaselineVersion, 1, 64)
	v.optionalInt("world.max_objects", c.MaxObjects, 1, 10_000_000)
	v.optionalInt("world.max_content_bytes", c.MaxContentBytes, 1, 1<<30)
	v.optionalInt("world.materialization_budget_bytes", c.MaterializationBudgetBytes, 1, 1<<30)
	v.optionalInt("world.max_staged_changes", c.MaxStagedChanges, 1, 100_000)
	v.optionalInt("world.conflict_retries", c.ConflictRetries, 0, 64)
}

func (v *validator) apps() {
	c := v.cfg.Apps
	if c == nil {
		return
	}
	v.optionalString("apps.engine_abi_version", c.EngineABIVersion, 1, 32)
	v.optionalInt("apps.max_source_bytes", c.MaxSourceBytes, 1, 16_777_216)
	v.optionalInt("apps.max_state_bytes", c.MaxStateBytes, 1, 16_777_216)
	v.optionalInt("apps.heap_limit_bytes", c.HeapLimitBytes, 1, 1<<30)
	v.optionalInt("apps.stack_limit_bytes", c.StackLimitBytes, 1, 1<<30)
	v.optionalInt("apps.execution_deadline_ms", c.ExecutionDeadlineMs, 1, 600_000)
	v.optionalInt("apps.instance_pool_size", c.InstancePoolSize, 0, 1024)
	v.optionalInt("apps.max_generations", c.MaxGenerations, 1, 1000)
	v.optionalInt("apps.max_repairs", c.MaxRepairs, 0, 1000)
	if v.has("apps.stack_limit_bytes") && v.has("apps.heap_limit_bytes") && c.StackLimitBytes > c.HeapLimitBytes {
		v.addf("apps.stack_limit_bytes", "must not exceed apps.heap_limit_bytes (%d)", c.HeapLimitBytes)
	}
}

func (v *validator) terminal() {
	c := v.cfg.Terminal
	if c == nil {
		return
	}
	v.optionalInt("terminal.scrollback_lines", c.ScrollbackLines, 1, 1_000_000)
	v.optionalInt("terminal.max_paste_bytes", c.MaxPasteBytes, 1, 16_777_216)
	v.optionalEnum("terminal.redraw_policy", c.RedrawPolicy, supportedRedraw...)
	v.optionalInt("terminal.refresh_budget_lines", c.RefreshBudgetLines, 1, 100_000)
	v.optionalEnum("terminal.completion_cache", c.CompletionCache, supportedCache...)
}

func (v *validator) discovery() {
	c := v.cfg.Discovery
	if c == nil {
		return
	}
	v.optionalInt64("discovery.refresh_interval_ms", int64(c.RefreshIntervalMs), 1000, oneDayMs)
	v.optionalInt64("discovery.stale_age_ms", c.StaleAgeMs, 1000, 7*oneDayMs)
	if v.has("discovery.stale_age_ms") && v.has("discovery.refresh_interval_ms") && c.StaleAgeMs < int64(c.RefreshIntervalMs) {
		v.addf("discovery.stale_age_ms", "must be at least discovery.refresh_interval_ms (%d)", c.RefreshIntervalMs)
	}
	if v.has("discovery.metadata_url") {
		v.length("discovery.metadata_url", c.MetadataURL, 1, 2083)
		v.httpsURL("discovery.metadata_url", c.MetadataURL)
	}
	v.optionalInt("discovery.min_context_tokens", c.MinContextTokens, 0, 10_000_000)
	v.enumList("discovery.capabilities", c.Capabilities, supportedCapabils)
	v.stringList("discovery.explicit_allow", c.ExplicitAllow, modelPattern, maxModelIDLen)
	if v.has("discovery.protocol_overrides") {
		for _, model := range sortedKeys(c.ProtocolOverrides) {
			opth := "discovery.protocol_overrides." + model
			if !modelPattern.MatchString(model) || len(model) > maxModelIDLen {
				v.addf(opth, "must be a model id matching %s", modelPattern)
			}
			proto := c.ProtocolOverrides[model]
			if !knownProtocol(proto) {
				v.addf(opth, "unsupported protocol %q (supported: %s)", proto, quotedList(supportedProtocols))
			}
		}
	}
}

func (v *validator) health() {
	c := v.cfg.Health
	if c == nil {
		return
	}
	v.optionalInt("health.max_concurrent_probes", c.MaxConcurrentProbes, 1, 64)
	v.optionalInt64("health.probe_interval_ms", c.ProbeIntervalMs, 1000, oneDayMs)
	v.optionalInt("health.max_probes_per_hour", c.MaxProbesPerHour, 1, 3600)
	v.optionalInt64("health.initial_backoff_ms", c.InitialBackoffMs, 1, oneDayMs)
	v.optionalInt64("health.max_backoff_ms", c.MaxBackoffMs, 1, oneDayMs)
	if v.has("health.max_backoff_ms") && v.has("health.initial_backoff_ms") && c.MaxBackoffMs < c.InitialBackoffMs {
		v.addf("health.max_backoff_ms", "must be at least health.initial_backoff_ms (%d)", c.InitialBackoffMs)
	}
	v.optionalFloat("health.jitter_ratio", c.JitterRatio, 0, 1)
	v.optionalInt64("health.reset_grace_ms", c.ResetGraceMs, 0, oneDayMs)
}

func (v *validator) limits() {
	c := v.cfg.Limits
	if c == nil {
		return
	}
	v.optionalInt("limits.turn_deadline_ms", c.TurnDeadlineMs, 1, 3_600_000)
	v.optionalInt("limits.max_attempts", c.MaxAttempts, 1, 32)
	v.optionalInt("limits.max_output_bytes", c.MaxOutputBytes, 1, 1<<30)
	v.optionalInt("limits.max_content_bytes", c.MaxContentBytes, 1, 1<<30)
}

func (v *validator) inference() {
	c := v.cfg.Inference
	if c == nil {
		return
	}
	v.optionalInt("inference.request_deadline_ms", c.RequestDeadlineMs, 1, 3_600_000)
	v.optionalInt("inference.max_steps", c.MaxSteps, 1, 1000)
	v.optionalInt("inference.max_output_tokens", c.MaxOutputTokens, 1, 1_000_000)
	v.optionalInt("inference.global_concurrency", c.GlobalConcurrency, 1, 10_000)
	v.optionalInt("inference.max_account_concurrency", c.MaxAccountConcurrency, 1, 10_000)
	if v.has("inference.max_account_concurrency") && v.has("inference.global_concurrency") &&
		c.MaxAccountConcurrency > c.GlobalConcurrency {
		v.addf("inference.max_account_concurrency", "must not exceed inference.global_concurrency (%d)", c.GlobalConcurrency)
	}
	v.optionalInt("inference.wait_queue_depth", c.WaitQueueDepth, 0, 100_000)
	if v.has("inference.request_deadline_ms") && v.cfg.Limits != nil &&
		v.has("limits.turn_deadline_ms") && c.RequestDeadlineMs > v.cfg.Limits.TurnDeadlineMs {
		v.addf("inference.request_deadline_ms", "must not exceed limits.turn_deadline_ms (%d)", v.cfg.Limits.TurnDeadlineMs)
	}
}

func (v *validator) spending() {
	c := v.cfg.Spending
	if c == nil {
		return
	}
	v.optionalPositive("spending.instance_limit_usd", c.InstanceLimitUSD, 1_000_000)
	v.optionalPositive("spending.account_limit_usd", c.AccountLimitUSD, 1_000_000)
	v.optionalPositive("spending.user_limit_usd", c.UserLimitUSD, 1_000_000)
	v.optionalPositive("spending.session_limit_usd", c.SessionLimitUSD, 1_000_000)
	v.optionalEnum("spending.unknown_cost_policy", c.UnknownCostPolicy, supportedCostPolicy...)
}

func (v *validator) persistence() {
	c := v.cfg.Persistence
	if c == nil {
		v.add("persistence", "required group is missing")
		return
	}
	if v.require("persistence.database_path") {
		v.length("persistence.database_path", c.DatabasePath, 1, maxFilePathLen)
		v.absolute("persistence.database_path", c.DatabasePath)
	}
	v.optionalEnum("persistence.durability", c.Durability, supportedDurability...)
	if v.has("persistence.backup_dir") && c.BackupDir != "" {
		v.length("persistence.backup_dir", c.BackupDir, 1, maxFilePathLen)
		v.absolute("persistence.backup_dir", c.BackupDir)
	}
	v.optionalInt("persistence.writer_queue_depth", c.WriterQueueDepth, 1, 1024)
	v.optionalInt64("persistence.backup_interval_ms", c.BackupIntervalMs, 0, oneDayMs)
	v.optionalEnum("persistence.event_retention", c.EventRetention, supportedRetention...)
	if c.EventRetention == "days" {
		v.requireInt("persistence.event_retention_days", c.EventRetentionDays, 1, maxDaysRetention)
	}
}

func (v *validator) exports() {
	c := v.cfg.Exports
	if c == nil {
		return
	}
	if v.require("exports.output_dir") {
		v.length("exports.output_dir", c.OutputDir, 1, maxFilePathLen)
		v.absolute("exports.output_dir", c.OutputDir)
	}
	if v.require("exports.formats") {
		if len(c.Formats) == 0 {
			v.add("exports.formats", "at least one export format is required")
		}
		v.enumList("exports.formats", c.Formats, supportedFormats)
	}
	v.optionalInt("exports.max_concurrent_jobs", c.MaxConcurrentJobs, 1, 64)
	v.optionalInt("exports.max_export_bytes", c.MaxExportBytes, 1, 1<<30)
	if c.RedactSecrets != nil && !*c.RedactSecrets {
		v.add("exports.redact_secrets", "must be true: exports never contain secrets")
	}
}

func (v *validator) operations() {
	c := v.cfg.Operations
	if c == nil {
		return
	}
	v.optionalEnum("operations.log_level", c.LogLevel, supportedLogLevels...)
	v.optionalInt64("operations.health_check_interval_ms", c.HealthCheckIntervalMs, 0, 600_000)
	v.optionalInt64("operations.shutdown_grace_ms", c.ShutdownGraceMs, 0, 600_000)
	v.optionalEnum("operations.instrumentation", c.Instrumentation, supportedInstrument...)
}

// --- list helpers ----------------------------------------------------------

// enumList validates a string array against a fixed vocabulary and rejects
// duplicates. An absent field is fine; an empty one is not.
func (v *validator) enumList(path string, values, allowed []string) {
	if !v.has(path) {
		return
	}
	if len(values) == 0 {
		v.add(path, "must list at least one value")
		return
	}
	seen := map[string]bool{}
	for i, val := range values {
		ipth := fmt.Sprintf("%s[%d]", path, i)
		if !containsString(allowed, val) {
			v.addf(ipth, "must be one of %s (got %q)", quotedList(allowed), val)
			continue
		}
		if seen[val] {
			v.addf(ipth, "duplicate value %q", val)
			continue
		}
		seen[val] = true
	}
}

// stringList validates a string array against a pattern and rejects duplicates.
func (v *validator) stringList(path string, values []string, pattern *regexp.Regexp, maxLen int) {
	if !v.has(path) {
		return
	}
	if len(values) == 0 {
		v.add(path, "must list at least one value")
		return
	}
	seen := map[string]bool{}
	for i, val := range values {
		ipth := fmt.Sprintf("%s[%d]", path, i)
		v.length(ipth, val, 1, maxLen)
		if !pattern.MatchString(val) {
			v.addf(ipth, "must match %s (got %q)", pattern, val)
			continue
		}
		if seen[val] {
			v.addf(ipth, "duplicate value %q", val)
			continue
		}
		seen[val] = true
	}
}

// --- small shared helpers ---------------------------------------------------

func knownProtocol(p Protocol) bool {
	for _, s := range supportedProtocols {
		if string(p) == s {
			return true
		}
	}
	return false
}

func protocolNames(info *productInfo) []string {
	out := make([]string, 0, len(info.protocolList))
	for _, p := range info.protocolList {
		out = append(out, string(p))
	}
	return out
}

// cliProtocols reports whether protocols is exactly ["chat"]: a CLI
// provider returns text, so no other protocol is meaningful for it.
func cliProtocols(protocols []Protocol) bool {
	return len(protocols) == 1 && protocols[0] == ProtocolChat
}

// protocolValues renders the declared protocol strings as written, before
// (and regardless of) whether each one is a known protocol, so diagnostics
// quote the operator's own input.
func protocolValues(protocols []Protocol) []string {
	out := make([]string, 0, len(protocols))
	for _, p := range protocols {
		out = append(out, string(p))
	}
	return out
}

func quotedList(items []string) string {
	quoted := make([]string, 0, len(items))
	for _, s := range items {
		quoted = append(quoted, fmt.Sprintf("%q", s))
	}
	return strings.Join(quoted, ", ")
}

func containsString(list []string, want string) bool {
	for _, s := range list {
		if s == want {
			return true
		}
	}
	return false
}

func sortedKeys[V any](m map[string]V) []string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	return keys
}
