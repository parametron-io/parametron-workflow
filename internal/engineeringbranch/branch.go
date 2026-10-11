// Package engineeringbranch authorizes exact development branches through #35
// and records one-shot evidence. Callers supply fresh, complete lifecycle input.
package engineeringbranch

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"reflect"
	"strings"
	"sync"
	"time"

	ep "github.com/parametron-io/parametron-workflow/internal/engineeringpolicy"
	"github.com/parametron-io/parametron-workflow/internal/github"
	"github.com/parametron-io/parametron-workflow/internal/storage"
)

var (
	ErrInvalid    = errors.New("engineering branch: invalid input")
	ErrProvenance = errors.New("engineering branch: invalid provenance")
	ErrAmbiguous  = errors.New("engineering branch: branch exists without completed creation provenance")
	ErrStorage    = errors.New("engineering branch: storage failure")
)

const (
	authorizationNamespace = "engineering_branch_authorization_v1"
	creationNamespace      = "engineering_branch_creation_v1"
)

// Store reuses the generic immutable provenance API; no branch schema is needed.
type Store interface {
	Provenance(context.Context, string, string) (storage.Provenance, error)
	RecordProvenance(context.Context, storage.Provenance) (bool, error)
	Event(context.Context, string) (storage.Event, error)
}
type Config struct {
	Repositories []github.Repository
	Branches     github.Branches
	Store        Store
	Clock        func() time.Time
}
type Controller struct {
	repositories []github.Repository
	branches     github.Branches
	store        Store
	clock        func() time.Time
	// One controller serializes its calls. The existing worker owns resource lanes
	// and exclusive database/runtime ownership across controllers and restarts.
	mu sync.Mutex
}

// Evidence is immutable authorization metadata. The namespace distinguishes
// reservation from successful creation; both facts carry identical v1 bytes.
// SHA is both the exact authorized base and the validated creation commit.
type Evidence struct {
	Version    int               `json:"version"`
	WorkItem   storage.Resource  `json:"work_item"`
	Repository github.Repository `json:"repository"`
	Branch     string            `json:"branch"`
	BaseRef    string            `json:"base_ref"`
	BaseSHA    string            `json:"base_sha"`
}

// Completion adds only the validated successful-creation provenance time.
// CreatedAt is immutable controller evidence, not the authorization event time.
type Completion struct {
	Completed bool
	Evidence  Evidence
	CreatedAt time.Time
}

