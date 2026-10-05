package domain_test

import (
	"encoding/json"
	"errors"
	"fmt"
	"testing"

	"j0s.at/vibeshell/internal/domain"
)

// validSampleIDs covers every identity type; each suffix is 26 Crockford
// base32 characters so Parse* accepts them.
var validSampleIDs = map[string]struct {
	parse  func(string) (fmt.Stringer, error)
	prefix string
}{
	"usr_0123456789ABCDEFGHJKMNPQRS": {func(s string) (fmt.Stringer, error) { return domain.ParseUserID(s) }, "usr"},
	"ses_0123456789ABCDEFGHJKMNPQRS": {func(s string) (fmt.Stringer, error) { return domain.ParseSessionID(s) }, "ses"},
	"trn_0123456789ABCDEFGHJKMNPQRS": {func(s string) (fmt.Stringer, error) { return domain.ParseTurnID(s) }, "trn"},
	"att_0123456789ABCDEFGHJKMNPQRS": {func(s string) (fmt.Stringer, error) { return domain.ParseAttemptID(s) }, "att"},
	"evt_0123456789ABCDEFGHJKMNPQRS": {func(s string) (fmt.Stringer, error) { return domain.ParseEventID(s) }, "evt"},
	"cnt_0123456789ABCDEFGHJKMNPQRS": {func(s string) (fmt.Stringer, error) { return domain.ParseContentID(s) }, "cnt"},
	"nod_0123456789ABCDEFGHJKMNPQRS": {func(s string) (fmt.Stringer, error) { return domain.ParseNodeID(s) }, "nod"},
	"nsp_0123456789ABCDEFGHJKMNPQRS": {func(s string) (fmt.Stringer, error) { return domain.ParseNamespaceID(s) }, "nsp"},
	"app_0123456789ABCDEFGHJKMNPQRS": {func(s string) (fmt.Stringer, error) { return domain.ParseAppID(s) }, "app"},
	"av_0123456789ABCDEFGHJKMNPQRS":  {func(s string) (fmt.Stringer, error) { return domain.ParseAppVersionID(s) }, "av"},
	"rte_0123456789ABCDEFGHJKMNPQRS": {func(s string) (fmt.Stringer, error) { return domain.ParseRouteID(s) }, "rte"},
	"acc_0123456789ABCDEFGHJKMNPQRS": {func(s string) (fmt.Stringer, error) { return domain.ParseAccountID(s) }, "acc"},
	"key_0123456789ABCDEFGHJKMNPQRS": {func(s string) (fmt.Stringer, error) { return domain.ParseKeyRef(s) }, "key"},
}

func TestIdentitiesParseAndRoundTrip(t *testing.T) {
	for raw, tc := range validSampleIDs {
		id, err := tc.parse(raw)
		if err != nil {
			t.Errorf("Parse(%q): %v", raw, err)
			continue
		}
		if id.String() != raw {
			t.Errorf("String() = %q, want %q", id.String(), raw)
		}
	}
}

func TestIdentitiesRejectMalformed(t *testing.T) {
	bad := []string{
		"",
		"usr_",
		"usr_0123456789ABCDEFGHJKMNPQRSX", // too long
		"usr_0123456789ABCDEFGHJKMNPQR",   // too short
		"usr_0123456789ABCDEFGHJKLMNPQRS", // contains I and L
		"usr_0123456789abcdefghjkmnpqrs",  // lowercase suffix
		"USR_0123456789ABCDEFGHJKMNPQRS",  // uppercase prefix
		"usr-0123456789ABCDEFGHJKMNPQRS",  // wrong separator
		"zzz_0123456789ABCDEFGHJKMNPQRS",  // unknown prefix still parses generically
	}
	for _, raw := range bad[:len(bad)-1] {
		if _, _, err := domain.ParseIdentity(raw); err == nil {
			t.Errorf("ParseIdentity(%q) succeeded, want error", raw)
		}
	}
	// A well-formed unknown prefix parses generically but is rejected by typed parsers.
	if _, _, err := domain.ParseIdentity(bad[len(bad)-1]); err != nil {
		t.Errorf("ParseIdentity(%q) failed, want success: %v", bad[len(bad)-1], err)
	}
	if _, err := domain.ParseUserID("ses_0123456789ABCDEFGHJKMNPQRS"); err == nil {
		t.Error("ParseUserID accepted a session-prefixed ID, want prefix error")
	}
}

func TestIdentityJSONIsStringForm(t *testing.T) {
	id, err := domain.ParseNodeID("nod_0123456789ABCDEFGHJKMNPQRS")
	if err != nil {
		t.Fatal(err)
	}
	raw, err := json.Marshal(id)
	if err != nil {
		t.Fatal(err)
	}
	if string(raw) != `"nod_0123456789ABCDEFGHJKMNPQRS"` {
		t.Fatalf("NodeID marshals as %s, want quoted string form", raw)
	}
	var back domain.NodeID
	if err := json.Unmarshal(raw, &back); err != nil {
		t.Fatal(err)
	}
	if back != id {
		t.Fatalf("round-trip = %v, want %v", back, id)
	}
	var wrong domain.UserID
	if err := json.Unmarshal(raw, &wrong); err == nil {
		t.Error("unmarshalling a node ID into UserID succeeded, want error")
	}
}

