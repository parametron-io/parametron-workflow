package prreview

import (
	"errors"
	"fmt"
	"reflect"
	"strings"
	"time"

	"github.com/parametron-io/parametron-workflow/internal/config"
	eb "github.com/parametron-io/parametron-workflow/internal/engineeringbranch"
	"github.com/parametron-io/parametron-workflow/internal/github"
	"github.com/parametron-io/parametron-workflow/internal/prbinding"
	"github.com/parametron-io/parametron-workflow/internal/pronboarding"
	sp "github.com/parametron-io/parametron-workflow/internal/semanticpolicy"
	"github.com/parametron-io/parametron-workflow/internal/storage"
)

var (
	ErrInvalid     = errors.New("pr review: invalid composition or identity")
	ErrRejected    = errors.New("pr review: rejected authority")
	ErrChanged     = errors.New("pr review: changed current state")
	ErrSuperseded  = errors.New("pr review: cycle superseded by native readiness")
	ErrCorrelation = errors.New("pr review: ambiguous native transition correlation")
	ErrProvenance  = errors.New("pr review: corrupt provenance")
	ErrAmbiguous   = errors.New("pr review: indeterminate draft outcome")
	ErrProject     = errors.New("pr review: conflicting project evidence")
	ErrStorage     = errors.New("pr review: storage failure")
)

type Outcome string

const (
	NoTransition Outcome = "no_transition"
	StartReview  Outcome = "start_review"
	EnterRework  Outcome = "enter_rework"
)

type Reason string

const (
	Unsupported Reason = "unsupported"
	Stale       Reason = "stale"
	Duplicate   Reason = "duplicate"
	Commented   Reason = "commented"
	Approved    Reason = "approved"
)

type Cycle struct {
	Ready          github.ReviewTransition
	Draft          github.ReviewTransition
	Rework         *github.ReviewSubmission
	DraftCompleted bool
	DraftAt        time.Time
}
type Input struct {
	Candidate            Candidate
	Binding              prbinding.Decision
	Onboarding           pronboarding.Completion
	PR                   github.PullRequest
	Facts                github.ReviewFacts
	Current              *Cycle
	PROwned, TargetOwned bool
}
type Decision struct {
	Outcome                              Outcome
	Reason                               Reason
	CycleID, EffectID                    string
	Ready                                github.ReviewTransition
	Review                               github.ReviewSubmission
	Status                               config.StatusRole
	PRStatus, TargetStatus, ConvertDraft bool
}

func validID(s string) bool { return s != "" && !strings.ContainsAny(s, " \t\r\n") }
func sameActor(a, b github.Actor) bool {
	return validID(a.ID) && a.ID == b.ID && a.Login != "" && strings.EqualFold(a.Login, b.Login) && a.Type != "" && a.Type == b.Type
}
func sameRepository(a, b github.Repository) bool {
	return a.ID == b.ID && strings.EqualFold(a.Owner, b.Owner) && strings.EqualFold(a.Name, b.Name)
}
func resource(i github.Identity, kind string) storage.Resource {
	return storage.Resource{Owner: strings.ToLower(i.Repository.Owner), Repository: strings.ToLower(i.Repository.Name), Kind: kind, Number: int64(i.Number), NodeID: i.ID}
}
func key(r storage.Resource) string {
	return fmt.Sprintf("%s/%s/%s/%d", r.Owner, r.Repository, r.Kind, r.Number)
}
func sameBinding(a, b prbinding.Evidence, head bool) bool {
	return a.PR == b.PR && a.Target == b.Target && a.Kind == b.Kind && a.Policy == b.Policy && github.SameUser(a.Developer, b.Developer) && a.Branch.Repository == b.Branch.Repository && a.Branch.Name == b.Branch.Name && (!head || a.Branch.SHA == b.Branch.SHA)
}
func validEvidence(b prbinding.Evidence) bool {
	name, err := eb.Name(resource(b.Target, "issue"))
	return err == nil && validID(b.PR.ID) && b.PR.Number > 0 && github.ValidBranchRepository(b.PR.Repository) && validID(b.Target.ID) && b.Target.Number > 0 && github.ValidBranchRepository(b.Target.Repository) && (b.Kind == sp.Task || b.Kind == sp.Feature || b.Kind == sp.Phase) && github.ValidHuman(b.Developer) && b.Policy == "active_org_member_repository_write_assignee_v1" && sameRepository(b.Branch.Repository, b.Target.Repository) && b.Branch.Name == name && github.ValidCommitSHA(b.Branch.SHA)
}
func equalTransition(a, b github.ReviewTransition) bool {
	return a.ID == b.ID && a.Kind == b.Kind && a.PR == b.PR && a.CreatedAt.Equal(b.CreatedAt) && ((a.Actor == nil && b.Actor == nil) || a.Actor != nil && b.Actor != nil && *a.Actor == *b.Actor)
}

