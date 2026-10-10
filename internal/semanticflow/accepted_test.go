package semanticflow

import (
	"context"
	"encoding/json"
	"errors"
	"testing"

	"github.com/parametron-io/parametron-workflow/internal/semanticpolicy"
	"github.com/parametron-io/parametron-workflow/internal/storage"
)

// This package owns durable decoding, including test fixtures for corrupt input.
type acceptedStore struct {
	*storage.Store
	record storage.Provenance
	err    error
}

func (s acceptedStore) Provenance(context.Context, string, string) (storage.Provenance, error) {
	return s.record, s.err
}
func TestAcceptedLoader(t *testing.T) {
	h := newHarness(t, "issue")
	h.step()
	p := h.completion("issue")
	r := storage.Resource{Owner: "PARAMETRON-IO", Repository: "REPO", Kind: "issue", Number: 27, NodeID: "ignored"}
	calls := h.calls.Load()
	for _, typ := range semanticpolicy.Types() {
		var c completion
		if err := json.Unmarshal(p.Metadata, &c); err != nil {
			t.Fatal(err)
		}
		c.Issue.Type = typ
		record := p
		record.Metadata, _ = json.Marshal(c)
		loader, err := NewAcceptedLoader(acceptedStore{Store: h.store, record: record})
		if err != nil {
			t.Fatal(err)
		}
		got, ok, err := loader.AcceptedIssue(context.Background(), r)
		if err != nil || !ok || got.Classification.Type != typ {
			t.Fatal(got, ok, err)
		}
		got.Classification.Labels[0] = "security"
		again, _, err := loader.AcceptedIssue(context.Background(), r)
		if err != nil || again.Classification.Labels[0] == "security" {
			t.Fatal("shared labels", err)
		}
	}
	if h.calls.Load() != calls {
		t.Fatal("model invoked")
	}
	loader, _ := NewAcceptedLoader(h.store)
	missing := r
	missing.Number = 99
	_, ok, err := loader.AcceptedIssue(context.Background(), missing)
	if err != nil || ok {
		t.Fatal(ok, err)
	}
	pr := r
	pr.Kind = "pull_request"
	if _, _, err := loader.AcceptedIssue(context.Background(), pr); err == nil {
		t.Fatal("PR accepted")
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, _, err := loader.AcceptedIssue(ctx, r); !errors.Is(err, context.Canceled) {
		t.Fatal(err)
	}
	if _, err := NewAcceptedLoader((*storage.Store)(nil)); err == nil {
		t.Fatal("nil store accepted")
	}
}
func TestAcceptedLoaderFailsClosed(t *testing.T) {
	h := newHarness(t, "issue")
	h.step()
	original := h.completion("issue")
	r := storage.Resource{Owner: "parametron-io", Repository: "repo", Kind: "issue", Number: 27}
	for _, name := range []string{"corrupt", "namespace", "key", "origin", "time"} {
		t.Run(name, func(t *testing.T) {
			p := original
			switch name {
			case "corrupt":
				p.Metadata = []byte(`{}`)
			case "namespace":
				p.Namespace = "other"
			case "key":
				p.Key = "other"
			case "origin":
				p.DeliveryID = "absent"
			case "time":
				p.CreatedAt = storage.Provenance{}.CreatedAt
			}
			loader, _ := NewAcceptedLoader(acceptedStore{Store: h.store, record: p})
			_, ok, err := loader.AcceptedIssue(context.Background(), r)
			if ok || !errors.Is(err, ErrCompletion) {
				t.Fatal(ok, err)
			}
		})
	}
	loader, _ := NewAcceptedLoader(acceptedStore{Store: h.store, err: errors.New("private DB diagnostic")})
	_, _, err := loader.AcceptedIssue(context.Background(), r)
	if !errors.Is(err, ErrPersistence) {
		t.Fatal(err)
	}
}