func TestScopeAuthorization(t *testing.T) {
	open := domain.DefaultScopePolicy()
	closed := domain.RestrictedScopePolicy()

	if err := open.AuthorizeRead(domain.ScopeShared, false); err != nil {
		t.Errorf("open policy should allow shared read: %v", err)
	}
	if err := closed.AuthorizeRead(domain.ScopeShared, false); !errors.Is(err, domain.CategoryDenied) {
		t.Errorf("closed policy shared read: want errors.Is denied, got %v", err)
	}
	if err := closed.AuthorizeRead(domain.ScopeUser, false); !errors.Is(err, domain.CategoryDenied) {
		t.Errorf("closed policy cross-user read: want denied, got %v", err)
	}
	if err := closed.AuthorizeRead(domain.ScopeUser, true); err != nil {
		t.Errorf("own user scope must stay readable when sharing is off: %v", err)
	}
	if err := closed.AuthorizeRead(domain.ScopeBaseline, false); err != nil {
		t.Errorf("baseline must stay world-readable: %v", err)
	}
	if err := open.AuthorizeWrite(domain.ScopeBaseline, true); !errors.Is(err, domain.CategoryDenied) {
		t.Errorf("baseline write: want denied, got %v", err)
	}
	if err := open.AuthorizeWrite(domain.ScopeShared, true); err != nil {
		t.Errorf("open policy should allow shared write: %v", err)
	}
	if err := closed.AuthorizeWrite(domain.ScopeShared, true); !errors.Is(err, domain.CategoryDenied) {
		t.Errorf("closed policy shared write: want denied, got %v", err)
	}
	// Denials must also match via errors.As to the structured type.
	err := closed.AuthorizeWrite(domain.ScopeShared, true)
	var de *domain.DomainError
	if !errors.As(err, &de) || de.Code != domain.CodeSharingDisabled {
		t.Errorf("denial As(*DomainError) = %v, want sharing_disabled", err)
	}
}

func TestDomainErrorCategoryMatching(t *testing.T) {
	inner := domain.NewDeniedError(domain.CodeScopeDenied, "nope", map[string]string{"scope": "shared"})
	wrapped := fmt.Errorf("turn failed: %w", inner)

	if !errors.Is(wrapped, domain.CategoryDenied) {
		t.Error("errors.Is(wrapped, CategoryDenied) = false, want true")
	}
	if errors.Is(wrapped, domain.CategoryConflict) {
		t.Error("errors.Is(wrapped, CategoryConflict) = true, want false")
	}
	var cat domain.ErrorCategory
	if !errors.As(wrapped, &cat) || cat != domain.CategoryDenied {
		t.Errorf("errors.As category = %v, want denied", cat)
	}
	var de *domain.DomainError
	if !errors.As(wrapped, &de) || de.Code != domain.CodeScopeDenied {
		t.Errorf("errors.As *DomainError = %v, want scope_denied", wrapped)
	}
	if !domain.IsDeniedError(wrapped) || domain.IsConflictError(wrapped) {
		t.Error("IsDeniedError/IsConflictError helpers disagree")
	}
	if got := domain.GetErrorCategory(wrapped); got != domain.CategoryDenied {
		t.Errorf("GetErrorCategory = %v, want denied", got)
	}
}

func TestFailureClassMatching(t *testing.T) {
	route, _ := domain.ParseRouteID("rte_0123456789ABCDEFGHJKMNPQRS")
	acc, _ := domain.ParseAccountID("acc_0123456789ABCDEFGHJKMNPQRS")
	env := domain.NewErrorEnvelope(domain.FailureQuotaExhausted, "exhausted", route, acc, 1798732800000)

	if !errors.Is(env, domain.FailureQuotaExhausted) {
		t.Error("errors.Is(envelope, FailureQuotaExhausted) = false, want true")
	}
	if errors.Is(env, domain.FailureRateLimited) {
		t.Error("errors.Is(envelope, FailureRateLimited) = true, want false")
	}
	var fc domain.FailureClass
	if !errors.As(env, &fc) || fc != domain.FailureQuotaExhausted {
		t.Errorf("errors.As class = %v, want quota_exhausted", fc)
	}
	if !domain.FailureQuotaExhausted.IsAccountScope() || !domain.FailureInvalidCredential.IsCredentialScope() {
		t.Error("failure scope helpers disagree")
	}
	if !domain.FailureRateLimited.IsRetryable() || domain.FailureUserCancelled.IsRetryable() {
		t.Error("IsRetryable disagrees: rate_limited must retry, user_cancelled must not")
	}
}

