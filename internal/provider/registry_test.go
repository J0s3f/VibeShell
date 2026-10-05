package provider

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"sync/atomic"
	"testing"

	"j0s.at/vibeshell/internal/domain"
)

// testNowUnixMilli is the instant the deterministic clock reports, so a failure
// envelope's timestamp is asserted rather than whatever the machine clock was.
const testNowUnixMilli int64 = 1_700_000_000_000

// Routes and account used by the registry tests.
var (
	httpRoute   = domain.MustParseRouteID("rte_AAAAAAAAAAAAAAAAAAAAAAAAAA")
	cliRoute    = domain.MustParseRouteID("rte_BBBBBBBBBBBBBBBBBBBBBBBBBB")
	testAccount = domain.MustParseAccountID("acc_CCCCCCCCCCCCCCCCCCCCCCCCCC")
)

// fixedClock is a deterministic ports.Clock.
type fixedClock struct{ now int64 }

func (c fixedClock) NowUnixMilli() int64   { return c.now }
func (c fixedClock) MonotonicNanos() int64 { return c.now * 1000 }

// recordingGateway is a ports.ModelGateway double. Its reply text identifies the
// provider so a test can tell which one a request reached, and it records the
// requests it received so the other provider can be shown to have received none.
type recordingGateway struct {
	name string

	mu       sync.Mutex
	received []domain.ModelRequest
}

func newRecordingGateway(name string) *recordingGateway { return &recordingGateway{name: name} }

func (g *recordingGateway) Request(_ context.Context, req domain.ModelRequest) (domain.ModelResponse, error) {
	g.mu.Lock()
	defer g.mu.Unlock()
	g.received = append(g.received, req)
	return domain.ModelResponse{
		RequestID:    req.RequestID,
		RouteID:      req.RouteID,
		AccountID:    req.AccountID,
		Message:      domain.Message{Role: domain.RoleAssistant, Content: g.name},
		FinishReason: domain.FinishReasonStop,
		Timestamp:    testNowUnixMilli,
	}, nil
}

func (g *recordingGateway) requests() []domain.ModelRequest {
	g.mu.Lock()
	defer g.mu.Unlock()
	return append([]domain.ModelRequest(nil), g.received...)
}

// countingGateway counts the dispatches it receives. The concurrency test
// replaces a route's binding while requests are in flight, so a per-provider
// request log would be discarded by the next replacement.
type countingGateway struct{ dispatched atomic.Int64 }

func (g *countingGateway) Request(context.Context, domain.ModelRequest) (domain.ModelResponse, error) {
	g.dispatched.Add(1)
	return domain.ModelResponse{}, nil
}

// requestFor builds a minimal request for a route.
func requestFor(route domain.RouteID) domain.ModelRequest {
	return domain.ModelRequest{
		RouteID:   route,
		AccountID: testAccount,
		Messages:  []domain.Message{{Role: domain.RoleUser, Content: "hello"}},
		RequestID: "req-1",
	}
}

func TestRegistry_RequestDispatchesToTheRouteProvider(t *testing.T) {
	registry := NewRegistry(fixedClock{now: testNowUnixMilli})
	httpGateway := newRecordingGateway("http")
	cliGateway := newRecordingGateway("cli")
	registry.Register(httpRoute, httpGateway)
	registry.Register(cliRoute, cliGateway)

	resp, err := registry.Request(context.Background(), requestFor(cliRoute))
	if err != nil {
		t.Fatalf("Request: %v", err)
	}
	if resp.Message.Content != cliGateway.name {
		t.Errorf("reply came from %q, want the cli provider %q", resp.Message.Content, cliGateway.name)
	}
	if got := httpGateway.requests(); len(got) != 0 {
		t.Errorf("http provider received %d requests, want 0", len(got))
	}

	// The request crosses the port unchanged, and the provider's response
	// reaches the caller unchanged.
	got := cliGateway.requests()
	if len(got) != 1 {
		t.Fatalf("cli provider received %d requests, want 1", len(got))
	}
	if got[0].RequestID != "req-1" || got[0].RouteID != cliRoute || got[0].AccountID != testAccount {
		t.Errorf("cli provider received %+v, want the dispatched request unchanged", got[0])
	}
	if resp.RequestID != "req-1" || resp.RouteID != cliRoute || resp.AccountID != testAccount {
		t.Errorf("response %+v, want the provider's response unchanged", resp)
	}
}

