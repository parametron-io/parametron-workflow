// Package prreview owns explicit accepted PR review and rework, never completion.
package prreview

import (
	"bytes"
	"encoding/json"
	"io"
	"strings"
	"time"

	"github.com/parametron-io/parametron-workflow/internal/github"
	"github.com/parametron-io/parametron-workflow/internal/storage"
)

type Candidate struct {
	Kind     string
	PRID     string
	Number   int64
	Actor    github.Actor
	At       time.Time
	ReviewID string
	State    string
}

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
		if _, ok := m[k]; ok {
			return nil, ErrInvalid
		}
		var v json.RawMessage
		if d.Decode(&v) != nil {
			return nil, ErrInvalid
		}
		m[k] = v
	}
	if _, err := d.Token(); err != nil {
		return nil, ErrInvalid
	}
	if _, err := d.Token(); err != io.EOF {
		return nil, ErrInvalid
	}
	return m, nil
}

func actor(raw []byte) (github.Actor, error) {
	m, err := object(raw)
	if err != nil {
		return github.Actor{}, err
	}
	var a github.Actor
	if json.Unmarshal(m["node_id"], &a.ID) != nil || json.Unmarshal(m["login"], &a.Login) != nil || json.Unmarshal(m["type"], &a.Type) != nil || !validID(a.ID) || a.Login == "" || a.Type == "" {
		return a, ErrInvalid
	}
	return a, nil
}

// Notification extracts selectors only. In particular updated_at is mutable;
// it can select a timeline node but cannot prove that a transition occurred.
func Notification(d storage.Delivery) (Candidate, bool, error) {
	var c Candidate
	if d.EventName != "pull_request" && d.EventName != "pull_request_review" {
		return c, false, nil
	}
	m, err := object(d.Payload)
	if err != nil {
		return c, false, err
	}
	var action string
	if _, present := m["action"]; !present && d.EventName == "pull_request" {
		return c, false, nil
	}
	if json.Unmarshal(m["action"], &action) != nil {
		return c, false, ErrInvalid
	}
	if d.EventName == "pull_request" && action != "ready_for_review" || d.EventName == "pull_request_review" && action != "submitted" {
		return c, false, nil
	}
	p, err := object(m["pull_request"])
	if err != nil {
		return c, true, err
	}
	if json.Unmarshal(p["node_id"], &c.PRID) != nil || json.Unmarshal(p["number"], &c.Number) != nil || !validID(c.PRID) || c.Number <= 0 {
		return c, true, ErrInvalid
	}
	if d.EventName == "pull_request" {
		c.Kind = "ready"
		c.Actor, err = actor(m["sender"])
		if err != nil || json.Unmarshal(p["updated_at"], &c.At) != nil || c.At.IsZero() {
			return c, true, ErrInvalid
		}
	} else {
		c.Kind = "review"
		r, err := object(m["review"])
		if err != nil {
			return c, true, err
		}
		if json.Unmarshal(r["node_id"], &c.ReviewID) != nil || !validID(c.ReviewID) || json.Unmarshal(r["state"], &c.State) != nil {
			return c, true, ErrInvalid
		}
		c.State = strings.ToUpper(c.State)
		switch c.State {
		case "COMMENTED", "APPROVED", "CHANGES_REQUESTED":
		default:
			return c, false, nil
		}
		c.Actor, err = actor(r["user"])
		if err != nil || json.Unmarshal(r["submitted_at"], &c.At) != nil || c.At.IsZero() {
			return c, true, ErrInvalid
		}
	}
	c.At = c.At.UTC()
	return c, true, nil
}
