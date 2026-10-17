package prcompanionreconcile

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"math"
	"reflect"
	"strconv"
	"strings"
	"time"

	"github.com/parametron-io/parametron-workflow/internal/config"
	eo "github.com/parametron-io/parametron-workflow/internal/engineeringorder"
	"github.com/parametron-io/parametron-workflow/internal/github"
	"github.com/parametron-io/parametron-workflow/internal/prbinding"
	pc "github.com/parametron-io/parametron-workflow/internal/prcompanion"
	"github.com/parametron-io/parametron-workflow/internal/storage"
)

const management = "engineering_pr_roadmap_number_v1"
const prOwnershipLimit = 4096

// Acquisition is retained after every update and clear. Its completed receipt
// establishes continuing management of THIS item/field, not a permanent Target
// authorization or a historic numeric grant. Replacement items are never adopted.
// A missing first receipt is ambiguous even if the observed number matches.
type record struct {
	Version              int
	Management           string
	PR                   github.Identity
	Project, Item, Field string
	Acquisition          attempt
	Attempt              attempt
	Revision             int64
}
type attempt struct {
	DeliveryID             string
	Origin                 storage.Resource
	State                  pc.State
	Binding                prbinding.Decision
	Value                  *int
	StartedAt, CompletedAt time.Time
}

