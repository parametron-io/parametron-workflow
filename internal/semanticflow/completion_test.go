package semanticflow

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"

	"github.com/parametron-io/parametron-workflow/internal/observe"
	"github.com/parametron-io/parametron-workflow/internal/semantic"
	"github.com/parametron-io/parametron-workflow/internal/storage"
)

func TestCompletionDeterminismAndStrictValidation(t *testing.T) {
	for _, kind := range []string{"issue", "pull_request"} {
		t.Run(kind, func(t *testing.T) {
			h := newHarness(t, kind)
			h.step()
			p := h.completion(kind)
			r, _ := normalizedResource(storage.Resource{Owner: "parametron-io", Repository: "repo", Kind: kind, Number: 27})
			var good completion
			if err := json.Unmarshal(p.Metadata, &good); err != nil {
				t.Fatal(err)
			}
			for n := 0; n < 20; n++ {
				data, err := json.Marshal(good)
				if err != nil || !bytes.Equal(data, p.Metadata) {
					t.Fatal("unstable metadata")
				}
			}
			for _, tc := range []struct {
				name   string
				mutate func(*completion)
			}{
				{"format", func(c *completion) { c.Format = 2 }},
				{"owner", func(c *completion) { c.Resource.Owner = "other" }},
				{"repository", func(c *completion) { c.Resource.Repository = "other" }},
				{"kind", func(c *completion) { c.Resource.Kind = "other" }},
				{"number", func(c *completion) { c.Resource.Number++ }},
				{"digest", func(c *completion) { c.InputDigest = "invalid" }},
				{"capability", func(c *completion) { c.Provenance.Capability = semantic.EstimateIssue }},
				{"provider", func(c *completion) { c.Provenance.Provider = strings.Repeat("x", 129) }},
				{"model", func(c *completion) { c.Provenance.Model = "invalid model" }},
				{"prompt identity", func(c *completion) { c.Provenance.PromptIdentity = "raw prompt" }},
				{"schema identity", func(c *completion) { c.Provenance.SchemaIdentity = "raw schema" }},
				{"prompt digest", func(c *completion) { c.Provenance.PromptDigest = "sha256:" + strings.Repeat("A", 64) }},
				{"schema digest", func(c *completion) { c.Provenance.SchemaDigest = "bad" }},
				{"no classification", func(c *completion) { c.Issue, c.PR = nil, nil }},
				{"labels", func(c *completion) {
					if c.Issue != nil {
						c.Issue.Labels = nil
					} else {
						c.PR.Labels = nil
					}
				}},
				{"taxonomy", func(c *completion) {
					if c.Issue != nil {
						c.Issue.Labels[0] = "help wanted"
					} else {
						c.PR.Labels[0] = "help wanted"
					}
				}},
				{"label order", func(c *completion) {
					if c.Issue != nil {
						c.Issue.Labels[0], c.Issue.Labels[1] = c.Issue.Labels[1], c.Issue.Labels[0]
					} else {
						c.PR.Labels[0], c.PR.Labels[1] = c.PR.Labels[1], c.PR.Labels[0]
					}
				}},
			} {
				t.Run(tc.name, func(t *testing.T) {
					var c completion
					json.Unmarshal(p.Metadata, &c)
					tc.mutate(&c)
					data, _ := json.Marshal(c)
					if _, err := decodeCompletion(data, r); !errors.Is(err, ErrCompletion) {
						t.Fatal("corrupt record accepted", err)
					}
				})
			}
			for _, raw := range [][]byte{nil, []byte(`{`), append(append([]byte{}, p.Metadata...), []byte(` {}`)...),
				bytes.Replace(p.Metadata, []byte(`"format":1`), []byte(`"format":1,"format":1`), 1),
				bytes.Replace(p.Metadata, []byte(`"format":1`), []byte(`"format":1,"unknown":false`), 1),
				bytes.Replace(p.Metadata, []byte(`"format":1`), []byte(`"Format":1`), 1),
				bytes.Replace(p.Metadata, []byte(`"format":1,`), nil, 1),
				bytes.Replace(p.Metadata, []byte(`"labels":["workflow","docs"]`), []byte(`"labels":["workflow","workflow"]`), 1),
				bytes.Replace(p.Metadata, []byte(`"provenance":{`), []byte(`"provenance":{"model":"extra",`), 1),
				[]byte(strings.Repeat("x", 16385)),
			} {
				if _, err := decodeCompletion(raw, r); !errors.Is(err, ErrCompletion) {
					t.Fatal("noncanonical record accepted", string(raw), err)
				}
			}
			if kind == "issue" {
				for _, field := range []string{"type", "priority", "effort"} {
					var c completion
					json.Unmarshal(p.Metadata, &c)
					switch field {
					case "type":
						c.Issue.Type = "invalid"
					case "priority":
						c.Issue.Priority = "invalid"
					case "effort":
						c.Issue.Effort = "invalid"
					}
					raw, _ := json.Marshal(c)
					if _, err := decodeCompletion(raw, r); err == nil {
						t.Fatal(field)
					}
				}
			}
		})
	}
}

