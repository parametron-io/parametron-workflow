package engineeringterminal

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"reflect"
	"strings"

	"github.com/parametron-io/parametron-workflow/internal/config"
	ec "github.com/parametron-io/parametron-workflow/internal/engineeringcontext"
	"github.com/parametron-io/parametron-workflow/internal/github"
	"github.com/parametron-io/parametron-workflow/internal/observe"
	sp "github.com/parametron-io/parametron-workflow/internal/semanticpolicy"
	"github.com/parametron-io/parametron-workflow/internal/storage"
)

var ErrCorrection = errors.New("engineering terminal: invalid correction provenance")

type CorrectionStore interface {
	Correction(context.Context, string, string) (storage.Correction, error)
	SaveCorrection(context.Context, int64, storage.Correction) error
}

// Version 1 Phase records remain readable. Version 2 adds verified native cycle
// and exact Project-item evidence for actor-neutral terminal corrections. None of
// these explanation records grant native close/reopen or field mutation authority.
type CorrectionEvidence struct {
	Version                     int
	Project                     string
	Graph                       ec.Context
	Rejection                   Rejection
	Applied, Reported, Resolved bool
	Native                      github.IssueClosure
	Item                        observe.ProjectItem
}
type phaseCorrectionV1 struct {
	Version                     int
	Project                     string
	Graph                       ec.Context
	Rejection                   Rejection
	Applied, Reported, Resolved bool
}

func correctionBytes(e CorrectionEvidence) ([]byte, error) {
	if e.Version == 1 {
		return json.Marshal(phaseCorrectionV1{e.Version, e.Project, e.Graph, e.Rejection, e.Applied, e.Reported, e.Resolved})
	}
	return json.Marshal(e)
}
func (c *Controller) validCorrection(e CorrectionEvidence, r storage.Resource) bool {
	return validCorrection(e, r, c.cfg.Project)
}

