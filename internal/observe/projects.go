package observe

import (
	"math"
	"sort"
	"strings"
	"time"

	"github.com/parametron-io/parametron-workflow/internal/config"
	"github.com/parametron-io/parametron-workflow/internal/github"
)

func normalizeProject(profile config.Profile, p config.ResolvedProject, content, kind string, items []github.ProjectItem) (ProjectState, error) {
	out := ProjectState{Profile: profile, ID: p.ID, Items: []ProjectItem{}}
	contentKind := "Issue"
	if kind == "pull_request" {
		contentKind = "PullRequest"
	}
	seen := map[string]bool{}
	roles := make([]config.FieldRole, 0, len(p.Fields))
	for role := range p.Fields {
		roles = append(roles, role)
	}
	sort.Slice(roles, func(i, j int) bool { return roles[i] < roles[j] })
	for _, item := range items {
		if strings.TrimSpace(item.ID) == "" || seen[item.ID] || item.ProjectID != p.ID || item.Content == nil || item.Content.ID != content || item.Content.Kind != contentKind {
			return ProjectState{}, ErrObservation
		}
		seen[item.ID] = true
		value := ProjectItem{ID: item.ID, Archived: item.Archived, Fields: map[config.FieldRole]FieldValue{}}
		// A Client promises only configured fields; reject inconsistent clients.
		if len(item.Values) > len(p.Fields) {
			return ProjectState{}, ErrObservation
		}
		count := 0
		for _, role := range roles {
			v, ok := item.Values[p.Fields[role]]
			if !ok {
				continue
			}
			count++
			if v.Kind != fieldKind(role) {
				return ProjectState{}, ErrObservation
			}
			switch v.Kind {
			case config.SingleSelect:
				if strings.TrimSpace(v.OptionID) == "" || v.Date != "" || v.Number != 0 {
					return ProjectState{}, ErrObservation
				}
			case config.Number:
				if math.IsNaN(v.Number) || math.IsInf(v.Number, 0) || v.OptionID != "" || v.Date != "" {
					return ProjectState{}, ErrObservation
				}
			case config.Date:
				if _, err := time.Parse(time.DateOnly, v.Date); err != nil || v.OptionID != "" || v.Number != 0 {
					return ProjectState{}, ErrObservation
				}
			}
			field := FieldValue{FieldValue: v}
			if role == config.Status {
				for status, id := range p.StatusOptions {
					if id == v.OptionID {
						field.StatusRole = status
					}
				}
			}
			value.Fields[role] = field
		}
		if count != len(item.Values) {
			return ProjectState{}, ErrObservation
		}
		out.Items = append(out.Items, value)
	}
	sort.Slice(out.Items, func(i, j int) bool { return out.Items[i].ID < out.Items[j].ID })
	return out, nil
}
