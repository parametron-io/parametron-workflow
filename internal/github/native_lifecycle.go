package github

import (
	"context"
	"encoding/json"
	"errors"
	"time"
)

var ErrNativeLifecycleChanged = errors.New("github: native lifecycle changed during reads")

// NativeTransition is bounded native close/reopen/merge evidence, not authority.
type NativeTransition struct {
	ID, Kind  string
	Resource  Identity
	CreatedAt time.Time
}
type NativeLifecycle struct {
	Resource    Identity
	Kind, State string
	Transitions []NativeTransition
}
type NativeLifecycleReader interface {
	NativeLifecycle(context.Context, Identity, string) (NativeLifecycle, error)
}

// IssueStates exposes only exact native Issue state changes. A successful return
// is a validated mutation response, not an inference from a later observation.
type IssueStates interface {
	CloseIssue(context.Context, Identity) (IssueStateReceipt, error)
	ReopenIssue(context.Context, Identity) (IssueStateReceipt, error)
}
type IssueStateReceipt struct {
	Issue Identity
	State string
}

func (t *Transport) nativeIdentity(ctx context.Context, i Identity, kind string) (string, error) {
	if !ValidBranchRepository(i.Repository) || !validIDs(i.ID) || i.Number <= 0 {
		return "", failure(Permanent)
	}
	field := "issue"
	if kind == "pull_request" {
		field = "pullRequest"
	} else if kind != "issue" {
		return "", failure(Permanent)
	}
	var w struct {
		wireIdentity
		State string
	}
	if err := t.resource(ctx, Ref{Owner: i.Repository.Owner, Repository: i.Repository.Name, Number: i.Number}, field, identitySelection+` state`, &w); err != nil {
		return "", err
	}
	current, err := w.wireIdentity.normalized()
	if err != nil || current != i || (w.State != "OPEN" && w.State != "CLOSED" && (kind != "pull_request" || w.State != "MERGED")) {
		return "", failure(Malformed)
	}
	return w.State, ctx.Err()
}
func (t *Transport) NativeLifecycle(ctx context.Context, i Identity, kind string) (NativeLifecycle, error) {
	state, err := t.nativeIdentity(ctx, i, kind)
	if err != nil {
		return NativeLifecycle{}, err
	}
	typ, types := "Issue", ",itemTypes:[CLOSED_EVENT,REOPENED_EVENT]"
	if kind == "pull_request" {
		typ, types = "PullRequest", ",itemTypes:[CLOSED_EVENT,REOPENED_EVENT,MERGED_EVENT]"
	}
	selection := `__typename ... on ClosedEvent{id createdAt closable{__typename ... on Issue{` + identitySelection + `} ... on PullRequest{` + identitySelection + `}}} ... on ReopenedEvent{id createdAt closable{__typename ... on Issue{` + identitySelection + `} ... on PullRequest{` + identitySelection + `}}}`
	if kind == "pull_request" {
		selection += ` ... on MergedEvent{id createdAt pullRequest{` + identitySelection + `}}`
	}
	nodes, err := t.connectionWithExtra(ctx, i.ID, typ, "timelineItems", selection, types)
	if err != nil {
		return NativeLifecycle{}, err
	}
	out := NativeLifecycle{Resource: i, Kind: kind, State: state, Transitions: []NativeTransition{}}
	seen := map[string]bool{}
	var at time.Time
	for _, raw := range nodes {
		var w struct {
			ID          string
			Kind        string `json:"__typename"`
			CreatedAt   time.Time
			Closable    *wireIdentity
			PullRequest *wireIdentity
		}
		if json.Unmarshal(raw, &w) != nil || !validIDs(w.ID) || w.CreatedAt.IsZero() || seen[w.ID] || (!at.IsZero() && !w.CreatedAt.After(at)) {
			return NativeLifecycle{}, failure(Malformed)
		}
		identity := w.Closable
		if w.Kind == "MergedEvent" && kind == "pull_request" {
			identity = w.PullRequest
		} else if w.Kind != "ClosedEvent" && w.Kind != "ReopenedEvent" {
			return NativeLifecycle{}, failure(Malformed)
		}
		if identity == nil {
			return NativeLifecycle{}, failure(Malformed)
		}
		current, err := identity.normalized()
		if err != nil || current != i {
			return NativeLifecycle{}, failure(Malformed)
		}
		out.Transitions = append(out.Transitions, NativeTransition{ID: w.ID, Kind: w.Kind, Resource: i, CreatedAt: w.CreatedAt})
		seen[w.ID], at = true, w.CreatedAt
	}
	final, err := t.nativeIdentity(ctx, i, kind)
	if err != nil {
		return NativeLifecycle{}, err
	}
	if final != state {
		return NativeLifecycle{}, ErrNativeLifecycleChanged
	}
	if err := out.Validate(i, kind, state); err != nil {
		return NativeLifecycle{}, err
	}
	return out, ctx.Err()
}
func (t *Transport) issueState(ctx context.Context, i Identity, close bool) (IssueStateReceipt, error) {
	state, err := t.nativeIdentity(ctx, i, "issue")
	if err != nil {
		return IssueStateReceipt{}, err
	}
	from, to, operation, input := "OPEN", "CLOSED", "closeIssue", "CloseIssueInput"
	if !close {
		from, to, operation, input = "CLOSED", "OPEN", "reopenIssue", "ReopenIssueInput"
	}
	// Callers compare drift first. A raced no-op is not a mutation receipt.
	if state != from {
		return IssueStateReceipt{}, ErrNativeLifecycleChanged
	}
	var data struct {
		Result *struct {
			Issue *struct {
				wireIdentity
				State string
			}
		}
	}
	if err := ctx.Err(); err != nil {
		return IssueStateReceipt{}, err
	}
	q := `mutation($input:` + input + `!){result:` + operation + `(input:$input){issue{` + identitySelection + ` state}}}`
	if err := t.query(ctx, q, map[string]any{"input": map[string]any{"issueId": i.ID}}, &data); err != nil {
		return IssueStateReceipt{}, err
	}
	if data.Result == nil || data.Result.Issue == nil {
		return IssueStateReceipt{}, failure(Malformed)
	}
	actual, err := data.Result.Issue.wireIdentity.normalized()
	if err != nil || actual != i || data.Result.Issue.State != to {
		return IssueStateReceipt{}, failure(Malformed)
	}
	return IssueStateReceipt{Issue: i, State: to}, ctx.Err()
}
func (t *Transport) CloseIssue(ctx context.Context, i Identity) (IssueStateReceipt, error) {
	return t.issueState(ctx, i, true)
}
func (t *Transport) ReopenIssue(ctx context.Context, i Identity) (IssueStateReceipt, error) {
	return t.issueState(ctx, i, false)
}