// NativeOrder validates exact identities and strict time ordering. Equal native
// timestamps cannot establish which lifecycle action came first and fail closed.
func NativeOrder(f github.ReviewFacts, pr github.Identity) error {
	if f.PR != pr {
		return ErrChanged
	}
	ids := map[string]bool{}
	var at time.Time
	for _, t := range f.Transitions {
		if !validID(t.ID) || t.PR != pr || t.CreatedAt.IsZero() || ids[t.ID] || (t.Kind != "ReadyForReviewEvent" && t.Kind != "ConvertToDraftEvent") {
			return ErrInvalid
		}
		if !at.IsZero() && !t.CreatedAt.After(at) {
			return ErrCorrelation
		}
		at, ids[t.ID] = t.CreatedAt, true
	}
	for _, r := range f.Submissions {
		if !validID(r.ID) || r.PR != pr || ids[r.ID] {
			return ErrInvalid
		}
		ids[r.ID] = true
		switch r.State {
		case "PENDING":
			if !r.SubmittedAt.IsZero() {
				return ErrInvalid
			}
		case "COMMENTED", "APPROVED", "CHANGES_REQUESTED", "DISMISSED":
			if r.SubmittedAt.IsZero() {
				return ErrInvalid
			}
		default:
			return ErrInvalid
		}
	}
	latest := map[string]bool{}
	for _, r := range f.LatestOpinionated {
		if latest[r.ID] {
			return ErrInvalid
		}
		latest[r.ID] = true
		found := false
		for _, submission := range f.Submissions {
			if reflect.DeepEqual(submission, r) {
				found = true
			}
		}
		if !found {
			return ErrChanged
		}
	}
	return nil
}

