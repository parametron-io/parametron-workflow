// Package semanticflow coordinates durable initial classification. It grants no
// lifecycle or mutation authority and leaves scheduling to the durable worker.
package semanticflow

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"reflect"
	"strings"
	"time"

	"github.com/parametron-io/parametron-workflow/internal/intent"
	"github.com/parametron-io/parametron-workflow/internal/observe"
	"github.com/parametron-io/parametron-workflow/internal/semantic"
	"github.com/parametron-io/parametron-workflow/internal/semanticpolicy"
	"github.com/parametron-io/parametron-workflow/internal/storage"
)

type State string

const (
	NotRequired State = "not_required"
	Pending     State = "pending"
	Accepted    State = "accepted"
)

// Classification separates semantic acceptance from observed GitHub metadata.
// Private fields ensure only the coordinator can construct accepted authority.
type Classification struct {
	state State
	issue *semanticpolicy.AcceptedIssue
	pr    *semanticpolicy.AcceptedPR
}

func (c Classification) State() State   { return c.state }
func (c Classification) Required() bool { return c.state == Pending || c.state == Accepted }
func (c Classification) Issue() (semanticpolicy.AcceptedIssue, bool) {
	if c.state != Accepted || c.issue == nil {
		return semanticpolicy.AcceptedIssue{}, false
	}
	v := *c.issue
	v.Classification.Labels = append([]semanticpolicy.Label{}, v.Classification.Labels...)
	return v, true
}
func (c Classification) PR() (semanticpolicy.AcceptedPR, bool) {
	if c.state != Accepted || c.pr == nil {
		return semanticpolicy.AcceptedPR{}, false
	}
	v := *c.pr
	v.Classification.Labels = append([]semanticpolicy.Label{}, v.Classification.Labels...)
	return v, true
}

type PolicyInput struct {
	Current        observe.PolicyInput
	Intent         intent.Intent
	Classification Classification
}
type Consumer interface {
	Evaluate(context.Context, PolicyInput) error
}
type PrimaryReader interface {
	ReadPrimary(context.Context, storage.Resource) (observe.ObservedState, error)
}
type CompletionStore interface {
	Provenance(context.Context, string, string) (storage.Provenance, error)
	RecordProvenance(context.Context, storage.Provenance) (bool, error)
	Event(context.Context, string) (storage.Event, error)
}
type Config struct {
	Store    CompletionStore
	Runner   semantic.Runner
	Primary  PrimaryReader
	Consumer Consumer
	Clock    func() time.Time
}
type Coordinator struct{ cfg Config }

