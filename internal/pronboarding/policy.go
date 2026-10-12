package pronboarding

import (
	"fmt"
	"strings"

	eb "github.com/parametron-io/parametron-workflow/internal/engineeringbranch"
	"github.com/parametron-io/parametron-workflow/internal/github"
	"github.com/parametron-io/parametron-workflow/internal/intent"
	"github.com/parametron-io/parametron-workflow/internal/prbinding"
	sp "github.com/parametron-io/parametron-workflow/internal/semanticpolicy"
	"github.com/parametron-io/parametron-workflow/internal/storage"
)

func canonical(r storage.Resource) storage.Resource {
	r.Owner = strings.ToLower(r.Owner)
	r.Repository = strings.ToLower(r.Repository)
	r.NodeID = ""
	return r
}
func key(r storage.Resource) string {
	return fmt.Sprintf("%s/%s/pull_request/%d", r.Owner, r.Repository, r.Number)
}
func sameIdentity(a, b github.Identity) bool {
	return a.ID == b.ID && a.Number == b.Number && a.Repository.ID == b.Repository.ID && strings.EqualFold(a.Repository.Owner, b.Repository.Owner) && strings.EqualFold(a.Repository.Name, b.Repository.Name)
}
func sameBinding(a, b prbinding.Evidence, head bool) bool {
	return sameIdentity(a.PR, b.PR) && sameIdentity(a.Target, b.Target) && a.Kind == b.Kind && a.Policy == b.Policy && github.SameUser(a.Developer, b.Developer) && a.Branch.Repository == b.Branch.Repository && a.Branch.Name == b.Branch.Name && (!head || a.Branch.SHA == b.Branch.SHA)
}
func (c *Controller) configured(i github.Identity) bool {
	if !validID(i.ID) || i.Number <= 0 {
		return false
	}
	for _, r := range c.cfg.Deployment.Repositories {
		if i.Repository.ID == r.ID && strings.EqualFold(i.Repository.Owner, r.Owner) && strings.EqualFold(i.Repository.Name, r.Name) {
			return true
		}
	}
	return false
}
func (c *Controller) validBinding(e prbinding.Evidence, r storage.Resource) bool {
	name, err := eb.Name(storage.Resource{Owner: strings.ToLower(e.Target.Repository.Owner), Repository: strings.ToLower(e.Target.Repository.Name), Kind: "issue", Number: int64(e.Target.Number)})
	return err == nil && c.configured(e.PR) && c.configured(e.Target) && r.Kind == "pull_request" && r.Number == int64(e.PR.Number) && r.Owner == strings.ToLower(e.PR.Repository.Owner) && r.Repository == strings.ToLower(e.PR.Repository.Name) && (e.Kind == sp.Task || e.Kind == sp.Feature || e.Kind == sp.Phase) && e.Policy == "active_org_member_repository_write_assignee_v1" && github.ValidHuman(e.Developer) && e.Branch.Repository.ID == e.Target.Repository.ID && strings.EqualFold(e.Branch.Repository.Owner, e.Target.Repository.Owner) && strings.EqualFold(e.Branch.Repository.Name, e.Target.Repository.Name) && e.Branch.Name == name && github.ValidCommitSHA(e.Branch.SHA)
}

func owned(p github.PullRequest) (bool, error) {
	i, err := intent.Parse(p.Body, intent.Context{Organization: strings.ToLower(p.Repository.Owner), Repository: strings.ToLower(p.Repository.Name)})
	if err != nil {
		return false, ErrChanged
	}
	return i.Effective().SetStatus, nil
}