type Result struct {
	// Completed is true only with validated durable creation provenance.
	Completed bool
	Evidence  Evidence
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
func New(c Config) (*Controller, error) {
	if absent(c.Store) || absent(c.Branches) || len(c.Repositories) == 0 {
		return nil, ErrInvalid
	}
	names, ids := map[string]bool{}, map[string]bool{}
	repos := append([]github.Repository(nil), c.Repositories...)
	for _, repo := range repos {
		key := strings.ToLower(repo.Owner + "/" + repo.Name)
		if !github.ValidBranchRepository(repo) || names[key] || ids[repo.ID] {
			return nil, ErrInvalid
		}
		names[key], ids[repo.ID] = true, true
	}
	if c.Clock == nil {
		c.Clock = time.Now
	}
	return &Controller{repositories: repos, branches: c.Branches, store: c.Store, clock: c.Clock}, nil
}
func identity(r storage.Resource) storage.Resource {
	r.Owner = strings.ToLower(r.Owner)
	r.Repository = strings.ToLower(r.Repository)
	r.NodeID = ""
	return r
}
func validResource(r storage.Resource) bool {
	return r.Owner != "" && r.Repository != "" && r.Owner == strings.ToLower(r.Owner) && r.Repository == strings.ToLower(r.Repository) && r.Kind == "issue" && validIssueNumber(r.Number) && r.NodeID == "" && github.ValidBranchRepository(github.Repository{ID: "validation", Owner: r.Owner, Name: r.Repository})
}

// Name contains repository-local stable Issue identity, independent of Type/title.
// Its maximum length is 37 ASCII bytes (19 decimal digits in positive int64).
func Name(r storage.Resource) (string, error) {
	if !validResource(identity(r)) {
		return "", ErrInvalid
	}
	name := fmt.Sprintf("development/issue-%d", r.Number)
	if !github.ValidBranchName(name) {
		return "", ErrInvalid
	}
	return name, nil
}
func key(r storage.Resource) string {
	return fmt.Sprintf("%s/%s/issue/%d", r.Owner, r.Repository, r.Number)
}
func (c *Controller) repository(r storage.Resource) (github.Repository, error) {
	for _, repo := range c.repositories {
		if strings.EqualFold(repo.Owner, r.Owner) && strings.EqualFold(repo.Name, r.Repository) {
			return repo, nil
		}
	}
	return github.Repository{}, ErrInvalid
}
func sameBranch(b github.Branch, repo github.Repository, name, sha string) bool {
	return b.Repository.ID == repo.ID && strings.EqualFold(b.Repository.Owner, repo.Owner) && strings.EqualFold(b.Repository.Name, repo.Name) && github.ValidBranchName(b.Name) && (name == "" || b.Name == name) && github.ValidCommitSHA(b.SHA) && (sha == "" || b.SHA == sha)
}
func storageFailure(ctx context.Context, err error) error {
	if ctx.Err() != nil {
		return ctx.Err()
	}
	if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
		return err
	}
	return ErrStorage
}
func (c *Controller) checkDelivery(ctx context.Context, id string, r storage.Resource) error {
	event, err := c.store.Event(ctx, id)
	if errors.Is(err, storage.ErrNotFound) {
		return ErrProvenance
	}
	if err != nil {
		return storageFailure(ctx, err)
	}
	if event.Delivery.ID != id || event.Resource == nil || identity(*event.Resource) != r {
		return ErrProvenance
	}
	return nil
}
func validateEvidence(e Evidence, r storage.Resource, repo github.Repository) bool {
	name, err := Name(r)
	return err == nil && e.Version == 1 && e.WorkItem == r && e.Repository == repo && e.Branch == name && github.ValidBranchName(e.BaseRef) && e.BaseRef != e.Branch && github.ValidCommitSHA(e.BaseSHA)
}
func (c *Controller) read(ctx context.Context, ns string, r storage.Resource, repo github.Repository) (storage.Provenance, Evidence, bool, error) {
	p, err := c.store.Provenance(ctx, ns, key(r))
	if errors.Is(err, storage.ErrNotFound) {
		return storage.Provenance{}, Evidence{}, false, nil
	}
	if err != nil {
		return p, Evidence{}, false, storageFailure(ctx, err)
	}
	var e Evidence
	// Re-encoding equality enforces strict shape, version, duplicate-key rejection,
	// canonical bytes, and no arbitrary prose, unknown fields or trailing values.
	if json.Unmarshal(p.Metadata, &e) != nil {
		return p, e, false, ErrProvenance
	}
	canonical, err := json.Marshal(e)
	if err != nil || !bytes.Equal(canonical, p.Metadata) || !validateEvidence(e, r, repo) || p.Namespace != ns || p.Key != key(r) || p.DeliveryID == "" || p.CreatedAt.IsZero() {
		return p, Evidence{}, false, ErrProvenance
	}
	if err := c.checkDelivery(ctx, p.DeliveryID, r); err != nil {
		return p, Evidence{}, false, err
	}
	return p, e, true, nil
}
func (c *Controller) record(ctx context.Context, ns, delivery string, e Evidence) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	data, err := json.Marshal(e)
	if err != nil {
		return ErrProvenance
	}
	_, err = c.store.RecordProvenance(ctx, storage.Provenance{Namespace: ns, Key: key(e.WorkItem), DeliveryID: delivery, Metadata: data, CreatedAt: c.clock()})
	if errors.Is(err, storage.ErrConflict) {
		return ErrProvenance
	}
	if err != nil {
		return storageFailure(ctx, err)
	}
	return ctx.Err()
}

