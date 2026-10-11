package engineeringactivity

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"maps"
	"reflect"
	"strings"
	"sync"
	"time"

	"github.com/parametron-io/parametron-workflow/internal/config"
	eb "github.com/parametron-io/parametron-workflow/internal/engineeringbranch"
	ep "github.com/parametron-io/parametron-workflow/internal/engineeringpolicy"
	"github.com/parametron-io/parametron-workflow/internal/github"
	"github.com/parametron-io/parametron-workflow/internal/observe"
	"github.com/parametron-io/parametron-workflow/internal/storage"
)

var (
	ErrInvalid    = errors.New("engineering activity: invalid input")
	ErrProvenance = errors.New("engineering activity: invalid provenance")
	ErrStorage    = errors.New("engineering activity: storage failure")
	ErrProject    = errors.New("engineering activity: exact Engineering item unavailable")
)

const (
	firstNamespace      = "engineering_first_activity_v1"
	completionNamespace = "engineering_activity_completion_v1"
)

type BranchEvidence interface {
	Completed(context.Context, storage.Resource) (eb.Completion, error)
}
type CurrentBranch interface {
	Branch(context.Context, github.Repository, string) (github.Branch, error)
}
type ProjectReader interface {
	ReadCurrent(context.Context, storage.Resource) (observe.ObservedState, error)
}
type Config struct {
	Repositories []github.Repository
	Project      config.ResolvedProject
	Provenance   BranchEvidence
	Branches     CurrentBranch
	Comparison   github.CommitComparison
	Projects     ProjectReader
	Fields       github.LifecycleFields
	Store        eb.Store
}
type Controller struct {
	cfg Config
	mu  sync.Mutex
}

// Evidence is bounded canonical v1 metadata. ReceivedAt is the immutable UTC
// delivery acceptance timestamp, not a commit or processing timestamp.
type Evidence struct {
	Version    int         `json:"version"`
	Branch     eb.Evidence `json:"branch"`
	HeadSHA    string      `json:"head_sha"`
	ReceivedAt time.Time   `json:"received_at"`
	StartDate  string      `json:"start_date"`
}
type Result struct {
	Accepted, Completed bool
	Evidence            Evidence
}

