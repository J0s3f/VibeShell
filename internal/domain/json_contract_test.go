package domain_test

import (
	"encoding/json"
	"reflect"
	"testing"

	"j0s.at/vibeshell/internal/domain"
)

// mustParse fails the test when a parser returns an error and otherwise
// returns the parsed value. JSON contract tests use only canary values.
func mustParse[T any](t *testing.T, s string, parse func(string) (T, error)) T {
	t.Helper()
	v, err := parse(s)
	if err != nil {
		t.Fatal(err)
	}
	return v
}

// zeroIdent is satisfied by every strongly-typed identity.
type zeroIdent interface {
	comparable
	IsZero() bool
}

// checkZeroIdentity asserts the zero identity encodes as a valid empty JSON
// value and decodes back to zero from both "" and null. Before the contract
// fix the zero identity marshalled to "_" and failed to unmarshal.
func checkZeroIdentity[T zeroIdent](t *testing.T, name string, zero T) {
	t.Helper()

	raw, err := json.Marshal(zero)
	if err != nil {
		t.Fatalf("%s: marshal zero: %v", name, err)
	}
	if string(raw) != `""` {
		t.Errorf("%s: zero marshals %s, want \"\"", name, raw)
	}
	for _, in := range []string{`""`, "null"} {
		var back T
		if err := json.Unmarshal([]byte(in), &back); err != nil {
			t.Errorf("%s: unmarshal %s: %v", name, in, err)
			continue
		}
		if !back.IsZero() {
			t.Errorf("%s: unmarshal %s = %v, want zero", name, in, back)
		}
	}
}

func TestZeroIdentityJSONRoundTrips(t *testing.T) {
	checkZeroIdentity(t, "UserID", domain.UserID{})
	checkZeroIdentity(t, "SessionID", domain.SessionID{})
	checkZeroIdentity(t, "TurnID", domain.TurnID{})
	checkZeroIdentity(t, "AttemptID", domain.AttemptID{})
	checkZeroIdentity(t, "EventID", domain.EventID{})
	checkZeroIdentity(t, "ContentID", domain.ContentID{})
	checkZeroIdentity(t, "NodeID", domain.NodeID{})
	checkZeroIdentity(t, "NamespaceID", domain.NamespaceID{})
	checkZeroIdentity(t, "AppID", domain.AppID{})
	checkZeroIdentity(t, "AppVersionID", domain.AppVersionID{})
	checkZeroIdentity(t, "RouteID", domain.RouteID{})
	checkZeroIdentity(t, "AccountID", domain.AccountID{})
	checkZeroIdentity(t, "KeyRef", domain.KeyRef{})
}

// checkNonZeroIdentity asserts a populated identity keeps its canonical
// "prefix_value" JSON shape and round-trips exactly.
func checkNonZeroIdentity[T comparable](t *testing.T, name string, id T, want string) {
	t.Helper()

	raw, err := json.Marshal(id)
	if err != nil {
		t.Fatalf("%s: marshal: %v", name, err)
	}
	if string(raw) != want {
		t.Errorf("%s: marshals %s, want %s", name, raw, want)
	}
	var back T
	if err := json.Unmarshal(raw, &back); err != nil {
		t.Fatalf("%s: unmarshal: %v", name, err)
	}
	if back != id {
		t.Errorf("%s: round-trip = %v, want %v", name, back, id)
	}
}

func TestNonZeroIdentityJSONShapePreserved(t *testing.T) {
	checkNonZeroIdentity(t, "UserID",
		mustParse(t, "usr_0123456789ABCDEFGHJKMNPQRS", domain.ParseUserID),
		`"usr_0123456789ABCDEFGHJKMNPQRS"`)
	checkNonZeroIdentity(t, "ContentID",
		mustParse(t, "cnt_0123456789ABCDEFGHJKMNPQRS", domain.ParseContentID),
		`"cnt_0123456789ABCDEFGHJKMNPQRS"`)
	checkNonZeroIdentity(t, "RouteID",
		mustParse(t, "rte_0123456789ABCDEFGHJKMNPQRS", domain.ParseRouteID),
		`"rte_0123456789ABCDEFGHJKMNPQRS"`)
}