// Ensure evaluates #35 from explicit authoritative current input, never observed
// Project Status. Input freshness/completeness belongs to the caller (#39).
// The root is the only work item acted on. Existing completion is reusable even
// when current eligibility has changed; authorization alone never proves creation.
// Callers must use the existing worker's exclusive, per-resource execution lanes.
func (c *Controller) Ensure(ctx context.Context, delivery string, in ep.Input) (Result, error) {
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
	r := identity(in.Context.Root)
	if !validResource(r) {
		return Result{}, ErrInvalid
	}
	repo, err := c.repository(r)
	if err != nil {
		return Result{}, err
	}
	if err := c.checkDelivery(ctx, delivery, r); err != nil {
		return Result{}, err
	}
	auth, evidence, authorized, completed, _, err := c.completed(ctx, r, repo)
	if err != nil {
		return Result{}, err
	}
	if completed {
		if err := ctx.Err(); err != nil {
			return Result{}, err
		}
		return Result{true, evidence}, nil
	}
	if err := ctx.Err(); err != nil {
		return Result{}, err
	}
	eligible := false
	for _, d := range plan.Decisions {
		if d.Resource == in.Context.Root {
			eligible = d.ExecutionEligible
		}
	}
	if !eligible {
		return Result{}, nil
	}
	name, err := Name(r)
	if err != nil {
		return Result{}, err
	}
	if !authorized {
		base, err := c.branches.DefaultBranch(ctx, repo)
		if err != nil {
			return Result{}, err
		}
		if !sameBranch(base, repo, "", "") || base.Name == name {
			return Result{}, ErrInvalid
		}
		evidence = Evidence{1, r, repo, name, base.Name, base.SHA}
	}
	if err := ctx.Err(); err != nil {
		return Result{}, err
	}
	// Read the exact reserved name. Existence, even with the exact base SHA, is
	// ambiguous without completion and never adopted or destructively recovered.
	branch, err := c.branches.Branch(ctx, repo, evidence.Branch)
	if err == nil {
		if !sameBranch(branch, repo, evidence.Branch, "") {
			return Result{}, ErrInvalid
		}
		return Result{}, ErrAmbiguous
	}
	var missing *github.MissingBranch
	if !errors.As(err, &missing) {
		return Result{}, err
	}
	if missing.Name != evidence.Branch || missing.Repository != repo {
		return Result{}, ErrInvalid
	}
	if err := ctx.Err(); err != nil {
		return Result{}, err
	}
	if !authorized {
		if err := c.record(ctx, authorizationNamespace, delivery, evidence); err != nil {
			return Result{}, err
		}
		auth.DeliveryID = delivery
	}
	if err := ctx.Err(); err != nil {
		return Result{}, err
	}
	branch, err = c.branches.CreateBranch(ctx, repo, evidence.Branch, evidence.BaseSHA)
	if err != nil {
		return Result{}, err
	}
	if err := ctx.Err(); err != nil {
		return Result{}, err
	}
	if !sameBranch(branch, repo, evidence.Branch, evidence.BaseSHA) {
		return Result{}, ErrInvalid
	}
	if err := c.record(ctx, creationNamespace, auth.DeliveryID, evidence); err != nil {
		return Result{}, err
	}
	return Result{true, evidence}, nil
}

// Completed reads validated completed #37 evidence without GitHub calls or
// creation authority. Authorization alone returns Completed=false.
func (c *Controller) Completed(ctx context.Context, resource storage.Resource) (Completion, error) {
	if err := ctx.Err(); err != nil {
		return Completion{}, err
	}
	r := identity(resource)
	if !validResource(r) {
		return Completion{}, ErrInvalid
	}
	repo, err := c.repository(r)
	if err != nil {
		return Completion{}, err
	}
	_, e, _, done, createdAt, err := c.completed(ctx, r, repo)
	if err != nil {
		return Completion{}, err
	}
	if err := ctx.Err(); err != nil {
		return Completion{}, err
	}
	if !done {
		return Completion{}, nil
	}
	return Completion{Completed: true, Evidence: e, CreatedAt: createdAt}, nil
}
func (c *Controller) completed(ctx context.Context, r storage.Resource, repo github.Repository) (storage.Provenance, Evidence, bool, bool, time.Time, error) {
	auth, e, authorized, err := c.read(ctx, authorizationNamespace, r, repo)
	if err != nil {
		return auth, e, false, false, time.Time{}, err
	}
	completion, created, done, err := c.read(ctx, creationNamespace, r, repo)
	if err != nil {
		return auth, e, false, false, time.Time{}, err
	}
	if done && (!authorized || e != created || auth.DeliveryID != completion.DeliveryID) {
		return auth, e, false, false, time.Time{}, ErrProvenance
	}
	return auth, e, authorized, done, completion.CreatedAt.UTC(), nil
}

// validIssueNumber checks positivity and platform-int range without narrowing.
func validIssueNumber(number int64) bool {
	return number > 0 && number <= math.MaxInt
}
