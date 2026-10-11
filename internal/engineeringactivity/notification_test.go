package engineeringactivity

import (
	"context"
	"encoding/json"
	"errors"
	"github.com/parametron-io/parametron-workflow/internal/github"
	"github.com/parametron-io/parametron-workflow/internal/storage"
	"testing"
)

func TestPushResolution(t *testing.T) {
	other := github.Repository{ID: "OTHER", Owner: repo.Owner, Name: "other"}
	for _, tc := range []struct {
		ref     string
		ok, bad bool
	}{
		{"refs/heads/development/issue-38", true, false}, {"refs/heads/main", false, false}, {"refs/heads/arbitrary", false, false}, {"refs/tags/development/issue-38", false, false}, {"development/issue-38", false, false},
		{"refs/heads/development/issue-038", false, true}, {"refs/heads/development/issue-0", false, true}, {"refs/heads/development/issue--1", false, true}, {"refs/heads/development/issue-+38", false, true}, {"refs/heads/development/issue-9223372036854775808", false, true}, {"refs/heads/development/issue-", false, true}, {"refs/heads/development/issue-x", false, true}, {"refs/heads/development/issue-38/extra", false, true},
	} {
		t.Run(tc.ref, func(t *testing.T) {
			for _, r := range []github.Repository{repo, other} {
				payload, _ := json.Marshal(map[string]any{"repository": map[string]any{"owner": map[string]any{"login": "PARAMETRON-IO"}, "name": r.Name}, "ref": tc.ref, "before": "ignored", "after": map[string]any{"malformed": "ignored"}})
				d := storage.Delivery{EventName: "push", Payload: payload}
				got, ok, err := ResolvePush(context.Background(), []github.Repository{repo, other}, d)
				if ok != tc.ok || (err != nil) != tc.bad {
					t.Fatal(got, ok, err)
				}
				if ok && (got.Owner != repo.Owner || got.Repository != r.Name || got.Number != 38 || got.Kind != "issue" || got.NodeID != "") {
					t.Fatal(got)
				}
			}
		})
	}
	for _, payload := range []string{`{}`, `null`, `{"repository":{"owner":{"login":"bad/owner"},"name":"workflow"},"ref":"refs/heads/main"}`, `{"repository":{"owner":{"login":"parametron-io"},"name":"outside"},"ref":"refs/heads/main"}`, `{"repository":{"owner":{"login":"parametron-io","login":"other"},"name":"workflow"},"ref":"refs/heads/main"}`, `{"repository":{"owner":{"login":"parametron-io"},"name":"workflow"},"ref":null}`, `{"repository":{"owner":{"login":"parametron-io"},"name":"workflow"},"ref":"refs/heads/main","ref":"refs/heads/development/issue-38"}`} {
		if _, _, err := ResolvePush(context.Background(), []github.Repository{repo}, storage.Delivery{EventName: "push", Payload: []byte(payload)}); err == nil {
			t.Fatal(payload)
		}
	}
	if _, ok, err := ResolvePush(context.Background(), nil, storage.Delivery{EventName: "commit"}); err != nil || ok {
		t.Fatal(ok, err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, _, err := ResolvePush(ctx, nil, storage.Delivery{}); !errors.Is(err, context.Canceled) {
		t.Fatal(err)
	}
}