func absent(v any) bool {
	if v == nil {
		return true
	}
	r := reflect.ValueOf(v)
	switch r.Kind() {
	case reflect.Pointer, reflect.Interface, reflect.Func:
		return r.IsNil()
	}
	return false
}
func validID(v string) bool {
	return v != "" && strings.TrimSpace(v) == v && !strings.ContainsAny(v, " \t\r\n")
}
func New(c Config) (*Controller, error) {
	for _, v := range []any{c.Provenance, c.Branches, c.Comparison, c.Projects, c.Fields, c.Store} {
		if absent(v) {
			return nil, ErrInvalid
		}
	}
	if len(c.Repositories) == 0 || !validID(c.Project.ID) || !validID(c.Project.Fields[config.Status]) || !validID(c.Project.Fields[config.StartDate]) || c.Project.Fields[config.Status] == c.Project.Fields[config.StartDate] || !validID(c.Project.StatusOptions[config.InProgress]) {
		return nil, ErrInvalid
	}
	names, ids := map[string]bool{}, map[string]bool{}
	for _, r := range c.Repositories {
		k := strings.ToLower(r.Owner + "/" + r.Name)
		if !github.ValidBranchRepository(r) || names[k] || ids[r.ID] {
			return nil, ErrInvalid
		}
		names[k] = true
		ids[r.ID] = true
	}
	c.Repositories = append([]github.Repository(nil), c.Repositories...)
	c.Project.Fields = maps.Clone(c.Project.Fields)
	c.Project.StatusOptions = maps.Clone(c.Project.StatusOptions)
	return &Controller{cfg: c}, nil
}
func identity(r storage.Resource) storage.Resource {
	r.Owner = strings.ToLower(r.Owner)
	r.Repository = strings.ToLower(r.Repository)
	r.NodeID = ""
	return r
}
func key(r storage.Resource) string {
	return fmt.Sprintf("%s/%s/issue/%d", r.Owner, r.Repository, r.Number)
}
func storageError(ctx context.Context, err error) error {
	if ctx.Err() != nil {
		return ctx.Err()
	}
	if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
		return err
	}
	return ErrStorage
}
func (c *Controller) delivery(ctx context.Context, id string, r storage.Resource) (storage.Event, error) {
	e, err := c.cfg.Store.Event(ctx, id)
	if errors.Is(err, storage.ErrNotFound) {
		return e, ErrProvenance
	}
	if err != nil {
		return e, storageError(ctx, err)
	}
	candidate, ok, err := ResolvePush(ctx, c.cfg.Repositories, e.Delivery)
	if err != nil {
		return e, err
	}
	if !ok || candidate != r || e.Resource == nil || identity(*e.Resource) != r || e.Delivery.ID != id || e.Delivery.ReceivedAt.IsZero() {
		return e, ErrProvenance
	}
	return e, nil
}
func (c *Controller) read(ctx context.Context, ns string, b eb.Evidence, creationTime time.Time) (storage.Provenance, Evidence, bool, error) {
	p, err := c.cfg.Store.Provenance(ctx, ns, key(b.WorkItem))
	if errors.Is(err, storage.ErrNotFound) {
		return p, Evidence{}, false, nil
	}
	if err != nil {
		return p, Evidence{}, false, storageError(ctx, err)
	}
	var e Evidence
	if json.Unmarshal(p.Metadata, &e) != nil {
		return p, e, false, ErrProvenance
	}
	canonical, _ := json.Marshal(e)
	if !bytes.Equal(canonical, p.Metadata) || e.Version != 1 || e.Branch != b || !github.ValidCommitSHA(e.HeadSHA) || e.HeadSHA == b.BaseSHA || p.Namespace != ns || p.Key != key(b.WorkItem) || p.CreatedAt.IsZero() || !github.ValidProjectDate(e.StartDate) {
		return p, Evidence{}, false, ErrProvenance
	}
	event, err := c.delivery(ctx, p.DeliveryID, b.WorkItem)
	if err != nil {
		return p, Evidence{}, false, err
	}
	if !event.Delivery.ReceivedAt.After(creationTime) || e.ReceivedAt != event.Delivery.ReceivedAt.UTC() || e.StartDate != e.ReceivedAt.Format(time.DateOnly) {
		return p, Evidence{}, false, ErrProvenance
	}
	return p, e, true, nil
}
func (c *Controller) record(ctx context.Context, ns, delivery string, e Evidence) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	data, _ := json.Marshal(e)
	_, err := c.cfg.Store.RecordProvenance(ctx, storage.Provenance{Namespace: ns, Key: key(e.Branch.WorkItem), DeliveryID: delivery, Metadata: data, CreatedAt: e.ReceivedAt})
	if errors.Is(err, storage.ErrConflict) {
		return ErrProvenance
	}
	if err != nil {
		return storageError(ctx, err)
	}
	return ctx.Err()
}