// Evaluate is pure: current native facts, completed onboarding and fresh binding
// must agree. Notification values only select facts; they supply no grant.
func Evaluate(in Input) (Decision, error) {
	none := func(r Reason) Decision { return Decision{Outcome: NoTransition, Reason: r} }
	if in.Candidate.Kind != "ready" && in.Candidate.Kind != "review" {
		return none(Unsupported), nil
	}
	if in.Binding.Outcome != prbinding.Accepted || in.Binding.Reason != "" {
		return Decision{}, ErrRejected
	}
	b := in.Binding.Evidence
	if !validEvidence(b) {
		return Decision{}, ErrInvalid
	}
	p := in.PR
	if p.Identity != b.PR || p.State != "OPEN" || p.Author == nil || !github.SameUser(*p.Author, b.Developer) || p.HeadRepository != b.Branch.Repository || p.HeadRef != b.Branch.Name || p.HeadSHA != b.Branch.SHA {
		return Decision{}, ErrChanged
	}
	o := in.Onboarding
	if !o.Completed || o.Resource != resource(b.PR, "pull_request") || o.Project == "" || o.CompletedAt.IsZero() || !sameBinding(o.Binding, b, false) {
		return Decision{}, ErrRejected
	}
	if err := NativeOrder(in.Facts, b.PR); err != nil {
		return Decision{}, err
	}
	c := in.Candidate
	if c.PRID != p.ID || c.Number != int64(p.Number) || c.At.IsZero() {
		return Decision{}, ErrChanged
	}
	if in.Current != nil && in.Current.Rework != nil && !in.Current.DraftCompleted {
		return Decision{}, ErrAmbiguous
	}
	if in.Current != nil {
		found := false
		for _, t := range in.Facts.Transitions {
			if equalTransition(t, in.Current.Ready) {
				found = true
			}
		}
		if !found {
			return Decision{}, ErrChanged
		}
	}
	if c.Kind == "ready" {
		var selected *github.ReviewTransition
		index := -1
		for i, t := range in.Facts.Transitions {
			if t.Kind == "ReadyForReviewEvent" && t.CreatedAt.Equal(c.At) && t.Actor != nil && github.SameUser(*t.Actor, c.Actor) {
				if selected != nil {
					return Decision{}, ErrCorrelation
				}
				copy := t
				selected, index = &copy, i
			}
		}
		if selected == nil {
			return Decision{}, ErrCorrelation
		}
		t := *selected
		if !t.CreatedAt.After(o.CompletedAt) || index != len(in.Facts.Transitions)-1 {
			return none(Stale), nil
		}
		if t.Actor == nil || !github.SameUser(*t.Actor, b.Developer) {
			return Decision{}, ErrRejected
		}
		if p.Draft {
			return none(Stale), nil
		}
		if in.Current != nil {
			if t.ID == in.Current.Ready.ID {
				return none(Duplicate), nil
			}
			if !t.CreatedAt.After(in.Current.Ready.CreatedAt) {
				return none(Stale), nil
			}
			previousIndex := -1
			for i, old := range in.Facts.Transitions {
				if equalTransition(old, in.Current.Ready) {
					previousIndex = i
				}
			}
			if in.Current.Rework == nil || !in.Current.DraftCompleted || !t.CreatedAt.After(in.Current.DraftAt) || previousIndex < 0 || index != previousIndex+2 || !equalTransition(in.Facts.Transitions[index-1], in.Current.Draft) || in.Current.Draft.Kind != "ConvertToDraftEvent" || in.Current.Draft.CreatedAt.Before(in.Current.Rework.SubmittedAt) {
				return Decision{}, ErrRejected
			}
		} else {
			// Completion supplies the applicable original Draft state. Another
			// post-onboarding readiness cannot be silently adopted without its event.
			for _, old := range in.Facts.Transitions[:index] {
				if old.CreatedAt.After(o.CompletedAt) {
					return Decision{}, ErrRejected
				}
			}
		}
		return Decision{Outcome: StartReview, CycleID: t.ID, Ready: t, Status: config.InReview, PRStatus: in.PROwned, TargetStatus: in.TargetOwned}, nil
	}
	var review *github.ReviewSubmission
	for _, r := range in.Facts.Submissions {
		if r.ID == c.ReviewID {
			copy := r
			review = &copy
		}
	}
	if review == nil || review.State == "DISMISSED" || review.State == "PENDING" {
		return none(Stale), nil
	}
	if review.Author == nil || !sameActor(*review.Author, c.Actor) || review.State != c.State || !review.SubmittedAt.Equal(c.At) {
		return Decision{}, ErrChanged
	}
	if review.State == "COMMENTED" {
		return none(Commented), nil
	}
	if review.State == "APPROVED" {
		return none(Approved), nil
	}
	currentOpinion := false
	for _, r := range in.Facts.LatestOpinionated {
		if reflect.DeepEqual(r, *review) {
			currentOpinion = true
		}
	}
	if !currentOpinion {
		return none(Stale), nil
	}
	if in.Current == nil || !review.SubmittedAt.After(in.Current.Ready.CreatedAt) {
		return none(Stale), nil
	}
	for _, transition := range in.Facts.Transitions {
		if transition.Kind == "ReadyForReviewEvent" && transition.CreatedAt.After(in.Current.Ready.CreatedAt) {
			return none(Stale), nil
		}
	}
	if in.Current.Rework != nil {
		return none(Duplicate), nil
	}
	if len(in.Facts.Transitions) == 0 || in.Facts.Transitions[len(in.Facts.Transitions)-1].ID != in.Current.Ready.ID || p.Draft {
		return none(Stale), nil
	}
	return Decision{Outcome: EnterRework, CycleID: in.Current.Ready.ID, EffectID: review.ID, Ready: in.Current.Ready, Review: *review, Status: config.InProgress, PRStatus: in.PROwned, TargetStatus: in.TargetOwned, ConvertDraft: true}, nil
}
