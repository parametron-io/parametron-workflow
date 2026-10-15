package engineeringterminal

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"reflect"

	ec "github.com/parametron-io/parametron-workflow/internal/engineeringcontext"
	sp "github.com/parametron-io/parametron-workflow/internal/semanticpolicy"
	"github.com/parametron-io/parametron-workflow/internal/storage"
)

var ErrCorrection = errors.New("engineering terminal: invalid correction provenance")

type CorrectionStore interface {
	Correction(context.Context, string, string) (storage.Correction, error)
	SaveCorrection(context.Context, int64, storage.Correction) error
}

// CorrectionEvidence retains the actual pre-correction rejection and its graph.
// Applied is a verified reopen receipt; Reported is verified comment acknowledgment.
// Neither field grants permission to repeat native or Project mutations.
type CorrectionEvidence struct {
	Version                     int
	Project                     string
	Graph                       ec.Context
	Rejection                   Rejection
	Applied, Reported, Resolved bool
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
	raw, _ := json.Marshal(e)
	decision, err := Evaluate(e.Graph, r)
	if row.Owner != "phase_closure_v1" || row.ResourceID != r.NodeID || row.Revision <= 0 || !bytes.Equal(raw, row.Metadata) || e.Version != 1 || e.Project != c.cfg.Project.ID || e.Rejection.Resource != r || err != nil || !decision.Reopen || decision.Rejection == nil || *decision.Rejection != e.Rejection {
		return row, e, ErrCorrection
	}
	return row, e, nil
}
func (c *Controller) saveCorrection(ctx context.Context, row storage.Correction, e CorrectionEvidence) error {
	raw, err := json.Marshal(e)
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
func (c *Controller) selectCorrection(ctx context.Context, graph ec.Context, rejection Rejection) error {
	r := rejection.Resource
	row, old, err := c.correction(ctx, r)
	if err != nil && !errors.Is(err, storage.ErrNotFound) {
		return err
	}
	if errors.Is(err, storage.ErrNotFound) {
		row = storage.Correction{Owner: "phase_closure_v1", ResourceID: r.NodeID}
	}
	if err == nil && !old.Reported && !old.Resolved {
		return nil
	}
	return c.saveCorrection(ctx, row, CorrectionEvidence{Version: 1, Project: c.cfg.Project.ID, Graph: graph, Rejection: rejection})
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

// CurrentCorrection rechecks managed membership and the current direct-child
// prerequisite. A hypothetical CLOSED state asks only whether the explanation
// remains relevant after an authorized reopen; it grants no mutation authority.
// Reporting or satisfying that prerequisite retires the explanation.
func (c *Controller) CurrentCorrection(ctx context.Context, r storage.Resource) (*CorrectionEvidence, error) {
	if c.cfg.Corrections == nil {
		return nil, ErrCorrection
	}
	_, e, err := c.correction(ctx, r)
	if errors.Is(err, storage.ErrNotFound) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	if e.Reported || e.Resolved {
		return nil, nil
	}
	retire := func() (*CorrectionEvidence, error) {
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
	phase := false
	for i, n := range graph.Nodes {
		if n.Resource == r {
			if n.Type != sp.Phase {
				return retire()
			}
			phase = true
			graph.Nodes = append([]ec.IssueNode(nil), graph.Nodes...)
			graph.Nodes[i].State = ec.Closed
		}
	}
	if !phase {
		return nil, ec.ErrChanged
	}
	_, _, managed, err := c.item(ctx, s, r)
	if err != nil {
		return nil, err
	}
	if !managed {
		return retire()
	}
	decision, err := Evaluate(graph, r)
	if err != nil {
		return nil, err
	}
	if !decision.Reopen || decision.Rejection == nil || *decision.Rejection != e.Rejection {
		return retire()
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
