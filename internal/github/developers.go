package github

import (
	"context"
	"encoding/json"
	"net/http"
	"net/url"
	"strings"

	"github.com/parametron-io/parametron-workflow/internal/config"
)

// DeveloperReads supplies current identity and authorization evidence only.
// Installation tokens need organization Members:read and repository Metadata:read.
// Missing/hidden resources retain NotFound; callers must not infer nonmembership
// from that ambiguous response.
type DeveloperReads interface {
	User(context.Context, string) (Actor, error)
	OrganizationMembership(context.Context, config.Organization, string) (Membership, error)
	RepositoryPermission(context.Context, Repository, string) (Permission, error)
}
type Membership struct {
	Organization config.Organization
	User         Actor
	State        string
}
type Permission struct {
	Repository Repository
	User       Actor
	Base       string
}

func validLogin(s string) bool {
	if len(s) == 0 || len(s) > 39 || s[0] == '-' || s[len(s)-1] == '-' {
		return false
	}
	for _, c := range s {
		if !(c >= 'a' && c <= 'z' || c >= 'A' && c <= 'Z' || c >= '0' && c <= '9' || c == '-') {
			return false
		}
	}
	return !strings.Contains(s, "--")
}
func ValidHuman(a Actor) bool {
	return validLogin(a.Login) && validIDs(a.ID) && a.Type == "User"
}
func SameUser(a, b Actor) bool {
	return ValidHuman(a) && ValidHuman(b) && a.ID == b.ID && strings.EqualFold(a.Login, b.Login)
}

type restUser struct {
	Login, Type string
	ID          int64
	NodeID      string `json:"node_id"`
}

func (u restUser) actor(login string) (Actor, error) {
	a := Actor{Login: u.Login, ID: u.NodeID, Type: u.Type}
	if u.ID <= 0 || !validIDs(u.NodeID) || !strings.EqualFold(login, u.Login) || !validLogin(u.Login) || (u.Type != "User" && u.Type != "Bot" && u.Type != "Organization") {
		return Actor{}, failure(Malformed)
	}
	return a, nil
}
func (t *Transport) developerRead(ctx context.Context, path string, out any) error {
	endpoint, _ := url.Parse(t.endpoint)
	prefix := strings.TrimSuffix(endpoint.Path, "/graphql")
	if prefix == "/api" {
		prefix = "/api/v3"
	}
	endpoint.Path = prefix + path
	body, err := t.request(ctx, http.MethodGet, endpoint.String(), nil)
	if err != nil {
		return err
	}
	if json.Unmarshal(body, out) != nil {
		return failure(Malformed)
	}
	return nil
}
func (t *Transport) User(ctx context.Context, login string) (Actor, error) {
	if !validLogin(login) {
		return Actor{}, failure(Permanent)
	}
	var u restUser
	if err := t.developerRead(ctx, "/users/"+login, &u); err != nil {
		return Actor{}, err
	}
	return u.actor(login)
}
func (t *Transport) OrganizationMembership(ctx context.Context, org config.Organization, login string) (Membership, error) {
	if blank(org.ID) || !validLogin(org.Login) || !validLogin(login) {
		return Membership{}, failure(Permanent)
	}
	var w struct {
		State        string
		User         restUser
		Organization struct {
			Login  string
			NodeID string `json:"node_id"`
		}
	}
	if err := t.developerRead(ctx, "/orgs/"+org.Login+"/memberships/"+login, &w); err != nil {
		return Membership{}, err
	}
	user, err := w.User.actor(login)
	if err != nil {
		return Membership{}, err
	}
	if w.Organization.NodeID != org.ID || !strings.EqualFold(w.Organization.Login, org.Login) || blank(w.State) {
		return Membership{}, failure(Malformed)
	}
	return Membership{org, user, w.State}, nil
}
func (t *Transport) RepositoryPermission(ctx context.Context, repo Repository, login string) (Permission, error) {
	if !ValidBranchRepository(repo) || !validLogin(login) {
		return Permission{}, failure(Permanent)
	}
	// REST's permission response has no repository node identity. Verify the
	// requested repository through the existing identity read before attributing it.
	current, err := t.Repository(ctx, repo.Owner, repo.Name)
	if err != nil {
		return Permission{}, err
	}
	if current != repo {
		return Permission{}, failure(Malformed)
	}
	var w struct {
		Permission string
		User       restUser
	}
	if err := t.developerRead(ctx, "/repos/"+repo.Owner+"/"+repo.Name+"/collaborators/"+login+"/permission", &w); err != nil {
		return Permission{}, err
	}
	user, err := w.User.actor(login)
	if err != nil {
		return Permission{}, err
	}
	if blank(w.Permission) {
		return Permission{}, failure(Malformed)
	}
	// GitHub returns write for maintain/custom roles with effective write access;
	// role_name is deliberately not used to reconstruct base permissions.
	return Permission{repo, user, w.Permission}, nil
}

var _ DeveloperReads = (*Transport)(nil)
