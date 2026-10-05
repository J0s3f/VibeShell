package simulation

import (
	"context"
	"encoding/json"

	"j0s.at/vibeshell/internal/domain"
)

type interactionProposeArgs struct {
	View   domain.AppView `json:"view"`
	Reason string         `json:"reason,omitempty"`
}

type interactionProposeResult struct {
	ProposalID string         `json:"proposal_id"`
	View       domain.AppView `json:"view"`
	Status     string         `json:"status"`
}

// interactionPropose validates a declarative interaction description
// against the approved schema — valid view mode and approved local
// primitives only — and returns a proposal. The proposal changes no session
// state: the turn coordinator applies it after commit.
func (r *Registry) interactionPropose(ctx context.Context, call CallContext, raw json.RawMessage) (json.RawMessage, error) {
	var args interactionProposeArgs
	if err := decodeArgs(raw, &args); err != nil {
		return nil, err
	}
	if args.View.Mode == "" {
		return nil, domain.NewValidationError(
			CodeInteractionInvalid, "interaction view requires a mode", nil)
	}
	if err := domain.ValidateView(args.View); err != nil {
		return nil, domain.NewValidationError(
			CodeInteractionInvalid, "interaction view rejected: "+err.Error(), nil)
	}
	proposalID, err := newID("ixn", r.deps.Random)
	if err != nil {
		return nil, err
	}
	return marshalResult(interactionProposeResult{
		ProposalID: proposalID,
		View:       args.View,
		Status:     "proposed",
	})
}