func TestHealthTransitions(t *testing.T) {
	route, _ := domain.ParseRouteID("rte_0123456789ABCDEFGHJKMNPQRS")
	acc, _ := domain.ParseAccountID("acc_0123456789ABCDEFGHJKMNPQRS")
	const now = int64(1798732800000)

	h := domain.NewHealthRecord(route, acc, "account", now)
	if h.State != domain.HealthHealthy {
		t.Fatalf("new record state = %v, want healthy", h.State)
	}
	retryAfter := int64(600000)
	h.RecordFailure(domain.FailureQuotaExhausted, "exhausted", &retryAfter, now)
	if h.State != domain.HealthCoolingDown {
		t.Fatalf("after quota failure state = %v, want cooling_down", h.State)
	}
	if h.CooldownUntil != now+retryAfter {
		t.Errorf("CooldownUntil = %d, want Retry-After honored (%d)", h.CooldownUntil, now+retryAfter)
	}
	if h.RetryAfter != retryAfter {
		t.Errorf("RetryAfter = %d, want %d", h.RetryAfter, retryAfter)
	}

	q := domain.NewHealthRecord(route, acc, "route", now)
	q.RecordFailure(domain.FailureModelNotFound, "gone", nil, now)
	if q.State != domain.HealthQuarantined {
		t.Fatalf("after model_not_found state = %v, want quarantined", q.State)
	}

	if !domain.CanTransition(domain.HealthCoolingDown, domain.HealthProbeEligible) {
		t.Error("cooling_down -> probe_eligible must be valid")
	}
	if domain.CanTransition(domain.HealthHealthy, domain.HealthProbing) {
		t.Error("healthy -> probing must be invalid (must pass through cooling)")
	}

	// Probe cycle returns to healthy on success.
	p := domain.NewHealthRecord(route, acc, "route", now)
	p.RecordFailure(domain.FailureProviderOutage, "5xx", nil, now)
	p.RecordProbeStart(now) // cooling_down cannot probe directly; stays put
	if p.State == domain.HealthProbing {
		t.Error("probe must not start from cooling_down")
	}
}

func TestPathValidation(t *testing.T) {
	for _, ok := range []string{"/", "/home/alice", "/a/new/place/manual.txt", "/archive/cloud-gardens/notes.txt"} {
		if _, err := domain.ParsePath(ok); err != nil {
			t.Errorf("ParsePath(%q) = %v, want success", ok, err)
		}
	}
	for _, bad := range []string{"", "relative/path", "/a//b", "/a/../b", "/a/./b", "/home/alice/", "/x\x00y"} {
		if _, err := domain.ParsePath(bad); err == nil {
			t.Errorf("ParsePath(%q) succeeded, want error", bad)
		}
	}
}

func TestValidationHelpers(t *testing.T) {
	// A file create (kind zero value) must validate: NodeKindFile == 0 is
	// legitimate, not a missing field. This guards the previous-model bug
	// that rejected every file mutation.
	ns, _ := domain.ParseNamespaceID("nsp_0123456789ABCDEFGHJKMNPQRS")
	node, _ := domain.ParseNodeID("nod_0123456789ABCDEFGHJKMNPQRS")
	res := domain.AppResult{
		NewState: domain.AppState{},
		View:     domain.AppView{Mode: domain.AppViewModeText},
		Effects: []domain.AppEffect{
			{Mutation: domain.Mutation{
				Type:        domain.MutationCreate,
				NamespaceID: ns,
				Path:        "/home/alice/notes.txt",
				Kind:        domain.NodeKindFile,
				Metadata:    domain.NewNodeMetadata(0o644, 1000, 1000, 1798732800000),
				Content:     domain.ContentRef{Size: 3, MediaType: "text/plain"},
			}},
			{Mutation: domain.Mutation{
				Type:        domain.MutationUpdate,
				NamespaceID: ns,
				Path:        "/home/alice/notes.txt",
				NodeID:      &node,
				ExpectedRev: 2,
			}},
		},
	}
	if err := domain.ValidateResult(res); err != nil {
		t.Errorf("ValidateResult(file create+update) = %v, want success", err)
	}

	bad := domain.AppManifest{ABIVersion: 999, CommandNames: []string{"x"}, Entrypoint: "main"}
	if err := domain.ValidateManifest(bad); err == nil {
		t.Error("ValidateManifest accepted a wrong ABI version, want error")
	}
	if err := domain.ValidateRoutePolicy(domain.RoutePolicy{}); err == nil {
		t.Error("ValidateRoutePolicy accepted empty tiers, want error")
	}
	if err := (domain.Pagination{Limit: 0}).Validate(); err == nil {
		t.Error("Pagination{0} accepted, want error")
	}
	if err := (domain.Pagination{Limit: 1001}).Validate(); err == nil {
		t.Error("Pagination{1001} accepted, want error")
	}
}