// Validate checks the bounded producer-output shape for the feedback adapter.
// CurrentCorrection additionally verifies configured Project identity and policy.
func (e CorrectionEvidence) Validate(r storage.Resource) error {
	project := config.ResolvedProject{ID: e.Project, StatusOptions: map[config.StatusRole]string{config.Done: e.Item.Fields[config.Status].OptionID}}
	if !validCorrection(e, r, project) {
		return ErrCorrection
	}
	return nil
}
func validCorrection(e CorrectionEvidence, r storage.Resource, project config.ResolvedProject) bool {
	if e.Project == "" || e.Project != project.ID || e.Rejection.Resource != r {
		return false
	}
	decision, err := Evaluate(e.Graph, r)
	if err != nil {
		return false
	}
	if e.Version == 1 {
		return e.Rejection.Reason == OpenChildren && decision.Reopen && decision.Rejection != nil && *decision.Rejection == e.Rejection
	}
	if e.Version != 2 || r.Kind != "issue" || e.Native.Issue.ID != r.NodeID || e.Native.Issue.Number != int(r.Number) || !strings.EqualFold(e.Native.Issue.Repository.Owner, r.Owner) || !strings.EqualFold(e.Native.Issue.Repository.Name, r.Repository) || e.Native.Validate(e.Native.Issue, e.Native.State) != nil || e.Item.ID == "" {
		return false
	}
	var n ec.IssueNode
	for _, node := range e.Graph.Nodes {
		if node.Resource == r {
			n = node
		}
	}
	if string(n.State) != e.Native.State {
		return false
	}
	switch e.Rejection.Reason {
	case OpenChildren:
		return decision.Reopen && decision.Rejection != nil && *decision.Rejection == e.Rejection
	case UnauthorizedClosure:
		if n.State != ec.Closed {
			return false
		}
		if n.Type == sp.Phase {
			closed, _, _, ok := e.Native.LatestClose()
			return ok && closed.CloserID != ""
		}
		return decision.Reopen && decision.Rejection != nil && decision.Rejection.Reason == UnauthorizedClosure
	case UnauthorizedDone:
		return n.State == ec.Open && e.Item.Fields[config.Status].StatusRole == config.Done && e.Item.Fields[config.Status].OptionID == project.StatusOptions[config.Done]
	}
	return false
}
func (c *Controller) correction(ctx context.Context, r storage.Resource) (storage.Correction, CorrectionEvidence, error) {
	row, err := c.cfg.Corrections.Correction(ctx, "phase_closure_v1", r.NodeID)
	if err != nil {
		return row, CorrectionEvidence{}, err
	}
	var e CorrectionEvidence
	if json.Unmarshal(row.Metadata, &e) != nil {
		return row, e, ErrCorrection
	}
	raw, _ := correctionBytes(e)
	if row.Owner != "phase_closure_v1" || row.ResourceID != r.NodeID || row.Revision <= 0 || !bytes.Equal(raw, row.Metadata) || !c.validCorrection(e, r) {
		return row, e, ErrCorrection
	}
	return row, e, nil
}
func (c *Controller) saveCorrection(ctx context.Context, row storage.Correction, e CorrectionEvidence) error {
	raw, err := correctionBytes(e)
	if err != nil {
		return ErrCorrection
	}
	previous := row.Revision
	row.Revision++
	row.Metadata = raw
	if err := c.cfg.Corrections.SaveCorrection(ctx, previous, row); err != nil {
		if ctx.Err() != nil {
			return ctx.Err()
		}
		if errors.Is(err, storage.ErrConflict) {
			return ec.ErrChanged
		}
		return errors.Join(ErrCorrection, err)
	}
	return nil
}
func lastNative(f github.IssueClosure) (string, string) {
	var closed, reopened string
	for _, e := range f.Items {
		if e.Kind == "ClosedEvent" {
			closed = e.ID
		}
		if e.Kind == "ReopenedEvent" {
			reopened = e.ID
		}
	}
	return closed, reopened
}
func (c *Controller) selectCorrection(ctx context.Context, graph ec.Context, rejection Rejection, native github.IssueClosure, item observe.ProjectItem) error {
	r := rejection.Resource
	row, old, err := c.correction(ctx, r)
	if err != nil && !errors.Is(err, storage.ErrNotFound) {
		return err
	}
	if errors.Is(err, storage.ErrNotFound) {
		row = storage.Correction{Owner: "phase_closure_v1", ResourceID: r.NodeID}
	}
	closed, reopened := lastNative(native)
	oldClose, oldReopen := lastNative(old.Native)
	// An OPEN/Done retry may be the remaining half of a recorded native reopen.
	// Keep the original rejected closure explanation rather than replacing it
	// with residual Project drift after a lost response or interrupted Status write.
	if err == nil && !old.Reported && !old.Resolved && rejection.Reason == UnauthorizedDone && old.Version == 2 && old.Native.State == "CLOSED" && native.State == "OPEN" && closed == oldClose && old.Item.ID == item.ID {
		return nil
	}
	if err == nil && !old.Reported && !old.Resolved && old.Rejection == rejection && old.Version == 2 && closed == oldClose && reopened == oldReopen && old.Item.ID == item.ID {
		return nil
	}
	e := CorrectionEvidence{Version: 2, Project: c.cfg.Project.ID, Graph: graph, Rejection: rejection, Native: native, Item: item}
	if !c.validCorrection(e, r) {
		return ErrCorrection
	}
	return c.saveCorrection(ctx, row, e)
}
func (c *Controller) markCorrection(ctx context.Context, r storage.Resource, reported bool) error {
	row, e, err := c.correction(ctx, r)
	if err != nil {
		return err
	}
	if reported {
		e.Reported = true
	} else {
		e.Applied = true
	}
	return c.saveCorrection(ctx, row, e)
}

