package github

import (
	"context"
	"encoding/json"
	"strings"
)

// Branches is a separate exact-ref capability, not workflow authorization.
// Names are short heads names; creation accepts only an exact commit SHA.
type Branches interface {
	DefaultBranch(context.Context, Repository) (Branch, error)
	Branch(context.Context, Repository, string) (Branch, error)
	CreateBranch(context.Context, Repository, string, string) (Branch, error)
}

type Branch struct {
	Repository Repository
	Name       string
	SHA        string
}

// MissingBranch proves that an exact heads lookup returned an explicit null ref
// in the validated repository. Generic NotFound (including HTTP 404 or a missing
// repository) does not prove branch absence and must not authorize creation.
// It retains the existing NotFound taxonomy through Unwrap.
type MissingBranch struct {
	Repository Repository
	Name       string
}

func (*MissingBranch) Error() string { return "github: exact branch absent" }
func (*MissingBranch) Unwrap() error { return failure(NotFound) }

// ValidBranchName accepts a bounded ASCII subset of Git heads names. It excludes
// revision expressions, full ref names, and invalid Git ref components.
func ValidBranchName(name string) bool {
	if len(name) == 0 || len(name) > 255 || strings.HasPrefix(name, "refs/") || strings.Contains(name, "..") {
		return false
	}
	for _, part := range strings.Split(name, "/") {
		if part == "" || part[0] == '.' || part[0] == '-' || strings.HasSuffix(part, ".") || strings.HasSuffix(part, ".lock") {
			return false
		}
		for _, c := range part {
			if !(c >= 'a' && c <= 'z' || c >= 'A' && c <= 'Z' || c >= '0' && c <= '9' || c == '-' || c == '_' || c == '.') {
				return false
			}
		}
	}
	return true
}
func ValidCommitSHA(sha string) bool {
	if len(sha) != 40 {
		return false
	}
	for _, c := range sha {
		if !(c >= '0' && c <= '9' || c >= 'a' && c <= 'f') {
			return false
		}
	}
	return true
}
func ValidBranchRepository(repo Repository) bool {
	if !validIDs(repo.ID) {
		return false
	}
	for _, s := range []string{repo.Owner, repo.Name} {
		if s == "" || s == "." || s == ".." || len(s) > 100 {
			return false
		}
		for _, c := range s {
			if !(c >= 'a' && c <= 'z' || c >= 'A' && c <= 'Z' || c >= '0' && c <= '9' || c == '-' || c == '_' || c == '.') {
				return false
			}
		}
	}
	return true
}

type branchRepository struct {
	ID, Name string
	Owner    *struct{ Login string }
}
type branchResponse struct {
	Name, Prefix string
	Repository   *branchRepository
	Target       *struct {
		Typename string `json:"__typename"`
		OID      string
	}
}

const branchSelection = `name prefix repository{id name owner{login}} target{__typename ... on Commit{oid}}`

func matchesBranchRepository(got *branchRepository, want Repository) bool {
	return got != nil && got.Owner != nil && got.ID == want.ID && strings.EqualFold(got.Name, want.Name) && strings.EqualFold(got.Owner.Login, want.Owner)
}
func decodeBranch(ref *branchResponse, repo Repository, name string) (Branch, error) {
	if ref == nil || !matchesBranchRepository(ref.Repository, repo) || ref.Prefix != "refs/heads/" || !ValidBranchName(ref.Name) || (name != "" && ref.Name != name) || ref.Target == nil || ref.Target.Typename != "Commit" || !ValidCommitSHA(ref.Target.OID) {
		return Branch{}, failure(Malformed)
	}
	return Branch{repo, ref.Name, ref.Target.OID}, nil
}
func (t *Transport) readBranch(ctx context.Context, repo Repository, name string) (Branch, error) {
	if !ValidBranchRepository(repo) || (name != "" && !ValidBranchName(name)) {
		return Branch{}, failure(Permanent)
	}
	var data struct {
		Repository json.RawMessage
	}
	field := `ref(qualifiedName:$ref)`
	decl := `,$ref:String!`
	variables := map[string]any{"owner": repo.Owner, "name": repo.Name}
	if name == "" {
		field = `defaultBranchRef`
		decl = ""
	} else {
		variables["ref"] = "refs/heads/" + name
	}
	q := `query($owner:String!,$name:String!` + decl + `){repository(owner:$owner,name:$name){id name owner{login} ref:` + field + `{` + branchSelection + `}}}`
	if err := t.query(ctx, q, variables, &data); err != nil {
		return Branch{}, err
	}
	if len(data.Repository) == 0 {
		return Branch{}, failure(Malformed)
	}
	if string(data.Repository) == "null" {
		return Branch{}, failure(NotFound)
	}
	var repository struct {
		branchRepository
		Ref json.RawMessage
	}
	if json.Unmarshal(data.Repository, &repository) != nil || !matchesBranchRepository(&repository.branchRepository, repo) || len(repository.Ref) == 0 {
		return Branch{}, failure(Malformed)
	}
	if string(repository.Ref) == "null" {
		if name != "" {
			return Branch{}, &MissingBranch{Repository: repo, Name: name}
		}
		return Branch{}, failure(NotFound)
	}
	var ref branchResponse
	if json.Unmarshal(repository.Ref, &ref) != nil {
		return Branch{}, failure(Malformed)
	}
	return decodeBranch(&ref, repo, name)
}
func (t *Transport) DefaultBranch(ctx context.Context, repo Repository) (Branch, error) {
	return t.readBranch(ctx, repo, "")
}
func (t *Transport) Branch(ctx context.Context, repo Repository, name string) (Branch, error) {
	if !ValidBranchName(name) {
		return Branch{}, failure(Permanent)
	}
	return t.readBranch(ctx, repo, name)
}
func (t *Transport) CreateBranch(ctx context.Context, repo Repository, name, sha string) (Branch, error) {
	if !ValidBranchRepository(repo) || !ValidBranchName(name) || !ValidCommitSHA(sha) {
		return Branch{}, failure(Permanent)
	}
	var data struct {
		Result *struct{ Ref *branchResponse }
	}
	err := t.query(ctx, `mutation($input:CreateRefInput!){result:createRef(input:$input){ref{`+branchSelection+`}}}`, map[string]any{"input": map[string]any{"repositoryId": repo.ID, "name": "refs/heads/" + name, "oid": sha}}, &data)
	if err != nil {
		return Branch{}, err
	}
	if data.Result == nil {
		return Branch{}, failure(Malformed)
	}
	branch, err := decodeBranch(data.Result.Ref, repo, name)
	if err != nil {
		return Branch{}, err
	}
	if branch.SHA != sha {
		return Branch{}, failure(Malformed)
	}
	return branch, nil
}

var _ Branches = (*Transport)(nil)
