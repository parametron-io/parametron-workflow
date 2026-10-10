// Package semanticpolicy validates advisory judgement. It grants no workflow authority.
package semanticpolicy

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"slices"

	"github.com/parametron-io/parametron-workflow/internal/semantic"
)

type IssueType string
type Priority string
type Effort string
type Label string
type Estimate int

const (
	Phase    IssueType = "Phase"
	Task     IssueType = "Task"
	Feature  IssueType = "Feature"
	Bug      IssueType = "Bug"
	Critical Priority  = "Critical"
	High     Priority  = "High"
	Medium   Priority  = "Medium"
	Low      Priority  = "Low"
	XS       Effort    = "XS"
	S        Effort    = "S"
	M        Effort    = "M"
	L        Effort    = "L"
	XL       Effort    = "XL"
	Unknown  Effort    = "Unknown"
)

func Types() []IssueType     { return []IssueType{Phase, Task, Feature, Bug} }
func Priorities() []Priority { return []Priority{Critical, High, Medium, Low} }
func Efforts() []Effort      { return []Effort{XS, S, M, L, XL, Unknown} }
func Estimates() []Estimate  { return []Estimate{1, 2, 3, 5, 8, 13} }

// ManagedLabels returns a fresh canonical ordered taxonomy. Helper labels are never owned here.
func ManagedLabels() []Label {
	return []Label{"engine", "freecad", "pdm", "studio", "configurator", "workflow", "dsl", "planning", "runtime", "records", "verification", "adapter", "docs", "infra", "ci", "determinism", "regression", "breaking-change", "compatibility", "security", "performance", "migration", "external"}
}

type IssueClassification struct {
	Type     IssueType `json:"type"`
	Labels   []Label   `json:"labels"`
	Priority Priority  `json:"priority"`
	Effort   Effort    `json:"effort"`
}
type PRClassification struct {
	Labels []Label `json:"labels"`
}
type AcceptedIssue struct {
	Classification IssueClassification
	Provenance     semantic.Provenance
}
type AcceptedPR struct {
	Classification PRClassification
	Provenance     semantic.Provenance
}
type AcceptedEstimate struct {
	Estimate   Estimate
	Provenance semantic.Provenance
}

var (
	ErrMalformed    = errors.New("semantic policy: malformed output")
	ErrMissing      = errors.New("semantic policy: missing field")
	ErrUnknown      = errors.New("semantic policy: unknown field")
	ErrDuplicate    = errors.New("semantic policy: duplicate field")
	ErrType         = errors.New("semantic policy: invalid JSON type or null")
	ErrIssueType    = errors.New("semantic policy: invalid issue type")
	ErrLabel        = errors.New("semantic policy: invalid or duplicate label")
	ErrPriority     = errors.New("semantic policy: invalid priority")
	ErrEffort       = errors.New("semantic policy: invalid effort")
	ErrEstimate     = errors.New("semantic policy: invalid estimate")
	ErrProvenance   = errors.New("semantic policy: capability mismatch")
	ErrPlanning     = errors.New("semantic policy: invalid planning context")
	ErrInapplicable = errors.New("semantic policy: estimate inapplicable")
)