// CurrentCorrection performs no lifecycle mutation. Current typed rejection or a
// still-pending explanation is bound to fresh owner policy, membership and cycle.
// Reporting, a superseding cycle, valid completion, type/routing changes, or the
// Phase prerequisite becoming satisfied retires old explanation evidence.
func (c *Controller) CurrentCorrection(ctx context.Context, r storage.Resource) (*CorrectionEvidence, error) {
	if c.cfg.Corrections == nil {
		return nil, ErrCorrection
	}
	_, e, err := c.correction(ctx, r)
	stored := err == nil
	if err != nil && !errors.Is(err, storage.ErrNotFound) {
		return nil, err
	}
	retire := func() (*CorrectionEvidence, error) {
		if !stored || e.Resolved || e.Reported {
			return nil, nil
		}
		row, latest, err := c.correction(ctx, r)
		if err != nil {
			return nil, err
		}
		if !reflect.DeepEqual(latest, e) {
			return nil, ec.ErrChanged
		}
		latest.Resolved = true
		return nil, c.saveCorrection(ctx, row, latest)
	}
	s, err := c.cfg.Snapshots.ResolveSnapshot(ctx)
	if err != nil {
		return nil, err
	}
	graph, err := s.Context(r)
	if err != nil {
		return nil, err
	}
	supported := false
	for _, node := range graph.Nodes {
		if node.Resource == r {
			supported = node.Type == sp.Phase || node.Type == sp.Task || node.Type == sp.Feature
		}
	}
	if !supported {
		return retire()
	}
	o, item, managed, err := c.item(ctx, s, r)
	if err != nil {
		return nil, err
	}
	if !managed {
		return retire()
	}
	decision, native, err := c.authorize(ctx, graph, r, o.Issue.Identity, "", false)
	if err != nil {
		return nil, err
	}
	var current *Rejection
	if decision.Reopen {
		current = decision.Rejection
	} else if native.State == "OPEN" && item.Fields[config.Status].OptionID == c.cfg.Project.StatusOptions[config.Done] {
		current = &Rejection{Resource: r, Reason: UnauthorizedDone}
	}
	if current != nil {
		if stored && !e.Reported && !e.Resolved {
			closed, reopened := lastNative(native)
			oldClose, oldReopen := lastNative(e.Native)
			sameCycle := e.Version == 1 || item.ID == e.Item.ID && closed == oldClose && (e.Rejection.Reason != UnauthorizedDone || reopened == oldReopen)
			if sameCycle && (e.Rejection == *current || current.Reason == UnauthorizedDone && e.Version == 2 && e.Native.State == "CLOSED") {
				return &e, ctx.Err()
			}
		}
		selected := CorrectionEvidence{Version: 2, Project: c.cfg.Project.ID, Graph: graph, Rejection: *current, Native: native, Item: item}
		if !c.validCorrection(selected, r) {
			return nil, ErrCorrection
		}
		return &selected, ctx.Err()
	}
	if !stored || e.Reported || e.Resolved {
		return nil, nil
	}
	if native.State == "CLOSED" || e.Version == 2 && item.ID != e.Item.ID {
		return retire()
	}
	if e.Rejection.Reason == OpenChildren {
		hypothetical := graph
		hypothetical.Nodes = append([]ec.IssueNode(nil), graph.Nodes...)
		for n := range hypothetical.Nodes {
			if hypothetical.Nodes[n].Resource == r {
				hypothetical.Nodes[n].State = ec.Closed
			}
		}
		gate, err := Evaluate(hypothetical, r)
		if err != nil {
			return nil, err
		}
		if gate.Rejection == nil || gate.Rejection.Reason != OpenChildren {
			return retire()
		}
	}
	if e.Version == 2 {
		closed, reopened := lastNative(native)
		oldClose, oldReopen := lastNative(e.Native)
		if closed != oldClose || e.Rejection.Reason == UnauthorizedDone && reopened != oldReopen {
			return retire()
		}
	}
	return &e, ctx.Err()
}
func (c *Controller) AcknowledgeCorrection(ctx context.Context, r storage.Resource, expected CorrectionEvidence) error {
	if c.cfg.Corrections == nil {
		return ErrCorrection
	}
	_, e, err := c.correction(ctx, r)
	if errors.Is(err, storage.ErrNotFound) || err == nil && e.Reported {
		return nil
	}
	if err != nil {
		return err
	}
	if !reflect.DeepEqual(e, expected) {
		return ec.ErrChanged
	}
	return c.markCorrection(ctx, r, true)
}
