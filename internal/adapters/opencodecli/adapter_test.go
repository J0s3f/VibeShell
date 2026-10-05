package opencodecli

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"j0s.at/vibeshell/internal/domain"
)

// fakeRunner is a Runner that replays canned output and records its inputs.
type fakeRunner struct {
	stdout []byte
	stderr []byte
	err    error

	gotName  string
	gotArgs  []string
	gotStdin []byte
	gotCtx   context.Context
}

func (f *fakeRunner) Run(ctx context.Context, name string, args []string, stdin []byte) ([]byte, []byte, error) {
	f.gotName = name
	f.gotArgs = append([]string(nil), args...)
	f.gotStdin = append([]byte(nil), stdin...)
	f.gotCtx = ctx
	return f.stdout, f.stderr, f.err
}

func testAdapter(r Runner) *Adapter {
	return &Adapter{
		Models: map[domain.RouteID]string{
			domain.MustParseRouteID("rte_AAAAAAAAAAAAAAAAAAAAAAAAAA"): "demo-model",
		},
		Runner: r,
	}
}

func testRequest() domain.ModelRequest {
	return domain.ModelRequest{
		RouteID:   domain.MustParseRouteID("rte_AAAAAAAAAAAAAAAAAAAAAAAAAA"),
		AccountID: domain.MustParseAccountID("acc_AAAAAAAAAAAAAAAAAAAAAAAAAA"),
		Messages:  []domain.Message{{Role: domain.RoleUser, Content: "hello"}},
		RequestID: "req-1",
	}
}

func TestRequestConcatenatesNDJSONTextParts(t *testing.T) {
	r := &fakeRunner{stdout: []byte(`{"type":"step_start","part":{}}
{"type":"text","part":{"type":"text","text":"Hello, "}}
{"type":"text","part":{"type":"text","text":"world"}}
{"type":"text","part":{"type":"text","text":"!"}}
{"type":"step_finish","part":{}}
`)}
	resp, err := testAdapter(r).Request(context.Background(), testRequest())
	if err != nil {
		t.Fatalf("Request: %v", err)
	}
	if got, want := resp.Message.Content, "Hello, world!"; got != want {
		t.Fatalf("content = %q, want %q", got, want)
	}
	if resp.RouteID.String() != "rte_AAAAAAAAAAAAAAAAAAAAAAAAAA" || resp.AccountID.String() != "acc_AAAAAAAAAAAAAAAAAAAAAAAAAA" {
		t.Fatalf("response identity not propagated: %+v", resp)
	}
}

func TestRequestDeadlineExceededMapsToNetworkTimeout(t *testing.T) {
	ctx, cancel := context.WithDeadline(context.Background(), time.Now().Add(-time.Second))
	defer cancel()
	r := &fakeRunner{err: ctx.Err()}
	_, err := testAdapter(r).Request(ctx, testRequest())
	if !errors.Is(err, domain.FailureNetworkTimeout) {
		t.Fatalf("err = %v, want network_timeout", err)
	}
}

func TestRequestDeadlineExceededViaRunnerError(t *testing.T) {
	r := &fakeRunner{err: context.DeadlineExceeded}
	_, err := testAdapter(r).Request(context.Background(), testRequest())
	if !errors.Is(err, domain.FailureNetworkTimeout) {
		t.Fatalf("err = %v, want network_timeout", err)
	}
}

func TestRequestEmptyOutputMapsToInvalidResponse(t *testing.T) {
	r := &fakeRunner{stdout: nil}
	_, err := testAdapter(r).Request(context.Background(), testRequest())
	if !errors.Is(err, domain.FailureInvalidResponse) {
		t.Fatalf("err = %v, want invalid_response", err)
	}
}

func TestRequestOutputWithoutTextMapsToInvalidResponse(t *testing.T) {
	r := &fakeRunner{stdout: []byte(`{"type":"step_start"}` + "\n")}
	_, err := testAdapter(r).Request(context.Background(), testRequest())
	if !errors.Is(err, domain.FailureInvalidResponse) {
		t.Fatalf("err = %v, want invalid_response", err)
	}
}

func TestRequestNonZeroExitMapsToProviderOutage(t *testing.T) {
	r := &fakeRunner{stderr: []byte("boom"), err: errors.New("exit status 1")}
	_, err := testAdapter(r).Request(context.Background(), testRequest())
	if !errors.Is(err, domain.FailureProviderOutage) {
		t.Fatalf("err = %v, want provider_outage", err)
	}
}

func TestRequestUnknownRouteMapsToModelNotFound(t *testing.T) {
	req := testRequest()
	req.RouteID = domain.MustParseRouteID("rte_BBBBBBBBBBBBBBBBBBBBBBBBBB")
	_, err := testAdapter(&fakeRunner{}).Request(context.Background(), req)
	if !errors.Is(err, domain.FailureModelNotFound) {
		t.Fatalf("err = %v, want model_not_found", err)
	}
}

func TestRequestNoMessagesMapsToInvalidArguments(t *testing.T) {
	req := testRequest()
	req.Messages = nil
	_, err := testAdapter(&fakeRunner{}).Request(context.Background(), req)
	if !errors.Is(err, domain.FailureInvalidArguments) {
		t.Fatalf("err = %v, want invalid_arguments", err)
	}
}

func TestMessagesToPrompt(t *testing.T) {
	tests := []struct {
		name string
		in   []domain.Message
		want string
	}{
		{"empty", nil, ""},
		{"single", []domain.Message{{Content: "hello"}}, "hello"},
		{
			"order preserved",
			[]domain.Message{{Content: "first"}, {Content: "second"}, {Content: "third"}},
			"first\n\nsecond\n\nthird",
		},
		{
			"blank messages skipped",
			[]domain.Message{{Content: "first"}, {Content: "  "}, {Content: ""}, {Content: "last"}},
			"first\n\nlast",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := messagesToPrompt(tt.in); got != tt.want {
				t.Fatalf("messagesToPrompt = %q, want %q", got, tt.want)
			}
		})
	}
}

func TestParseEventsIgnoresNonJSONLines(t *testing.T) {
	out := "downloading model...\n" +
		`{"type":"text","part":{"type":"text","text":"hi"}}` + "\n" +
		"not json at all\n"
	text, err := parseEvents([]byte(out))
	if err != nil {
		t.Fatalf("parseEvents: %v", err)
	}
	if text != "hi" {
		t.Fatalf("text = %q, want %q", text, "hi")
	}
}

func TestParseEventsNoJSONEvents(t *testing.T) {
	if _, err := parseEvents([]byte("warning: no server\nplain text\n")); err == nil ||
		!strings.Contains(err.Error(), "no JSON events") {
		t.Fatalf("err = %v, want 'no JSON events'", err)
	}
}

func TestRequestInvokesCLIWithProviderAndModel(t *testing.T) {
	r := &fakeRunner{stdout: []byte(`{"type":"text","part":{"type":"text","text":"ok"}}` + "\n")}
	a := testAdapter(r)
	a.ProviderID = "opencode-go"
	a.Binary = "opencode-dev"
	if _, err := a.Request(context.Background(), testRequest()); err != nil {
		t.Fatalf("Request: %v", err)
	}
	if r.gotName != "opencode-dev" {
		t.Fatalf("binary = %q", r.gotName)
	}
	args := strings.Join(r.gotArgs, " ")
	if !strings.Contains(args, "-m opencode-go/demo-model") || !strings.Contains(args, "--format json") {
		t.Fatalf("args = %v", r.gotArgs)
	}
}
