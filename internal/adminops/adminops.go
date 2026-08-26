// Package adminops holds the capability-request approve/deny core shared by
// every operator-facing presenter (the HTML Web UI, the JSON API, the CLI).
// It is the meeting point between requestlog (state) and config (the YAML
// edit that makes an approval effective) — the approve/deny flow used to be
// duplicated in internal/webui and cmd/requests.go; this package is the
// single copy both now call.
package adminops

import (
	"github.com/pti/wraptool/internal/config"
	"github.com/pti/wraptool/internal/requestlog"
)

// ApproveOutcome classifies what approving a request did to the config, once
// status has already flipped to approved.
type ApproveOutcome int

const (
	// OutcomeNewRule means a new allow rule was appended.
	OutcomeNewRule ApproveOutcome = iota
	// OutcomeExtended means an existing allow rule gained additional flags.
	OutcomeExtended
	// OutcomeNoChange means the capability was already allowed; nothing changed.
	OutcomeNoChange
	// OutcomeNotApplied means the caller asked for status-only approval (no
	// config write attempted).
	OutcomeNotApplied
)

// ApproveResult describes the outcome of approving a capability request.
type ApproveResult struct {
	Request *requestlog.CapabilityRequest
	Outcome ApproveOutcome
	// AddedFlags lists flags added by the config edit (new rule or extension).
	// Empty for OutcomeNoChange and OutcomeNotApplied.
	AddedFlags []string
	// Snippet is the YAML representation of the resulting allow rule, suitable
	// for display to the operator. Empty when nothing was written.
	Snippet string
	// ApplyErr is non-nil when status was flipped to approved but the config
	// write failed — the caller renders "approved but edit config by hand"
	// rather than treating the whole operation as failed.
	ApplyErr error
}

// Approve marks the request approved, then — when apply is true — merges the
// capability into the config at configPath via config.ApplyRequest. flagTypes
// is the already-merged flag-type map (each caller owns its own merge: the
// Web UI folds in the operator's per-flag form overrides, the CLI passes
// req.FlagTypes through unchanged).
//
// Status flips to approved before the apply attempt is made, and an apply
// failure is reported via ApplyResult.ApplyErr rather than the returned
// error — the returned error is reserved for a failure to even record the
// approval (unknown request ID, log write failure).
func Approve(store *requestlog.Store, configPath, id string, apply bool, flagTypes map[string]string) (*ApproveResult, error) {
	req, err := store.Get(id)
	if err != nil {
		return nil, err
	}
	if err := store.UpdateStatus(id, requestlog.StatusApproved, ""); err != nil {
		return nil, err
	}

	result := &ApproveResult{Request: req}
	if !apply {
		result.Outcome = OutcomeNotApplied
		return result, nil
	}

	applied, err := config.ApplyRequest(configPath, req.Tool, req.Subcommand, req.Flags, flagTypes)
	if err != nil {
		result.ApplyErr = err
		return result, nil
	}

	result.Snippet = applied.Snippet
	switch {
	case applied.NewRule:
		result.Outcome = OutcomeNewRule
		result.AddedFlags = applied.AddedFlags
	case len(applied.AddedFlags) > 0:
		result.Outcome = OutcomeExtended
		result.AddedFlags = applied.AddedFlags
	default:
		result.Outcome = OutcomeNoChange
	}
	return result, nil
}

// Deny marks the request denied, recording reason (which may be empty).
func Deny(store *requestlog.Store, id, reason string) error {
	return store.UpdateStatus(id, requestlog.StatusDenied, reason)
}