// object checks syntax before inspecting exact fields, including escaped duplicate keys.
func object(raw []byte, fields ...string) (map[string]json.RawMessage, error) {
	if !json.Valid(raw) {
		return nil, ErrMalformed
	}
	d := json.NewDecoder(bytes.NewReader(raw))
	token, err := d.Token()
	if err != nil || token != json.Delim('{') {
		return nil, ErrType
	}
	out := make(map[string]json.RawMessage)
	for d.More() {
		key, err := d.Token()
		if err != nil {
			return nil, ErrMalformed
		}
		name := key.(string)
		if _, ok := out[name]; ok {
			return nil, ErrDuplicate
		}
		if !slices.Contains(fields, name) {
			return nil, ErrUnknown
		}
		var value json.RawMessage
		if d.Decode(&value) != nil {
			return nil, ErrMalformed
		}
		if bytes.Equal(bytes.TrimSpace(value), []byte("null")) {
			return nil, ErrType
		}
		out[name] = value
	}
	if _, err = d.Token(); err != nil {
		return nil, ErrMalformed
	}
	if _, err = d.Token(); err != io.EOF {
		return nil, ErrMalformed
	}
	for _, name := range fields {
		if _, ok := out[name]; !ok {
			return nil, ErrMissing
		}
	}
	return out, nil
}
func decode[T any](raw json.RawMessage) (T, error) {
	var value T
	if json.Unmarshal(raw, &value) != nil {
		return value, ErrType
	}
	return value, nil
}
func normalizeLabels(labels []Label) ([]Label, error) {
	if labels == nil {
		return nil, ErrType
	}
	ordered := ManagedLabels()
	out := make([]Label, 0, len(labels))
	for i, label := range labels {
		if !slices.Contains(ordered, label) || slices.Contains(labels[:i], label) {
			return nil, ErrLabel
		}
	}
	for _, label := range ordered {
		if slices.Contains(labels, label) {
			out = append(out, label)
		}
	}
	return out, nil
}
func validateClassification(c IssueClassification) (IssueClassification, error) {
	if !slices.Contains(Types(), c.Type) {
		return IssueClassification{}, ErrIssueType
	}
	if !slices.Contains(Priorities(), c.Priority) {
		return IssueClassification{}, ErrPriority
	}
	if !slices.Contains(Efforts(), c.Effort) {
		return IssueClassification{}, ErrEffort
	}
	labels, err := normalizeLabels(c.Labels)
	if err != nil {
		return IssueClassification{}, err
	}
	c.Labels = labels
	return c, nil
}
func issue(raw []byte) (IssueClassification, error) {
	o, err := object(raw, "type", "labels", "priority", "effort")
	if err != nil {
		return IssueClassification{}, err
	}
	t, err := decode[IssueType](o["type"])
	if err != nil {
		return IssueClassification{}, err
	}
	l, err := decode[[]Label](o["labels"])
	if err != nil {
		return IssueClassification{}, err
	}
	p, err := decode[Priority](o["priority"])
	if err != nil {
		return IssueClassification{}, err
	}
	e, err := decode[Effort](o["effort"])
	if err != nil {
		return IssueClassification{}, err
	}
	return validateClassification(IssueClassification{t, l, p, e})
}

// DecodeIssueClassification validates durable classification with the same strict
// schema and domains as fresh model output; it performs no semantic execution.
func DecodeIssueClassification(raw []byte) (IssueClassification, error) { return issue(raw) }

// DecodePRClassification is the corresponding durable PR validation boundary.
func DecodePRClassification(raw []byte) (PRClassification, error) {
	o, err := object(raw, "labels")
	if err != nil {
		return PRClassification{}, err
	}
	labels, err := decode[[]Label](o["labels"])
	if err != nil {
		return PRClassification{}, err
	}
	labels, err = normalizeLabels(labels)
	if err != nil {
		return PRClassification{}, err
	}
	return PRClassification{labels}, nil
}

type Service struct{ Runner semantic.Runner }

func (s Service) run(ctx context.Context, c semantic.Capability, input semantic.Input) (semantic.Result, error) {
	if s.Runner == nil {
		return semantic.Result{}, semantic.ErrConfiguration
	}
	r, err := s.Runner.Run(ctx, semantic.Request{Capability: c, Input: input})
	if err != nil {
		return semantic.Result{}, err
	}
	if r.Provenance.Capability != c {
		return semantic.Result{}, ErrProvenance
	}
	return r, nil
}
func (s Service) ClassifyIssue(ctx context.Context, title, body string) (AcceptedIssue, error) {
	r, err := s.run(ctx, semantic.ClassifyIssue, semantic.Input{Title: title, Body: body})
	if err != nil {
		return AcceptedIssue{}, err
	}
	c, err := issue(r.Output)
	if err != nil {
		return AcceptedIssue{}, err
	}
	return AcceptedIssue{c, r.Provenance}, nil
}
func (s Service) ClassifyPR(ctx context.Context, title, body string) (AcceptedPR, error) {
	r, err := s.run(ctx, semantic.ClassifyPR, semantic.Input{Title: title, Body: body})
	if err != nil {
		return AcceptedPR{}, err
	}
	c, err := DecodePRClassification(r.Output)
	if err != nil {
		return AcceptedPR{}, err
	}
	return AcceptedPR{c, r.Provenance}, nil
}
func (s Service) EstimateIssue(ctx context.Context, planning PlanningContext) (AcceptedEstimate, error) {
	input, err := planning.input()
	if err != nil {
		return AcceptedEstimate{}, err
	}
	r, err := s.run(ctx, semantic.EstimateIssue, input)
	if err != nil {
		return AcceptedEstimate{}, err
	}
	o, err := object(r.Output, "estimate")
	if err != nil {
		return AcceptedEstimate{}, err
	}
	value, err := decode[Estimate](o["estimate"])
	if err != nil {
		return AcceptedEstimate{}, err
	}
	if !slices.Contains(Estimates(), value) {
		return AcceptedEstimate{}, ErrEstimate
	}
	return AcceptedEstimate{value, r.Provenance}, nil
}