func TestRegistry_RequestForUnregisteredRouteFailsClosed(t *testing.T) {
	registry := NewRegistry(fixedClock{now: testNowUnixMilli})
	registry.Register(httpRoute, newRecordingGateway("http"))

	resp, err := registry.Request(context.Background(), requestFor(cliRoute))
	if err == nil {
		t.Fatalf("Request returned %+v, want a model_not_found failure", resp)
	}
	if resp.RequestID != "" {
		t.Errorf("response carries request id %q, want the zero response", resp.RequestID)
	}
	if !errors.Is(err, domain.FailureModelNotFound) {
		t.Errorf("error %v does not classify as %v", err, domain.FailureModelNotFound)
	}

	// The envelope names the route it could not serve and is stamped by the
	// injected clock.
	var envelope domain.ErrorEnvelope
	if !errors.As(err, &envelope) {
		t.Fatalf("error %v is not a domain.ErrorEnvelope", err)
	}
	if envelope.RouteID == nil || *envelope.RouteID != cliRoute {
		t.Errorf("envelope route %v, want %s", envelope.RouteID, cliRoute)
	}
	if envelope.AccountID == nil || *envelope.AccountID != testAccount {
		t.Errorf("envelope account %v, want %s", envelope.AccountID, testAccount)
	}
	if envelope.Timestamp != testNowUnixMilli {
		t.Errorf("envelope timestamp %d, want the clock's %d", envelope.Timestamp, testNowUnixMilli)
	}
}

func TestRegistry_RegisterReplacesTheEarlierProvider(t *testing.T) {
	registry := NewRegistry(fixedClock{now: testNowUnixMilli})
	first := newRecordingGateway("first")
	second := newRecordingGateway("second")
	registry.Register(httpRoute, first)
	registry.Register(httpRoute, second)

	if gateway, ok := registry.Provider(httpRoute); !ok || gateway != second {
		t.Errorf("Provider returned (%v, %t), want the replacement", gateway, ok)
	}
	if registry.Len() != 1 {
		t.Errorf("Len = %d, want 1: a replacement is not a second route", registry.Len())
	}

	if _, err := registry.Request(context.Background(), requestFor(httpRoute)); err != nil {
		t.Fatalf("Request: %v", err)
	}
	if got := first.requests(); len(got) != 0 {
		t.Errorf("replaced provider received %d requests, want 0", len(got))
	}
	if got := second.requests(); len(got) != 1 {
		t.Errorf("replacement provider received %d requests, want 1", len(got))
	}
}

func TestRegistry_LenCountsRoutesWithAProvider(t *testing.T) {
	registry := NewRegistry(nil)
	if got := registry.Len(); got != 0 {
		t.Fatalf("Len = %d before any registration, want 0", got)
	}

	registry.Register(httpRoute, newRecordingGateway("http"))
	if got := registry.Len(); got != 1 {
		t.Errorf("Len = %d after one registration, want 1", got)
	}

	registry.Register(cliRoute, newRecordingGateway("cli"))
	if got := registry.Len(); got != 2 {
		t.Errorf("Len = %d after a second route, want 2", got)
	}

	registry.Register(cliRoute, newRecordingGateway("cli-again"))
	if got := registry.Len(); got != 2 {
		t.Errorf("Len = %d after replacing a route's provider, want 2", got)
	}
}

func TestRegistry_ConcurrentRegisterAndRequest(t *testing.T) {
	const (
		routeCount  = 4
		workerCount = 8
		iterations  = 200
	)

	routeIDs := make([]domain.RouteID, routeCount)
	for i := range routeIDs {
		// Digits are valid Crockford base32, so a zero padded counter yields
		// canonical route identities without a table of literals.
		routeIDs[i] = domain.MustParseRouteID(fmt.Sprintf("rte_%026d", i+1))
	}

	registry := NewRegistry(fixedClock{now: testNowUnixMilli})

	// Every route is bound before the workers start and Register only ever
	// replaces a binding, so a request for one of these routes can never
	// legitimately fail closed.
	var mu sync.Mutex
	registered := make([]*countingGateway, 0, routeCount+workerCount*iterations)
	bind := func(route domain.RouteID, gateway *countingGateway) {
		mu.Lock()
		defer mu.Unlock()
		registered = append(registered, gateway)
		registry.Register(route, gateway)
	}

	for _, route := range routeIDs {
		bind(route, &countingGateway{})
	}

	var wg sync.WaitGroup
	for w := 0; w < workerCount; w++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for i := 0; i < iterations; i++ {
				route := routeIDs[i%routeCount]
				bind(route, &countingGateway{})
				if _, err := registry.Request(context.Background(), requestFor(route)); err != nil {
					// t.Errorf is safe from a worker goroutine; only
					// t.Fatalf has to stay on the test goroutine.
					t.Errorf("Request for the registered route %s failed: %v", route, err)
				}
			}
		}()
	}
	wg.Wait()

	// Every request reached exactly one provider, even though the bindings were
	// being replaced throughout.
	total := 0
	for _, gateway := range registered {
		total += int(gateway.dispatched.Load())
	}
	if want := workerCount * iterations; total != want {
		t.Errorf("providers received %d requests in total, want %d", total, want)
	}
}
