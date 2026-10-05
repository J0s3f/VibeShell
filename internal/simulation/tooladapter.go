package simulation

import (
	"context"
	"errors"
	"strings"

	"j0s.at/vibeshell/internal/application"
	"j0s.at/vibeshell/internal/domain"
	"j0s.at/vibeshell/internal/ports"
)

// NamespaceResolver resolves the trusted namespace IDs for a turn. The
// composition root supplies it; namespace IDs are never taken from model input.
type NamespaceResolver interface {
	Namespaces(ctx context.Context, req application.TurnRequest) (CallNamespaces, error)
}

// ToolAdapter adapts the server-owned tool Registry to the application's
// ToolExecutor seam.
//
// It is the single place where an application tool batch becomes registry
// calls, so it is also where the trust boundary is kept: identity, working
// directory, scope policy, prompt version, namespace IDs, and the call's
// timestamp all come from the pinned TurnRequest and the injected resolver,
// while only the tool name, arguments, and proposed scope come from the model.
// The tool arguments are forwarded byte for byte; each handler validates them.
//
// A model-proposed per-call timeout is deliberately ignored: it is untrusted
// input and must not shorten or extend the turn deadline the coordinator owns.
type ToolAdapter struct {
	Registry   *Registry
	Namespaces NamespaceResolver
	Clock      ports.Clock
	UID, GID   uint32
}

var _ application.ToolExecutor = (*ToolAdapter)(nil)

// Execute builds one trusted CallContext for the batch and runs every
// requested call through the registry, in request order.
//
// A tool failure is reported as a typed error on that call's result, so one
// bad call neither hides the others nor fails the batch: the model sees the
// error and decides what to do next. Only a failure to establish the trusted
// call context — an unusable adapter composition or a namespace resolution
// failure — aborts the batch, because then no call could be authorized.
func (a *ToolAdapter) Execute(ctx context.Context, batch application.ToolBatch) (application.ToolBatchResult, error) {
	if err := a.checkComplete(); err != nil {
		return application.ToolBatchResult{}, err
	}
	namespaces, err := a.Namespaces.Namespaces(ctx, batch.Request)
	if err != nil {
		return application.ToolBatchResult{}, err
	}
	call := CallContext{
		SessionID:     batch.Request.Session,
		UserID:        batch.Request.Principal,
		CWD:           batch.Request.Context.CWD,
		Policy:        batch.Request.Snapshot.ScopePolicy,
		Namespaces:    namespaces,
		UID:           a.UID,
		GID:           a.GID,
		TurnID:        batch.Request.Turn,
		AttemptID:     batch.Request.Attempt,
		PromptVersion: batch.Request.Snapshot.PromptVersion,
		NowUnixMilli:  a.Clock.NowUnixMilli(),
	}

	results := make([]application.ToolCallResult, 0, len(batch.Calls))
	for _, requested := range batch.Calls {
		results = append(results, a.executeOne(ctx, call, requested))
	}
	return application.ToolBatchResult{Results: results}, nil
}

// executeOne runs a single requested call and folds its outcome into a result
// slot. The batch executor depends on the result carrying both outcomes, so a
// failure is never collapsed into a missing entry.
func (a *ToolAdapter) executeOne(ctx context.Context, call CallContext, requested application.ToolCallRequest) application.ToolCallResult {
	result, err := a.Registry.Execute(ctx, call, ToolCall{Name: requested.Name, Arguments: requested.Arguments})
	if err != nil {
		return application.ToolCallResult{Name: requested.Name, Error: asToolError(err)}
	}
	return application.ToolCallResult{Name: requested.Name, Result: result.Result}
}

// checkComplete rejects an incomplete composition at the batch boundary, so a
// misconfigured adapter fails with a typed error instead of panicking inside a
// session goroutine.
func (a *ToolAdapter) checkComplete() error {
	missing := make([]string, 0, 3)
	if a.Registry == nil {
		missing = append(missing, "Registry")
	}
	if a.Namespaces == nil {
		missing = append(missing, "Namespaces")
	}
	if a.Clock == nil {
		missing = append(missing, "Clock")
	}
	if len(missing) == 0 {
		return nil
	}
	return domain.NewInternalError(domain.CodeInvariantViolation,
		"tool adapter is missing required dependencies: "+strings.Join(missing, ", "), nil)
}

// asToolError normalizes a tool failure into the typed error the application
// seam carries per call. Handlers are expected to return *domain.DomainError;
// anything else would leak a bare Go error to the model, so it is reported as
// an internal failure with the original error kept for diagnosis.
func asToolError(err error) *domain.DomainError {
	var typed *domain.DomainError
	if errors.As(err, &typed) {
		return typed
	}
	return domain.NewInternalError(domain.CodeInvariantViolation, "tool call failed without a typed error", err)
}
