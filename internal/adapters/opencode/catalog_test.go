package opencode

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func testMetadataBody() string {
	model := func(id, npm string, tool bool, input, output []string, ctx int, cost string) string {
		return fmt.Sprintf(`%q:{"id":%q,"tool_call":%t,"modalities":{"input":%s,"output":%s},"limit":{"context":%d},"cost":%s,"provider":{"npm":%q}}`,
			id, id, tool, strList(input), strList(output), ctx, cost, npm)
	}
	zero := `{"input":0,"output":0,"cache_read":0,"cache_write":0}`
	paid := `{"input":1.0,"output":2.0}`
	parts := []string{
		model("glow-free", "@ai-sdk/openai", true, []string{"text"}, []string{"text"}, 200000, zero),
		model("claude-thing-free", "@ai-sdk/anthropic", true, []string{"text"}, []string{"text"}, 200000, zero),
		model("gem-flash-free", "@ai-sdk/google", true, []string{"text"}, []string{"text"}, 200000, zero),
		model("big-pickle", "@ai-sdk/openai", true, []string{"text"}, []string{"text"}, 200000, zero),
		model("rich-paid-free", "@ai-sdk/openai", true, []string{"text"}, []string{"text"}, 200000, paid),
		model("tiny-free", "@ai-sdk/openai", true, []string{"text"}, []string{"text"}, 8000, zero),
		model("img-free", "@ai-sdk/openai", true, []string{"image"}, []string{"text"}, 200000, zero),
		model("notool-free", "@ai-sdk/openai", false, []string{"text"}, []string{"text"}, 200000, zero),
		// mystery-free has no cost block: suitability must need an
		// operator decision, never silently zero.
		`"mystery-free":{"id":"mystery-free","tool_call":true,"modalities":{"input":["text"],"output":["text"]},"limit":{"context":200000},"provider":{"npm":"@ai-sdk/openai"}}`,
	}
	return `{"opencode":{"models":{` + strings.Join(parts, ",") + `}},"opencode-go":{"models":{}}}`
}

func strList(in []string) string {
	out := "["
	for i, s := range in {
		if i > 0 {
			out += ","
		}
		out += `"` + s + `"`
	}
	return out + "]"
}

func loadTestMetadata(t *testing.T) map[Product]map[string]Meta {
	t.Helper()
	metas, err := LoadMetadata([]byte(testMetadataBody()))
	if err != nil {
		t.Fatal(err)
	}
	return metas
}

func filterOpts() FilterOptions {
	return FilterOptions{
		MinContext:     100000,
		ExplicitAllow:  map[string]bool{"big-pickle": true},
		StructuredOnly: map[string]bool{"jev-1.13-free": true},
	}
}

func TestSuitableFreeMatrix(t *testing.T) {
	metas := loadTestMetadata(t)[ProductConsole]
	cases := []struct {
		id       string
		eligible bool
		reason   string
		protocol Protocol
	}{
		{"glow-free", true, ReasonEligibleAutoFree, ProtocolChat},
		{"claude-thing-free", true, ReasonEligibleAutoFree, ProtocolMessages},
		{"gem-flash-free", true, ReasonEligibleAutoFree, ProtocolGemini},
		{"big-pickle", true, ReasonEligibleExplicit, ProtocolChat},
		{"rich-paid-free", false, ReasonKnownPaid, ""},
		{"mystery-free", false, ReasonUnknownCost, ""},
		{"ghost-free", false, ReasonUnknownMetadata, ""},
		{"tiny-free", false, ReasonContextTooSmall, ""},
		{"img-free", false, ReasonNoTextModality, ""},
		{"notool-free", false, ReasonNoToolSupport, ""},
		{"jev-1.13-free", false, ReasonStructuredOnly, ""},
		{"plain-model", false, ReasonNotFreeSuffix, ""},
	}
	// plain-model needs metadata present but no -free suffix and no
	// explicit allow to hit the suffix gate.
	metas["plain-model"] = metas["glow-free"]
	for _, c := range cases {
		d := SuitableFree(ProductConsole, c.id, metas, filterOpts())
		if d.Eligible != c.eligible || d.Reason != c.reason {
			t.Fatalf("%s: eligible=%t reason=%q, want %t %q", c.id, d.Eligible, d.Reason, c.eligible, c.reason)
		}
		if c.eligible && d.Protocol != c.protocol {
			t.Fatalf("%s: protocol=%q, want %q", c.id, d.Protocol, c.protocol)
		}
	}
}

func TestBigPickleNeedsExplicitAllow(t *testing.T) {
	metas := loadTestMetadata(t)[ProductConsole]
	opts := filterOpts()
	opts.ExplicitAllow = nil
	if d := SuitableFree(ProductConsole, "big-pickle", metas, opts); d.Eligible {
		t.Fatalf("big-pickle eligible without explicit allow: %+v", d)
	}
}

func TestExplicitDenyWins(t *testing.T) {
	metas := loadTestMetadata(t)[ProductConsole]
	opts := filterOpts()
	opts.ExplicitDeny = map[string]bool{"glow-free": true}
	if d := SuitableFree(ProductConsole, "glow-free", metas, opts); d.Eligible || d.Reason != ReasonDeniedExplicit {
		t.Fatalf("deny did not win: %+v", d)
	}
}