func fmtNumber(v int64) string { return strconv.FormatInt(v, 10) }
func (c *Controller) origin(ctx context.Context, id string) (storage.Resource, error) {
	e, err := c.cfg.Store.Event(ctx, id)
	if errors.Is(err, storage.ErrNotFound) {
		return storage.Resource{}, ErrOwnership
	}
	if err != nil {
		return storage.Resource{}, storeError(err)
	}
	if e.Delivery.ID != id || e.Resource == nil || e.Sequence <= 0 {
		return storage.Resource{}, ErrOwnership
	}
	r := *e.Resource
	if (r.Kind != "issue" && r.Kind != "pull_request") || r.Number <= 0 || r.Owner != strings.ToLower(r.Owner) || r.Repository != strings.ToLower(r.Repository) {
		return storage.Resource{}, ErrOwnership
	}
	configured := false
	for _, repo := range c.cfg.Repositories {
		if r.Owner == strings.ToLower(repo.Owner) && r.Repository == strings.ToLower(repo.Name) {
			configured = true
		}
	}
	if !configured {
		return storage.Resource{}, ErrOwnership
	}
	return r, nil
}
func storeError(err error) error {
	if err == nil {
		return nil
	}
	if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
		return err
	}
	if errors.Is(err, storage.ErrConflict) {
		return ErrOwnership
	}
	return errors.Join(ErrStorage, err)
}
func (c *Controller) validAttempt(ctx context.Context, a attempt, pr, originalTarget github.Identity, acquisition bool) error {
	if !validID(a.DeliveryID) || a.StartedAt.IsZero() || !a.CompletedAt.IsZero() && a.CompletedAt.Before(a.StartedAt) {
		return ErrOwnership
	}
	origin, err := c.origin(ctx, a.DeliveryID)
	if err != nil {
		return err
	}
	if origin != a.Origin {
		return ErrOwnership
	}
	maximum := eo.FirstRoadmapBase + eo.MaxRoadmapSegments*eo.RoadmapStride - 1
	if a.Value != nil && (*a.Value <= eo.FirstRoadmapBase || *a.Value > maximum || *a.Value%eo.DirectChildStride == 0) {
		return ErrOwnership
	}
	if a.Binding.Outcome == prbinding.Accepted {
		b := a.Binding.Evidence
		if a.Binding.Reason != "" || b.PR != pr || !c.configured(b.Target) || b.Policy != "active_org_member_repository_write_assignee_v1" || !github.ValidHuman(b.Developer) || b.Branch.Repository != b.Target.Repository || !github.ValidBranchName(b.Branch.Name) || !github.ValidCommitSHA(b.Branch.SHA) || (b.Kind != "Phase" && b.Kind != "Task" && b.Kind != "Feature") {
			return ErrOwnership
		}
	} else if a.Value != nil || a.Binding.Outcome != prbinding.Rejected || a.Binding.Evidence != (prbinding.Evidence{}) {
		return ErrOwnership
	}
	if acquisition && (a.Value == nil || a.Binding.Outcome != prbinding.Accepted) {
		return ErrOwnership
	}
	if a.State != pc.Open && a.State != pc.Closed && a.State != pc.Merged {
		return ErrOwnership
	}
	if a.Value != nil && (a.State != pc.Open || a.Binding.Outcome != prbinding.Accepted) {
		return ErrOwnership
	}
	// Reuse #48's structural binding contract for historical records too.
	// CLOSED here is validation-only: this never supplies current eligibility.
	if _, err := pc.Evaluate(pc.Input{PullRequests: []pc.PullRequest{{Resource: resource(pr, "pull_request"), Identity: pr, State: pc.Closed, Binding: a.Binding}}}); err != nil {
		return ErrOwnership
	}
	if a.Value == nil {
		plan, err := pc.Evaluate(pc.Input{PullRequests: []pc.PullRequest{{Resource: resource(pr, "pull_request"), Identity: pr, State: a.State, Binding: a.Binding}}, Owned: []pc.OwnedField{{PR: pr, Target: originalTarget}}})
		if err != nil || len(plan.Clear) != 1 || plan.Clear[0] != resource(pr, "pull_request") {
			return ErrOwnership
		}
	}
	return nil
}
func (c *Controller) ownership(ctx context.Context) (map[storage.Resource]record, error) {
	rows, err := c.cfg.Store.CompanionFields(ctx, c.cfg.Project.ID, c.cfg.Project.Fields[config.RoadmapOrder])
	if err != nil {
		return nil, storeError(err)
	}
	if len(rows) > prOwnershipLimit {
		return nil, ErrOwnership
	}
	out := map[storage.Resource]record{}
	items := map[string]bool{}
	names := map[string]bool{}
	for _, row := range rows {
		var o record
		if len(row.Metadata) > 32768 || json.Unmarshal(row.Metadata, &o) != nil {
			return nil, ErrOwnership
		}
		canonical, err := json.Marshal(o)
		if err != nil || !bytes.Equal(canonical, row.Metadata) {
			return nil, ErrOwnership
		}
		r := resource(o.PR, "pull_request")
		if o.Version != 1 || o.Management != management || !c.configured(o.PR) || o.PR.Repository.Owner != r.Owner || o.PR.Repository.Name != r.Repository || o.Project != c.cfg.Project.ID || o.Field != c.cfg.Project.Fields[config.RoadmapOrder] || !validID(o.Item) || o.Item == o.PR.ID || o.Item == o.Project || o.Item == o.Field || row.Key != key(r) || row.Project != o.Project || row.Field != o.Field || row.Item != o.Item || row.Revision != o.Revision || o.Revision <= 0 || o.Revision == math.MaxInt64 || items[o.Item] || names[key(r)] {
			return nil, ErrOwnership
		}
		if err := c.validAttempt(ctx, o.Acquisition, o.PR, o.Acquisition.Binding.Evidence.Target, true); err != nil {
			return nil, err
		}
		if err := c.validAttempt(ctx, o.Attempt, o.PR, o.Acquisition.Binding.Evidence.Target, false); err != nil {
			return nil, err
		}
		if o.Acquisition.CompletedAt.IsZero() {
			if o.Revision != 1 || !reflect.DeepEqual(o.Acquisition, o.Attempt) {
				return nil, ErrOwnership
			}
		} else if o.Revision < 2 || o.Revision == 2 && !reflect.DeepEqual(o.Acquisition, o.Attempt) || o.Revision > 2 && o.Attempt.StartedAt.Before(o.Acquisition.CompletedAt) {
			return nil, ErrOwnership
		}
		items[o.Item], names[key(r)] = true, true
		out[r] = o
	}
	return out, ctx.Err()
}
func (c *Controller) save(ctx context.Context, o *record) error {
	old := o.Revision
	if old == math.MaxInt64 {
		return ErrOwnership
	}
	o.Revision++
	raw, err := json.Marshal(o)
	if err != nil || len(raw) > 32768 {
		return ErrOwnership
	}
	return storeError(c.cfg.Store.SaveCompanionField(ctx, old, storage.CompanionField{Key: key(resource(o.PR, "pull_request")), Item: o.Item, Project: o.Project, Field: o.Field, Revision: o.Revision, Metadata: raw}))
}

// A persisted attempt grants no effects. This special read permits only the exact
// in-flight first acquisition written by this invocation, and authenticates it
// against a fresh complete plan before the first GitHub call. Public retries do
// not receive this exception and fail closed on an unreceipted acquisition.
func (c *Controller) loadPending(ctx context.Context, o record, before view) (view, error) {
	after, err := c.loadWithPending(ctx, &o)
	if err != nil {
		return view{}, err
	}
	expected := before
	expected.Ownership = make(map[storage.Resource]record, len(before.Ownership)+1)
	for r, p := range before.Ownership {
		expected.Ownership[r] = p
	}
	expected.Ownership[resource(o.PR, "pull_request")] = o
	if !reflect.DeepEqual(expected, after) {
		return view{}, ErrChanged
	}
	return after, nil
}