// Validate checks a bounded reader result, including fake/provider output. It
// establishes identity and supported native chronology, never mutation authority.
func (f NativeLifecycle) Validate(i Identity, kind, state string) error {
	if !ValidBranchRepository(i.Repository) || !validIDs(i.ID) || i.Number <= 0 || kind != "issue" && kind != "pull_request" || state != "OPEN" && state != "CLOSED" && (kind != "pull_request" || state != "MERGED") {
		return failure(Malformed)
	}
	if f.Resource != i || f.Kind != kind || f.State != state {
		return ErrNativeLifecycleChanged
	}
	seen := map[string]bool{}
	var at time.Time
	for _, e := range f.Transitions {
		if e.Resource != i || !validIDs(e.ID) || e.CreatedAt.IsZero() || seen[e.ID] || !at.IsZero() && !e.CreatedAt.After(at) {
			return failure(Malformed)
		}
		if e.Kind != "ClosedEvent" && e.Kind != "ReopenedEvent" && (kind != "pull_request" || e.Kind != "MergedEvent") {
			return failure(Malformed)
		}
		seen[e.ID], at = true, e.CreatedAt
	}
	if len(f.Transitions) == 0 {
		if state != "OPEN" {
			return failure(Malformed)
		}
		return nil
	}
	last := f.Transitions[len(f.Transitions)-1].Kind
	if state == "OPEN" && last == "ReopenedEvent" || state == "CLOSED" && last == "ClosedEvent" || kind == "pull_request" && state == "MERGED" && last == "MergedEvent" {
		return nil
	}
	return ErrNativeLifecycleChanged
}