func TestExpandFreeAndEndpointRefusal(t *testing.T) {
	metas := loadTestMetadata(t)[ProductConsole]
	eligible, rejected := ExpandFree(ProductConsole,
		[]string{"glow-free", "claude-thing-free", "rich-paid-free", "ghost-free"}, metas, filterOpts())
	if len(eligible) != 2 || len(rejected) != 2 {
		t.Fatalf("eligible=%d rejected=%d", len(eligible), len(rejected))
	}
	chatURL, err := EndpointForDecision(eligible[0])
	if err != nil {
		t.Fatal(err)
	}
	msgURL, err := EndpointForDecision(Decision{Product: ProductConsole, ID: "claude-thing-free", Eligible: true, Protocol: ProtocolMessages})
	if err != nil {
		t.Fatal(err)
	}
	if msgURL == chatURL {
		t.Fatalf("messages model resolves to chat endpoint %s", chatURL)
	}
	if _, err := EndpointForDecision(Decision{Product: ProductConsole, ID: "x", Eligible: true}); err == nil {
		t.Fatal("unknown protocol resolved instead of refusing")
	}
	if _, err := EndpointForDecision(rejected[0]); err == nil {
		t.Fatal("ineligible model resolved instead of refusing")
	}
}

func TestProtocolOverridePinsResponses(t *testing.T) {
	metas := loadTestMetadata(t)[ProductConsole]
	opts := filterOpts()
	opts.ProtocolOverrides = map[string]Protocol{"glow-free": ProtocolResponses}
	d := SuitableFree(ProductConsole, "glow-free", metas, opts)
	if !d.Eligible || d.Protocol != ProtocolResponses {
		t.Fatalf("override not honored: %+v", d)
	}
	u, err := EndpointForDecision(d)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasSuffix(u, "/responses") {
		t.Fatalf("url = %s", u)
	}
}

func TestMetadataCacheRecordsSourceHashTime(t *testing.T) {
	c := &Cache{clock: stubClock{now: 4242}}
	body := testMetadataBody()
	if err := c.StoreMetadata([]byte(body), "test://models.dev"); err != nil {
		t.Fatal(err)
	}
	metas, prov := c.Metadata()
	if metas[ProductConsole]["glow-free"].Context != 200000 {
		t.Fatalf("metadata not stored: %+v", metas)
	}
	sum := sha256.Sum256([]byte(body))
	if prov.SHA256 != hex.EncodeToString(sum[:]) || prov.Source != "test://models.dev" || prov.FetchedAt != 4242 {
		t.Fatalf("provenance = %+v", prov)
	}
	if _, ok := c.MetadataFor(ProductGo); !ok {
		t.Fatal("go product metadata missing")
	}
}

func TestCatalogueRefreshStoresProvenanceAndFallsBack(t *testing.T) {
	good := `{"object":"list","data":[{"id":"b-free","object":"model","created":7,"owned_by":"o"},{"id":"a-free","object":"model","created":8,"owned_by":"o"}]}`
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("User-Agent") != ClientIdentity {
			http.Error(w, "bad agent", 400)
			return
		}
		_, _ = io.WriteString(w, good)
	}))
	t.Cleanup(srv.Close)

	c := &Cache{clock: stubClock{now: 1000}, MaxAgeMs: 60_000}
	snap, err := c.RefreshCatalog(context.Background(), ProductConsole, srv.URL+"/models")
	if err != nil {
		t.Fatal(err)
	}
	if snap.Count != 2 || len(snap.IDs) != 2 || snap.IDs[0] != "a-free" {
		t.Fatalf("snapshot = %+v", snap)
	}
	sum := sha256.Sum256([]byte(good))
	if snap.SHA256 != hex.EncodeToString(sum[:]) || snap.Source != srv.URL+"/models" || snap.FetchedAt != 1000 {
		t.Fatalf("provenance = %+v", snap)
	}

	// A failed refresh leaves the last success intact for use within age.
	bad := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(500)
	}))
	t.Cleanup(bad.Close)
	if _, err := c.RefreshCatalog(context.Background(), ProductConsole, bad.URL); err == nil {
		t.Fatal("failed refresh returned success")
	}
	if got, ok := c.SnapshotWithinAge(ProductConsole, 1000+59_000); !ok || got.SHA256 != snap.SHA256 {
		t.Fatalf("last-good not usable within age: %+v %t", got, ok)
	}
	if _, ok := c.SnapshotWithinAge(ProductConsole, 1000+61_000); ok {
		t.Fatal("stale snapshot usable past its age")
	}
}

func TestCatalogueRejectsMalformedShape(t *testing.T) {
	for _, body := range []string{
		`{"object":"list"}`,
		`{"object":"list","data":[{"object":"model"}]}`,
		`not json`,
		`{"data":[]}`,
	} {
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			_, _ = io.WriteString(w, body)
		}))
		t.Cleanup(srv.Close)
		c := &Cache{clock: stubClock{now: 1}}
		if _, err := c.RefreshCatalog(context.Background(), ProductConsole, srv.URL); err == nil {
			t.Fatalf("malformed catalogue accepted: %s", body)
		}
	}
}
