package github

import (
	"context"
	"encoding/json"
	"net/http"
	"net/url"
	"strings"
	"time"
)

// CommitComparison exposes only exact revisions and their forward relationship.
type CommitComparison interface {
	CompareCommits(context.Context, Repository, string, string) (Comparison, error)
}
type Comparison struct {
	Repository        Repository
	BaseSHA, HeadSHA  string
	Status            string
	AheadBy, BehindBy int
}

func ValidComparison(c Comparison) bool {
	if !ValidBranchRepository(c.Repository) || !ValidCommitSHA(c.BaseSHA) || !ValidCommitSHA(c.HeadSHA) || c.AheadBy < 0 || c.BehindBy < 0 {
		return false
	}
	switch c.Status {
	case "identical":
		return c.BaseSHA == c.HeadSHA && c.AheadBy == 0 && c.BehindBy == 0
	case "ahead":
		return c.BaseSHA != c.HeadSHA && c.AheadBy > 0 && c.BehindBy == 0
	case "behind":
		return c.BaseSHA != c.HeadSHA && c.AheadBy == 0 && c.BehindBy > 0
	case "diverged":
		return c.BaseSHA != c.HeadSHA && c.AheadBy > 0 && c.BehindBy > 0
	}
	return false
}

// CompareCommits uses GitHub's exact SHA REST comparison, after validating both
// commit identities in the configured repository through GraphQL. It reads no
// commit prose/history and shares HTTP/token/error/cancellation mechanics.
func (t *Transport) CompareCommits(ctx context.Context, repo Repository, base, head string) (Comparison, error) {
	if !ValidBranchRepository(repo) || !ValidCommitSHA(base) || !ValidCommitSHA(head) {
		return Comparison{}, failure(Permanent)
	}
	var data struct {
		Repository *struct {
			branchRepository
			Base, Head *struct {
				Typename   string `json:"__typename"`
				OID        string
				Repository *branchRepository
			}
		}
	}
	q := `query($owner:String!,$name:String!,$base:String!,$head:String!){repository(owner:$owner,name:$name){id name owner{login} base:object(expression:$base){__typename ... on Commit{oid repository{id name owner{login}}}} head:object(expression:$head){__typename ... on Commit{oid repository{id name owner{login}}}}}}`
	if err := t.query(ctx, q, map[string]any{"owner": repo.Owner, "name": repo.Name, "base": base, "head": head}, &data); err != nil {
		return Comparison{}, err
	}
	r := data.Repository
	if r == nil {
		return Comparison{}, failure(NotFound)
	}
	if !matchesBranchRepository(&r.branchRepository, repo) || r.Base == nil || r.Head == nil || r.Base.Typename != "Commit" || r.Head.Typename != "Commit" || r.Base.OID != base || r.Head.OID != head || !matchesBranchRepository(r.Base.Repository, repo) || !matchesBranchRepository(r.Head.Repository, repo) {
		return Comparison{}, failure(Malformed)
	}
	endpoint, _ := url.Parse(t.endpoint)
	prefix := strings.TrimSuffix(endpoint.Path, "/graphql")
	if prefix == "/api" {
		prefix = "/api/v3"
	}
	endpoint.Path = prefix + "/repos/" + repo.Owner + "/" + repo.Name + "/compare/" + base + "..." + head
	expectedURL := endpoint.String()
	endpoint.RawQuery = "per_page=1"
	body, err := t.request(ctx, http.MethodGet, endpoint.String(), nil)
	if err != nil {
		return Comparison{}, err
	}
	var got struct {
		URL       string
		Status    string
		AheadBy   *int                  `json:"ahead_by"`
		BehindBy  *int                  `json:"behind_by"`
		Base      *struct{ SHA string } `json:"base_commit"`
		MergeBase *struct{ SHA string } `json:"merge_base_commit"`
	}
	if json.Unmarshal(body, &got) != nil || got.URL != expectedURL || got.AheadBy == nil || got.BehindBy == nil || got.Base == nil || got.Base.SHA != base || got.MergeBase == nil || !ValidCommitSHA(got.MergeBase.SHA) {
		return Comparison{}, failure(Malformed)
	}
	c := Comparison{repo, base, head, got.Status, *got.AheadBy, *got.BehindBy}
	if !ValidComparison(c) || ((c.Status == "ahead" || c.Status == "identical") && got.MergeBase.SHA != base) || (c.Status == "behind" && got.MergeBase.SHA != head) {
		return Comparison{}, failure(Malformed)
	}
	return c, nil
}

// LifecycleFields grants only single-select and DATE writes; policy owns when.
type LifecycleFields interface {
	SetProjectOption(context.Context, string, string, string, string) error
	SetProjectDate(context.Context, string, string, string, string) error
}

func ValidProjectDate(date string) bool {
	d, err := time.Parse(time.DateOnly, date)
	return err == nil && d.Year() >= 1 && d.Format(time.DateOnly) == date
}
func (t *Transport) SetProjectDate(ctx context.Context, project, item, field, date string) error {
	if !validIDs(project, item, field) || !ValidProjectDate(date) {
		return failure(Permanent)
	}
	var data struct {
		Result *struct {
			ProjectV2Item *struct {
				ID      string
				Project *struct{ ID string }
			}
		}
	}
	err := t.query(ctx, `mutation($input:UpdateProjectV2ItemFieldValueInput!){result:updateProjectV2ItemFieldValue(input:$input){projectV2Item{id project{id}}}}`, map[string]any{"input": map[string]any{"projectId": project, "itemId": item, "fieldId": field, "value": map[string]any{"date": date}}}, &data)
	if err != nil {
		return err
	}
	if data.Result == nil || data.Result.ProjectV2Item == nil || data.Result.ProjectV2Item.ID != item || data.Result.ProjectV2Item.Project == nil || data.Result.ProjectV2Item.Project.ID != project {
		return failure(Malformed)
	}
	return nil
}

var _ CommitComparison = (*Transport)(nil)
var _ LifecycleFields = (*Transport)(nil)