func absent(v any) bool {
	if v == nil {
		return true
	}
	r := reflect.ValueOf(v)
	switch r.Kind() {
	case reflect.Pointer, reflect.Func, reflect.Interface:
		return r.IsNil()
	}
	return false
}
func New(c Config) (*Coordinator, error) {
	if absent(c.Store) || absent(c.Runner) || absent(c.Primary) || absent(c.Consumer) || c.Clock == nil {
		return nil, observe.ErrConfiguration
	}
	return &Coordinator{c}, nil
}
func (c *Coordinator) Evaluate(ctx context.Context, current observe.PolicyInput) error {
	in, err := c.Enrich(ctx, current)
	if err != nil {
		return err
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	return c.cfg.Consumer.Evaluate(ctx, in)
}

// Enrich returns Pending with no typed acceptance on classification failure.
// Evaluate only hands successful enrichment to downstream policy.
func (c *Coordinator) Enrich(ctx context.Context, current observe.PolicyInput) (PolicyInput, error) {
	in := PolicyInput{Current: current, Classification: Classification{state: NotRequired}}
	if err := ctx.Err(); err != nil {
		return in, err
	}
	o := current.Observed
	r, err := normalizedResource(o.Resource)
	if err != nil {
		return in, err
	}
	if o.Presence == observe.Missing {
		if o.Issue != nil || o.PullRequest != nil {
			return in, observe.ErrObservation
		}
		return in, nil
	}
	title, body, err := content(o)
	if err != nil {
		return in, err
	}
	in.Intent, err = intent.Parse(body, intent.Context{Organization: r.Owner, Repository: r.Repository})
	if err != nil {
		return in, ErrIntent
	}
	if !in.Intent.Effective().Classification {
		return in, nil
	}
	in.Classification.state = Pending
	key, _ := ResourceKey(o.Resource)
	stored, err := c.cfg.Store.Provenance(ctx, Namespace, key)
	if err == nil {
		var accepted Classification
		accepted, err = c.load(ctx, stored, r)
		if err == nil {
			in.Classification = accepted
		}
		return in, err
	}
	if !errors.Is(err, storage.ErrNotFound) {
		return in, persistence(err)
	}
	digest := fingerprint(r, title, body, in.Intent)
	record := completion{Format: 1, Resource: r, InputDigest: digest}
	service := semanticpolicy.Service{Runner: c.cfg.Runner}
	if r.Kind == "issue" {
		value, runErr := service.ClassifyIssue(ctx, title, body)
		if runErr != nil {
			return in, execution(runErr)
		}
		record.Issue, record.Provenance = &value.Classification, provenance(value.Provenance)
	} else {
		value, runErr := service.ClassifyPR(ctx, title, body)
		if runErr != nil {
			return in, execution(runErr)
		}
		record.PR, record.Provenance = &value.Classification, provenance(value.Provenance)
	}
	if err := ctx.Err(); err != nil {
		return in, err
	}
	// Revalidate only primary content, never classifier-owned output or Projects.
	fresh, err := c.cfg.Primary.ReadPrimary(ctx, o.Resource)
	if err != nil {
		return in, err
	}
	fr, err := normalizedResource(fresh.Resource)
	if err != nil || fr != r {
		return in, observe.ErrObservation
	}
	if fresh.Presence == observe.Missing {
		return in, ErrStale
	}
	ft, fb, err := content(fresh)
	if err != nil {
		return in, err
	}
	fi, err := intent.Parse(fb, intent.Context{Organization: r.Owner, Repository: r.Repository})
	// Changed content, including newly invalid syntax, rejects this old judgement.
	if err != nil || fingerprint(r, ft, fb, fi) != digest {
		return in, ErrStale
	}
	metadata, err := json.Marshal(record)
	if err != nil {
		return in, ErrCompletion
	}
	accepted, err := decodeCompletion(metadata, r)
	if err != nil {
		return in, ErrPolicy
	}
	if err := c.verifyOrigin(ctx, current.DeliveryID, r); err != nil {
		return in, err
	}
	if err := ctx.Err(); err != nil {
		return in, err
	}
	_, err = c.cfg.Store.RecordProvenance(ctx, storage.Provenance{Namespace: Namespace, Key: key, DeliveryID: current.DeliveryID, Metadata: metadata, CreatedAt: c.cfg.Clock()})
	if err != nil {
		return in, persistence(err)
	}
	in.Classification = accepted
	return in, nil
}

func content(o observe.ObservedState) (string, string, error) {
	if o.Presence != observe.Present {
		return "", "", observe.ErrObservation
	}
	if o.Resource.Kind == "issue" && o.Issue != nil && o.PullRequest == nil {
		return o.Issue.Title, o.Issue.Body, nil
	}
	if o.Resource.Kind == "pull_request" && o.PullRequest != nil && o.Issue == nil {
		return o.PullRequest.Title, o.PullRequest.Body, nil
	}
	return "", "", observe.ErrObservation
}

type resource struct {
	Owner      string `json:"owner"`
	Repository string `json:"repository"`
	Kind       string `json:"kind"`
	Number     int64  `json:"number"`
}

func normalizedResource(r storage.Resource) (resource, error) {
	v := resource{strings.ToLower(r.Owner), strings.ToLower(r.Repository), r.Kind, r.Number}
	if v.Owner == "" || v.Repository == "" || strings.ContainsAny(v.Owner+v.Repository, "/#:\\ \t\r\n") || v.Number <= 0 || (v.Kind != "issue" && v.Kind != "pull_request") {
		return resource{}, observe.ErrBinding
	}
	return v, nil
}

// ResourceKey excludes node ID and distinguishes Issue from PR identity.
func ResourceKey(r storage.Resource) (string, error) {
	v, err := normalizedResource(r)
	if err != nil {
		return "", err
	}
	return fmt.Sprintf("%s/%s/%s/%d", v.Owner, v.Repository, v.Kind, v.Number), nil
}
func fingerprint(r resource, title, body string, i intent.Intent) string {
	data, _ := json.Marshal(struct {
		Resource  resource         `json:"resource"`
		Title     string           `json:"title"`
		Body      string           `json:"body"`
		Intent    intent.Intent    `json:"intent"`
		Effective intent.Effective `json:"effective"`
	}{r, title, body, i, i.Effective()})
	sum := sha256.Sum256(data)
	return "sha256:" + hex.EncodeToString(sum[:])
}
