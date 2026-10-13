package github

import (
	"context"
	"encoding/json"
	"errors"
	"reflect"
	"time"
)

// ReviewFactsReader reads native transition and submission identities for one
// exact PR. These observations confer no workflow mutation authority.
type ReviewFactsReader interface {
	PullRequestReviewFacts(context.Context, Identity) (ReviewFacts, error)
}

type ReviewTransition struct {
	ID        string
	Kind      string
	PR        Identity
	Actor     *Actor
	CreatedAt time.Time
}

type ReviewSubmission struct {
	ID          string
	PR          Identity
	Author      *Actor
	State       string
	SubmittedAt time.Time
}

type ReviewFacts struct {
	PR                Identity
	Transitions       []ReviewTransition
	Submissions       []ReviewSubmission
	LatestOpinionated []ReviewSubmission
}

var ErrReviewFactsChanged = errors.New("github: native review facts changed during reads")

type reviewActor struct {
	ID, Login string
	Type      string `json:"__typename"`
}

func (a *reviewActor) normalized() (*Actor, error) {
	if a == nil {
		return nil, nil
	}
	if !validIDs(a.ID) || blank(a.Login) || blank(a.Type) {
		return nil, failure(Malformed)
	}
	return &Actor{ID: a.ID, Login: a.Login, Type: a.Type}, nil
}

func (t *Transport) PullRequestReviewFacts(ctx context.Context, pr Identity) (ReviewFacts, error) {
	if !validIDs(pr.ID) || pr.Number <= 0 || !ValidBranchRepository(pr.Repository) {
		return ReviewFacts{}, failure(Permanent)
	}
	var current wireIdentity
	if err := t.resource(ctx, Ref{Owner: pr.Repository.Owner, Repository: pr.Repository.Name, Number: pr.Number}, "pullRequest", identitySelection, &current); err != nil {
		return ReviewFacts{}, err
	}
	identity, err := current.normalized()
	if err != nil || identity != pr {
		return ReviewFacts{}, failure(Malformed)
	}
	selection := `__typename ... on ReadyForReviewEvent{id createdAt actor{__typename ... on User{id login} ... on Bot{id login}} pullRequest{` + identitySelection + `}} ... on ConvertToDraftEvent{id createdAt actor{__typename ... on User{id login} ... on Bot{id login}} pullRequest{` + identitySelection + `}}`
	nodes, err := t.connection(ctx, pr.ID, "PullRequest", "timelineItems", selection)
	if err != nil {
		return ReviewFacts{}, err
	}
	facts := ReviewFacts{PR: pr, Transitions: []ReviewTransition{}, Submissions: []ReviewSubmission{}}
	seen := map[string]bool{}
	for _, raw := range nodes {
		var w struct {
			Kind        string `json:"__typename"`
			ID          string
			CreatedAt   time.Time
			Actor       *reviewActor
			PullRequest wireIdentity
		}
		if json.Unmarshal(raw, &w) != nil || w.Kind == "" {
			return ReviewFacts{}, failure(Malformed)
		}
		if w.Kind != "ReadyForReviewEvent" && w.Kind != "ConvertToDraftEvent" {
			continue
		}
		identity, err := w.PullRequest.normalized()
		if err != nil || identity != pr || !validIDs(w.ID) || w.CreatedAt.IsZero() || seen[w.ID] {
			return ReviewFacts{}, failure(Malformed)
		}
		actor, err := w.Actor.normalized()
		if err != nil {
			return ReviewFacts{}, err
		}
		seen[w.ID] = true
		facts.Transitions = append(facts.Transitions, ReviewTransition{ID: w.ID, Kind: w.Kind, PR: identity, Actor: actor, CreatedAt: w.CreatedAt.UTC()})
	}
	nodes, err = t.connection(ctx, pr.ID, "PullRequest", "reviews", `id state submittedAt author{__typename ... on User{id login} ... on Bot{id login}} pullRequest{`+identitySelection+`}`)
	if err != nil {
		return ReviewFacts{}, err
	}
	for _, raw := range nodes {
		var w struct {
			ID, State   string
			SubmittedAt *time.Time
			Author      *reviewActor
			PullRequest wireIdentity
		}
		if json.Unmarshal(raw, &w) != nil {
			return ReviewFacts{}, failure(Malformed)
		}
		identity, err := w.PullRequest.normalized()
		if err != nil || identity != pr || !validIDs(w.ID) || seen[w.ID] {
			return ReviewFacts{}, failure(Malformed)
		}
		switch w.State {
		case "PENDING":
			if w.SubmittedAt != nil {
				return ReviewFacts{}, failure(Malformed)
			}
		case "COMMENTED", "APPROVED", "CHANGES_REQUESTED", "DISMISSED":
			if w.SubmittedAt == nil || w.SubmittedAt.IsZero() {
				return ReviewFacts{}, failure(Malformed)
			}
		default:
			return ReviewFacts{}, failure(Malformed)
		}
		author, err := w.Author.normalized()
		if err != nil {
			return ReviewFacts{}, err
		}
		seen[w.ID] = true
		var submitted time.Time
		if w.SubmittedAt != nil {
			submitted = w.SubmittedAt.UTC()
		}
		facts.Submissions = append(facts.Submissions, ReviewSubmission{ID: w.ID, PR: identity, Author: author, State: w.State, SubmittedAt: submitted})
	}
	// GitHub supplies current latest opinions per user. Do not invent reviewer
	// eligibility or decide supersession from webhook ordering.
	nodes, err = t.connection(ctx, pr.ID, "PullRequest", "latestOpinionatedReviews", `id state submittedAt author{__typename ... on User{id login} ... on Bot{id login}} pullRequest{`+identitySelection+`}`)
	if err != nil {
		return ReviewFacts{}, err
	}
	facts.LatestOpinionated = []ReviewSubmission{}
	latestIDs := map[string]bool{}
	for _, raw := range nodes {
		var w struct {
			ID, State   string
			SubmittedAt time.Time
			Author      *reviewActor
			PullRequest wireIdentity
		}
		if json.Unmarshal(raw, &w) != nil || !validIDs(w.ID) || latestIDs[w.ID] || w.SubmittedAt.IsZero() {
			return ReviewFacts{}, failure(Malformed)
		}
		switch w.State {
		case "COMMENTED", "APPROVED", "CHANGES_REQUESTED", "DISMISSED":
		default:
			return ReviewFacts{}, failure(Malformed)
		}
		identity, err := w.PullRequest.normalized()
		if err != nil || identity != pr {
			return ReviewFacts{}, failure(Malformed)
		}
		author, err := w.Author.normalized()
		if err != nil {
			return ReviewFacts{}, err
		}
		review := ReviewSubmission{ID: w.ID, PR: identity, Author: author, State: w.State, SubmittedAt: w.SubmittedAt.UTC()}
		found := false
		for _, submission := range facts.Submissions {
			if reflect.DeepEqual(submission, review) {
				found = true
			}
		}
		if !found {
			return ReviewFacts{}, ErrReviewFactsChanged
		}
		latestIDs[w.ID] = true
		facts.LatestOpinionated = append(facts.LatestOpinionated, review)
	}
	return facts, ctx.Err()
}

var _ ReviewFactsReader = (*Transport)(nil)