func TestContentRefJSONZeroRoundTrips(t *testing.T) {
	zero := domain.ContentRef{}
	if !zero.IsZero() {
		t.Fatal("zero ContentRef.IsZero() = false, want true")
	}

	raw, err := json.Marshal(zero)
	if err != nil {
		t.Fatalf("marshal zero ContentRef: %v", err)
	}
	var back domain.ContentRef
	if err := json.Unmarshal(raw, &back); err != nil {
		t.Fatalf("zero ContentRef does not round-trip: %v", err)
	}
	if !reflect.DeepEqual(back, zero) {
		t.Errorf("zero ContentRef round-trip = %+v, want %+v", back, zero)
	}

	for _, in := range []string{"null", `""`, "{}"} {
		var v domain.ContentRef
		if err := json.Unmarshal([]byte(in), &v); err != nil {
			t.Errorf("unmarshal %s into ContentRef: %v", in, err)
			continue
		}
		if !v.IsZero() {
			t.Errorf("unmarshal %s = %+v, want zero", in, v)
		}
	}

	// Empty content is a real reference with a media type, distinct from zero.
	if domain.EmptyContentRef().IsZero() {
		t.Error("EmptyContentRef().IsZero() = true, want false")
	}

	ref := domain.ContentRef{
		Hash:      mustParse(t, "cnt_0123456789ABCDEFGHJKMNPQRS", domain.ParseContentID),
		Size:      1048576,
		MediaType: "application/octet-stream",
	}
	rawRef, err := json.Marshal(ref)
	if err != nil {
		t.Fatalf("marshal non-zero ContentRef: %v", err)
	}
	var backRef domain.ContentRef
	if err := json.Unmarshal(rawRef, &backRef); err != nil {
		t.Fatalf("non-zero ContentRef does not round-trip: %v", err)
	}
	if !reflect.DeepEqual(backRef, ref) {
		t.Errorf("non-zero ContentRef round-trip = %+v, want %+v", backRef, ref)
	}
}

func TestChangeSetJSONZeroValueRoundTrips(t *testing.T) {
	var zero domain.ChangeSet

	raw, err := json.Marshal(zero)
	if err != nil {
		t.Fatalf("marshal zero ChangeSet: %v", err)
	}
	var back domain.ChangeSet
	if err := json.Unmarshal(raw, &back); err != nil {
		t.Fatalf("zero ChangeSet does not round-trip: %v", err)
	}
	if !reflect.DeepEqual(back, zero) {
		t.Errorf("zero ChangeSet round-trip:\n want %+v\n got  %+v", zero, back)
	}
}

func TestMutationJSONZeroOptionalIdentitiesRoundTrips(t *testing.T) {
	ns := mustParse(t, "nsp_0123456789ABCDEFGHJKMNPQRS", domain.ParseNamespaceID)
	path := domain.MustParsePath("/home/alice/notes.txt")
	zeroNode := domain.NodeID{}

	cases := []struct {
		name string
		m    domain.Mutation
	}{
		{"fully zero", domain.Mutation{}},
		{"create with zero content", domain.Mutation{
			Type:        domain.MutationCreate,
			NamespaceID: ns,
			Path:        path,
			Kind:        domain.NodeKindFile,
			Content:     domain.ContentRef{},
		}},
		{"update with pointer to zero node id", domain.Mutation{
			Type:        domain.MutationUpdate,
			NamespaceID: ns,
			Path:        path,
			NodeID:      &zeroNode,
			ExpectedRev: 2,
		}},
	}
	for _, tc := range cases {
		raw, err := json.Marshal(tc.m)
		if err != nil {
			t.Fatalf("%s: marshal: %v", tc.name, err)
		}
		var back domain.Mutation
		if err := json.Unmarshal(raw, &back); err != nil {
			t.Fatalf("%s: unmarshal: %v (json %s)", tc.name, err, raw)
		}
		if !reflect.DeepEqual(back, tc.m) {
			t.Errorf("%s: round-trip:\n want %+v\n got  %+v\n json %s", tc.name, tc.m, back, raw)
		}
	}
}

func TestRequiredIdentityValidationRejectsZero(t *testing.T) {
	res := domain.AppResult{
		View: domain.AppView{Mode: domain.AppViewModeText},
		Effects: []domain.AppEffect{{Mutation: domain.Mutation{
			Type:        domain.MutationCreate,
			NamespaceID: domain.NamespaceID{},
			Path:        domain.MustParsePath("/home/alice/notes.txt"),
			Kind:        domain.NodeKindFile,
		}}},
	}
	if err := domain.ValidateResult(res); err == nil {
		t.Error("ValidateResult accepted a mutation with a zero namespace_id, want error")
	}
}
