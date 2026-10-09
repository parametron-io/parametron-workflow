// Package observe turns durable notification identity into current observations.
// Observation is not authorization. This package has no mutation or scheduling authority.
package observe

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"strings"

	"github.com/parametron-io/parametron-workflow/internal/config"
	"github.com/parametron-io/parametron-workflow/internal/storage"
	"github.com/parametron-io/parametron-workflow/internal/worker"
)

var (
	ErrIdentity      = errors.New("observe: invalid or ambiguous notification identity")
	ErrRepository    = errors.New("observe: repository outside deployment")
	ErrConfiguration = errors.New("observe: invalid resolved deployment")
	ErrBinding       = errors.New("observe: invalid resource binding")
	ErrObservation   = errors.New("observe: inconsistent current observation")
)

type Resolver struct{ deployment config.ResolvedConfig }

func NewResolver(deployment config.ResolvedConfig) (*Resolver, error) {
	d, err := snapshot(deployment)
	if err != nil {
		return nil, err
	}
	return &Resolver{d}, nil
}

// object rejects nonobjects and duplicate keys at identity-bearing levels.
// Unknown values are retained only as opaque JSON, never decoded as current state.
func object(data []byte) (map[string]json.RawMessage, error) {
	d := json.NewDecoder(bytes.NewReader(data))
	t, err := d.Token()
	if err != nil || t != json.Delim('{') {
		return nil, ErrIdentity
	}
	m := map[string]json.RawMessage{}
	for d.More() {
		t, err := d.Token()
		if err != nil {
			return nil, ErrIdentity
		}
		k, ok := t.(string)
		if !ok {
			return nil, ErrIdentity
		}
		if _, exists := m[k]; exists {
			return nil, ErrIdentity
		}
		var v json.RawMessage
		if d.Decode(&v) != nil {
			return nil, ErrIdentity
		}
		m[k] = v
	}
	if _, err := d.Token(); err != nil {
		return nil, ErrIdentity
	}
	if _, err := d.Token(); err != io.EOF {
		return nil, ErrIdentity
	}
	return m, nil
}

func (r *Resolver) Resolve(ctx context.Context, e storage.Event) (storage.Resource, error) {
	if err := ctx.Err(); err != nil {
		return storage.Resource{}, err
	}
	root, err := object(e.Delivery.Payload)
	if err != nil {
		return storage.Resource{}, err
	}
	repo, err := object(root["repository"])
	if err != nil {
		return storage.Resource{}, ErrIdentity
	}
	owner, err := object(repo["owner"])
	if err != nil {
		return storage.Resource{}, ErrIdentity
	}
	var login, name string
	if json.Unmarshal(owner["login"], &login) != nil || json.Unmarshal(repo["name"], &name) != nil || login == "" || name == "" {
		return storage.Resource{}, ErrIdentity
	}
	i, hasIssue := root["issue"]
	p, hasPR := root["pull_request"]
	if hasIssue == hasPR {
		return storage.Resource{}, ErrIdentity
	}
	kind, raw := "issue", i
	if hasPR {
		kind, raw = "pull_request", p
	}
	candidate, err := object(raw)
	if err != nil {
		return storage.Resource{}, err
	}
	// GitHub issue_comment uses an issue-shaped identity for both Issues and PRs.
	if marker, exists := candidate["pull_request"]; exists {
		if !hasIssue {
			return storage.Resource{}, ErrIdentity
		}
		if _, err := object(marker); err != nil {
			return storage.Resource{}, ErrIdentity
		}
		kind = "pull_request"
	}
	var number int64
	if json.Unmarshal(candidate["number"], &number) != nil || number <= 0 || int64(int(number)) != number {
		return storage.Resource{}, ErrIdentity
	}
	resource := storage.Resource{Owner: strings.ToLower(login), Repository: strings.ToLower(name), Kind: kind, Number: number}
	if _, err := repository(r.deployment, resource); err != nil {
		return storage.Resource{}, err
	}
	return resource, nil
}

func repository(d config.ResolvedConfig, r storage.Resource) (config.Repository, error) {
	if !strings.EqualFold(r.Owner, d.Organization.Login) {
		return config.Repository{}, ErrRepository
	}
	for _, repo := range d.Repositories {
		if strings.EqualFold(repo.Owner, r.Owner) && strings.EqualFold(repo.Name, r.Repository) {
			return repo, nil
		}
	}
	return config.Repository{}, ErrRepository
}

var _ worker.ResourceResolver = (*Resolver)(nil)
