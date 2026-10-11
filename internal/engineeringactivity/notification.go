// Package engineeringactivity owns standalone first-development activity policy.
// Production fresh context assembly and routing belong to #39.
package engineeringactivity

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"strconv"
	"strings"

	"github.com/parametron-io/parametron-workflow/internal/engineeringbranch"
	"github.com/parametron-io/parametron-workflow/internal/github"
	"github.com/parametron-io/parametron-workflow/internal/storage"
)

// object rejects duplicate identity keys while ignoring unrelated payload data.
func object(raw []byte) (map[string]json.RawMessage, error) {
	d := json.NewDecoder(bytes.NewReader(raw))
	t, err := d.Token()
	if err != nil || t != json.Delim('{') {
		return nil, ErrInvalid
	}
	m := map[string]json.RawMessage{}
	for d.More() {
		t, err := d.Token()
		if err != nil {
			return nil, ErrInvalid
		}
		k, ok := t.(string)
		if !ok {
			return nil, ErrInvalid
		}
		if _, exists := m[k]; exists {
			return nil, ErrInvalid
		}
		var v json.RawMessage
		if d.Decode(&v) != nil {
			return nil, ErrInvalid
		}
		m[k] = v
	}
	if _, err = d.Token(); err != nil {
		return nil, ErrInvalid
	}
	if _, err = d.Token(); err != io.EOF {
		return nil, ErrInvalid
	}
	return m, nil
}

// ResolvePush returns (candidate,true,nil), (zero,false,nil) for unrelated
// notifications, or a bounded error for malformed/out-of-deployment identity.
// before/after and actor fields are never read or retained.
func ResolvePush(ctx context.Context, repos []github.Repository, d storage.Delivery) (storage.Resource, bool, error) {
	if err := ctx.Err(); err != nil {
		return storage.Resource{}, false, err
	}
	if d.EventName != "push" {
		return storage.Resource{}, false, nil
	}
	root, err := object(d.Payload)
	if err != nil {
		return storage.Resource{}, false, err
	}
	repo, err := object(root["repository"])
	if err != nil {
		return storage.Resource{}, false, err
	}
	owner, err := object(repo["owner"])
	if err != nil {
		return storage.Resource{}, false, err
	}
	var login, name, ref string
	if json.Unmarshal(owner["login"], &login) != nil || json.Unmarshal(repo["name"], &name) != nil || json.Unmarshal(root["ref"], &ref) != nil || !github.ValidBranchRepository(github.Repository{ID: "validation", Owner: login, Name: name}) || ref == "" {
		return storage.Resource{}, false, ErrInvalid
	}
	configured := false
	for _, r := range repos {
		if !github.ValidBranchRepository(r) {
			return storage.Resource{}, false, ErrInvalid
		}
		if strings.EqualFold(r.Owner, login) && strings.EqualFold(r.Name, name) {
			configured = true
		}
	}
	if !configured {
		return storage.Resource{}, false, ErrInvalid
	}
	const prefix = "refs/heads/development/issue-"
	if !strings.HasPrefix(ref, prefix) {
		return storage.Resource{}, false, nil
	}
	num := strings.TrimPrefix(ref, prefix)
	n, err := strconv.ParseInt(num, 10, 64)
	r := storage.Resource{Owner: strings.ToLower(login), Repository: strings.ToLower(name), Kind: "issue", Number: n}
	canonical, e := engineeringbranch.Name(r)
	if err != nil || e != nil || ref != "refs/heads/"+canonical {
		return storage.Resource{}, false, ErrInvalid
	}
	return r, true, nil
}