// item refetches normalized observations for an exact current Issue. The caller
// supplies complete lifecycle input separately; no native relationships enter it.
func (c *Controller) item(ctx context.Context, r storage.Resource) (observe.ProjectItem, error) {
	if err := ctx.Err(); err != nil {
		return observe.ProjectItem{}, err
	}
	o, err := c.cfg.Projects.ReadCurrent(ctx, r)
	if err != nil {
		return observe.ProjectItem{}, err
	}
	if o.Resource != r || o.Presence != observe.Present || o.Issue == nil || o.PullRequest != nil || o.Issue.State != "OPEN" || o.Issue.ID != r.NodeID || int64(o.Issue.Number) != r.Number || identity(storage.Resource{Owner: o.Issue.Repository.Owner, Repository: o.Issue.Repository.Name, Kind: "issue", Number: int64(o.Issue.Number)}) != identity(r) {
		return observe.ProjectItem{}, ErrProject
	}
	repoOK := false
	for _, repo := range c.cfg.Repositories {
		if repo.ID == o.Issue.Repository.ID && strings.EqualFold(repo.Owner, o.Issue.Repository.Owner) && strings.EqualFold(repo.Name, o.Issue.Repository.Name) {
			repoOK = true
		}
	}
	if !repoOK {
		return observe.ProjectItem{}, ErrProject
	}
	var found *observe.ProjectState
	for i := range o.Projects {
		p := &o.Projects[i]
		if p.Profile == config.Engineering {
			if found != nil || p.ID != c.cfg.Project.ID {
				return observe.ProjectItem{}, ErrProject
			}
			found = p
		}
	}
	if found == nil || len(found.Items) != 1 || found.Items[0].Archived || !validID(found.Items[0].ID) {
		return observe.ProjectItem{}, ErrProject
	}
	item := found.Items[0]
	if date, ok := item.Fields[config.StartDate]; ok {
		if date.Kind != config.Date || !github.ValidProjectDate(date.Date) || date.OptionID != "" || date.Number != 0 {
			return observe.ProjectItem{}, ErrProject
		}
	}
	if status, ok := item.Fields[config.Status]; ok {
		if status.Kind != config.SingleSelect || !validID(status.OptionID) || status.Date != "" || status.Number != 0 {
			return observe.ProjectItem{}, ErrProject
		}
	}
	return item, nil
}