func TestCorruptDurableRecordFailsClosedUnderWorker(t *testing.T) {
	h := newHarness(t, "issue")
	r := storage.Resource{Owner: "parametron-io", Repository: "repo", Kind: "issue", Number: 27}
	if err := h.store.BindResource(context.Background(), "initial", r); err != nil {
		t.Fatal(err)
	}
	_, err := h.store.RecordProvenance(context.Background(), storage.Provenance{Namespace: Namespace, Key: "parametron-io/repo/issue/27", DeliveryID: "initial", Metadata: []byte(`{"format":2}`), CreatedAt: h.now()})
	if err != nil {
		t.Fatal(err)
	}
	h.step()
	h.state("initial", storage.Failed, "semantic_completion")
	if h.calls.Load() != 0 || len(h.inputs) != 0 {
		t.Fatal("corrupt record granted authority")
	}
}

type failingStore struct {
	*storage.Store
	readErr, writeErr error
}

func (s failingStore) Provenance(ctx context.Context, ns, key string) (storage.Provenance, error) {
	if s.readErr != nil {
		return storage.Provenance{}, s.readErr
	}
	return s.Store.Provenance(ctx, ns, key)
}
func (s failingStore) RecordProvenance(ctx context.Context, p storage.Provenance) (bool, error) {
	if s.writeErr != nil {
		return false, s.writeErr
	}
	return s.Store.RecordProvenance(ctx, p)
}
func TestPersistenceFailuresLeavePending(t *testing.T) {
	for _, tc := range []struct {
		read, write error
		expected    error
	}{
		{errors.New("RAW DATABASE DIAGNOSTIC"), nil, ErrPersistence},
		{nil, errors.New("RAW DATABASE DIAGNOSTIC"), ErrPersistence},
		{nil, storage.ErrConflict, ErrCompletion},
	} {
		t.Run(tc.expected.Error()+strings.Repeat("w", btoi(tc.write != nil)), func(t *testing.T) {
			h := newHarness(t, "issue")
			h.flow.cfg.Store = failingStore{h.store, tc.read, tc.write}
			r := storage.Resource{Owner: "parametron-io", Repository: "repo", Kind: "issue", Number: 27}
			if err := h.store.BindResource(context.Background(), "initial", r); err != nil {
				t.Fatal(err)
			}
			o, err := h.processor.ReadPrimary(context.Background(), r)
			if err != nil {
				t.Fatal(err)
			}
			in, err := h.flow.Enrich(context.Background(), observe.PolicyInput{DeliveryID: "initial", Observed: o})
			if !errors.Is(err, tc.expected) || in.Classification.State() != Pending {
				t.Fatal(in, err)
			}
			h.noCompletion("issue")
			if strings.Contains(err.Error(), "RAW") {
				t.Fatal("raw diagnostics escaped")
			}
		})
	}
}
func btoi(b bool) int {
	if b {
		return 1
	}
	return 0
}

func TestOriginRecordMustAgree(t *testing.T) {
	h := newHarness(t, "issue")
	h.step()
	p := h.completion("issue")
	r, _ := normalizedResource(storage.Resource{Owner: "parametron-io", Repository: "repo", Kind: "issue", Number: 27})
	h.insert("unbound", "issue")
	p.DeliveryID = "unbound"
	if _, err := h.flow.load(context.Background(), p, r); !errors.Is(err, ErrCompletion) {
		t.Fatal("unbound origin trusted", err)
	}
	if err := h.store.BindResource(context.Background(), "unbound", storage.Resource{Owner: r.Owner, Repository: r.Repository, Kind: r.Kind, Number: 28}); err != nil {
		t.Fatal(err)
	}
	if _, err := h.flow.load(context.Background(), p, r); !errors.Is(err, ErrCompletion) {
		t.Fatal("wrong origin trusted", err)
	}
}

func TestRefetchIdentityFailureCannotAccept(t *testing.T) {
	h := newHarness(t, "issue")
	h.runner = &semantic.Fake{ClassifyIssueFunc: func(context.Context, semantic.Input) (semantic.Result, error) {
		h.mu.Lock()
		h.issue.Number = 28
		h.mu.Unlock()
		return result(t, semantic.ClassifyIssue), nil
	}}
	h.compose()
	h.step()
	h.state("initial", storage.Failed, "local_processor")
	h.noCompletion("issue")
	if len(h.inputs) != 0 {
		t.Fatal("wrong identity accepted")
	}
}
