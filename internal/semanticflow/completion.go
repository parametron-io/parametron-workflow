package semanticflow

import (
	"bytes"
	"context"
	"encoding/json"
	"strings"

	"github.com/parametron-io/parametron-workflow/internal/semantic"
	"github.com/parametron-io/parametron-workflow/internal/semanticpolicy"
	"github.com/parametron-io/parametron-workflow/internal/storage"
)

const Namespace = "semantic.classification"

// This envelope is a durable compatibility contract, unlike prompt filenames.
type completion struct {
	Format      int                                 `json:"format"`
	Resource    resource                            `json:"resource"`
	InputDigest string                              `json:"input_digest"`
	Issue       *semanticpolicy.IssueClassification `json:"issue,omitempty"`
	PR          *semanticpolicy.PRClassification    `json:"pr,omitempty"`
	Provenance  provenance                          `json:"provenance"`
}
type provenance struct {
	Capability     semantic.Capability `json:"capability"`
	Provider       string              `json:"provider"`
	Model          string              `json:"model"`
	PromptIdentity string              `json:"prompt_identity"`
	PromptDigest   string              `json:"prompt_digest"`
	SchemaIdentity string              `json:"schema_identity"`
	SchemaDigest   string              `json:"schema_digest"`
}

func validDigest(s string) bool {
	if len(s) != 71 || !strings.HasPrefix(s, "sha256:") {
		return false
	}
	for _, ch := range s[7:] {
		if !(ch >= '0' && ch <= '9' || ch >= 'a' && ch <= 'f') {
			return false
		}
	}
	return true
}
func (p provenance) valid(cap semantic.Capability) bool {
	return p.Capability == cap && (semantic.DeploymentConfig{Cheap: semantic.Selection{Provider: p.Provider, Model: p.Model}}).Validate() == nil &&
		p.PromptIdentity == "prompts/cheap/"+string(cap)+".txt" && p.SchemaIdentity == "schemas/model/"+string(cap)+".json" && validDigest(p.PromptDigest) && validDigest(p.SchemaDigest)
}
func decodeCompletion(data []byte, expected resource) (Classification, error) {
	fail := func() (Classification, error) { return Classification{}, ErrCompletion }
	if len(data) > 16384 {
		return fail()
	}
	var record completion
	if json.Unmarshal(data, &record) != nil || record.Format != 1 || record.Resource != expected || !validDigest(record.InputDigest) {
		return fail()
	}
	// Exact canonical bytes reject unknown/missing/duplicate/case-variant fields,
	// nulls, noncanonical label order, and alternate numeric representations.
	canonical, err := json.Marshal(record)
	if err != nil || !bytes.Equal(canonical, data) {
		return fail()
	}
	if expected.Kind == "issue" {
		if record.Issue == nil || record.PR != nil || !record.Provenance.valid(semantic.ClassifyIssue) {
			return fail()
		}
		raw, _ := json.Marshal(record.Issue)
		value, err := semanticpolicy.DecodeIssueClassification(raw)
		if err != nil {
			return fail()
		}
		normalized, _ := json.Marshal(value)
		if !bytes.Equal(raw, normalized) {
			return fail()
		}
		return Classification{state: Accepted, issue: &semanticpolicy.AcceptedIssue{Classification: value, Provenance: semantic.Provenance(record.Provenance)}}, nil
	}
	if record.PR == nil || record.Issue != nil || !record.Provenance.valid(semantic.ClassifyPR) {
		return fail()
	}
	raw, _ := json.Marshal(record.PR)
	value, err := semanticpolicy.DecodePRClassification(raw)
	if err != nil {
		return fail()
	}
	normalized, _ := json.Marshal(value)
	if !bytes.Equal(raw, normalized) {
		return fail()
	}
	return Classification{state: Accepted, pr: &semanticpolicy.AcceptedPR{Classification: value, Provenance: semantic.Provenance(record.Provenance)}}, nil
}
func (c *Coordinator) load(ctx context.Context, p storage.Provenance, r resource) (Classification, error) {
	key, _ := ResourceKey(storage.Resource{Owner: r.Owner, Repository: r.Repository, Kind: r.Kind, Number: r.Number})
	if p.Namespace != Namespace || p.Key != key || p.DeliveryID == "" || p.CreatedAt.IsZero() {
		return Classification{}, ErrCompletion
	}
	accepted, err := decodeCompletion(p.Metadata, r)
	if err != nil {
		return Classification{}, err
	}
	// The originating delivery must agree with this durable resource fact.
	if err := c.verifyOrigin(ctx, p.DeliveryID, r); err != nil {
		return Classification{}, err
	}
	return accepted, nil
}

func (c *Coordinator) verifyOrigin(ctx context.Context, deliveryID string, r resource) error {
	event, err := c.cfg.Store.Event(ctx, deliveryID)
	if err != nil {
		return persistence(err)
	}
	if event.Resource == nil {
		return ErrCompletion
	}
	origin, err := normalizedResource(*event.Resource)
	if err != nil || origin != r {
		return ErrCompletion
	}
	return nil
}