// Handle acts only on Context.Root. Every unfinished attempt recomputes #35,
// refetches the authorized branch and Project state, and compares remaining
// effects. The caller must exclusively own resource lanes, as in #37.
func (c *Controller) Handle(ctx context.Context, delivery string, in ep.Input) (Result, error) {
	if err := ctx.Err(); err != nil {
		return Result{}, err
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	if err := ctx.Err(); err != nil {
		return Result{}, err
	}
	plan, err := ep.Evaluate(in)
	if err != nil {
		return Result{}, err
	}
	root := in.Context.Root
	r := identity(root)
	event, err := c.delivery(ctx, delivery, r)
	if err != nil {
		return Result{}, err
	}
	branch, err := c.cfg.Provenance.Completed(ctx, root)
	if err != nil {
		return Result{}, err
	}
	if !branch.Completed {
		return Result{}, nil
	}
	if branch.CreatedAt.IsZero() {
		return Result{}, ErrProvenance
	}
	// Processing-time completion cannot authorize a notification accepted before
	// (or simultaneously with) the successful controller creation fact.
	if !event.Delivery.ReceivedAt.After(branch.CreatedAt) {
		return Result{}, ctx.Err()
	}
	b := branch.Evidence
	name, err := eb.Name(r)
	if err != nil || b.WorkItem != r || b.Branch != name || b.Version != 1 || !github.ValidCommitSHA(b.BaseSHA) {
		return Result{}, ErrProvenance
	}
	configured := false
	for _, repo := range c.cfg.Repositories {
		if repo == b.Repository {
			configured = true
		}
	}
	if !configured {
		return Result{}, ErrProvenance
	}
	first, e, accepted, err := c.read(ctx, firstNamespace, b, branch.CreatedAt)
	if err != nil {
		return Result{}, err
	}
	completion, done, completed, err := c.read(ctx, completionNamespace, b, branch.CreatedAt)
	if err != nil {
		return Result{}, err
	}
	if completed {
		if !accepted || done != e || completion.DeliveryID != first.DeliveryID {
			return Result{}, ErrProvenance
		}
		return Result{true, true, e}, ctx.Err()
	}
	result := Result{Accepted: accepted, Evidence: e}
	var target *ep.Decision
	for i := range plan.Decisions {
		if plan.Decisions[i].Resource == root {
			target = &plan.Decisions[i]
		}
	}
	if target == nil {
		return result, ErrInvalid
	}
	if !target.ExecutionEligible {
		return result, nil
	}
	current, err := c.cfg.Branches.Branch(ctx, b.Repository, b.Branch)
	if err != nil {
		var missing *github.MissingBranch
		if errors.As(err, &missing) && missing.Repository == b.Repository && missing.Name == b.Branch {
			return result, ctx.Err()
		}
		return result, err
	}
	if err := ctx.Err(); err != nil {
		return result, err
	}
	if current.Repository != b.Repository || current.Name != b.Branch || !github.ValidCommitSHA(current.SHA) {
		return result, ErrInvalid
	}
	if current.SHA == b.BaseSHA {
		return result, ctx.Err()
	}
	comparison, err := c.cfg.Comparison.CompareCommits(ctx, b.Repository, b.BaseSHA, current.SHA)
	if err != nil {
		return result, err
	}
	if err := ctx.Err(); err != nil {
		return result, err
	}
	if comparison.Repository != b.Repository || comparison.BaseSHA != b.BaseSHA || comparison.HeadSHA != current.SHA || !github.ValidComparison(comparison) {
		return result, ErrInvalid
	}
	if comparison.Status != "ahead" {
		return result, ctx.Err()
	}
	targetItem, err := c.item(ctx, root)
	if err != nil {
		return result, err
	}
	var parent *ep.Decision
	var parentItem observe.ProjectItem
	// Direct Phase implementation progresses only itself, even with a Phase parent.
	isPhase := false
	for _, n := range in.Context.Nodes {
		if n.Resource == root && n.Type == "Phase" {
			isPhase = true
		}
	}
	if !isPhase && target.ParentPhase != nil {
		for i := range plan.Decisions {
			if plan.Decisions[i].Resource == *target.ParentPhase {
				parent = &plan.Decisions[i]
			}
		}
		if parent == nil {
			return result, ErrInvalid
		}
		if parent.StatusOwned {
			parentItem, err = c.item(ctx, parent.Resource)
			if err != nil {
				return result, err
			}
			if parentItem.ID == targetItem.ID {
				return result, ErrProject
			}
		}
	}
	if !accepted {
		e = Evidence{1, b, current.SHA, event.Delivery.ReceivedAt.UTC(), event.Delivery.ReceivedAt.UTC().Format(time.DateOnly)}
		if err := c.record(ctx, firstNamespace, delivery, e); err != nil {
			return result, err
		}
		first.DeliveryID = delivery
		result = Result{Accepted: true, Evidence: e}
	}
	status := func(item observe.ProjectItem) error {
		if err := ctx.Err(); err != nil {
			return err
		}
		if item.Fields[config.Status].OptionID == c.cfg.Project.StatusOptions[config.InProgress] {
			return nil
		}
		return c.cfg.Fields.SetProjectOption(ctx, c.cfg.Project.ID, item.ID, c.cfg.Project.Fields[config.Status], c.cfg.Project.StatusOptions[config.InProgress])
	}
	if target.StatusOwned {
		if err := status(targetItem); err != nil {
			return result, err
		}
	}
	if err := ctx.Err(); err != nil {
		return result, err
	}
	if _, set := targetItem.Fields[config.StartDate]; !set {
		if err := c.cfg.Fields.SetProjectDate(ctx, c.cfg.Project.ID, targetItem.ID, c.cfg.Project.Fields[config.StartDate], e.StartDate); err != nil {
			return result, err
		}
	}
	if parent != nil && parent.StatusOwned {
		if err := status(parentItem); err != nil {
			return result, err
		}
	}
	if err := c.record(ctx, completionNamespace, first.DeliveryID, e); err != nil {
		return result, err
	}
	result.Completed = true
	return result, nil
}
