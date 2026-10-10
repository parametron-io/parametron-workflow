package config

import (
	"fmt"
	"strings"
)

// Schema is normalized discovery input, not a GitHub transport interface.
// Issue #9 supplies current organization, repository and Project identities.
type Schema struct {
	Organizations []Organization
	Repositories  []Repository
	Projects      []Project
	IssueTypes    []IssueType
}
type IssueType struct{ ID, Name string }
type Organization struct{ ID, Login string }
type Repository struct{ ID, Owner, Name string }
type Project struct {
	ID, Owner string
	Number    int
	Fields    []Field
}
type FieldKind string

const (
	SingleSelect FieldKind = "single_select"
	Number       FieldKind = "number"
	Date         FieldKind = "date"
)

type Field struct {
	ID, Name string
	Kind     FieldKind
	Options  []Option
}
type Option struct{ ID, Name string }

// ResolvedConfig is distinct from source configuration. Every ID originates in
// discovery input. Binding maps and slices are newly allocated by Resolve.
type ResolvedConfig struct {
	Organization Organization
	Repositories []Repository
	Engineering  ResolvedProject
	BugTracker   ResolvedProject
	IssueTypes   map[string]string
}
type ResolvedProject struct {
	ID            string
	Number        int
	Fields        map[FieldRole]string
	StatusOptions map[StatusRole]string
	FieldOptions  map[FieldRole]map[string]string
}

// unique rejects missing and ambiguous matches rather than depending on input
// order. Even repeated identical records are ambiguous discovery input.
func unique[T any](values []T, match func(T) bool, path string) (T, error) {
	var result T
	count := 0
	for _, v := range values {
		if match(v) {
			result = v
			count++
		}
	}
	if count != 1 {
		return result, fmt.Errorf("%s: expected exactly one match, found %d", path, count)
	}
	return result, nil
}
func requireID(path, id string) error {
	if strings.TrimSpace(id) == "" {
		return fmt.Errorf("%s: discovered ID is empty", path)
	}
	return nil
}

// Resolve validates source config again so callers cannot bypass validation by
// constructing SourceConfig directly. No partial configuration is returned.
func Resolve(s SourceConfig, schema Schema) (ResolvedConfig, error) {
	if err := s.Validate(); err != nil {
		return ResolvedConfig{}, err
	}
	result, err := resolve(s, schema)
	if err != nil {
		return ResolvedConfig{}, err
	}
	return result, nil
}
func resolve(s SourceConfig, schema Schema) (ResolvedConfig, error) {
	var result ResolvedConfig
	org, err := unique(schema.Organizations, func(o Organization) bool { return o.Login == s.Organization }, "organization "+s.Organization)
	if err != nil {
		return result, err
	}
	if err := requireID("organization", org.ID); err != nil {
		return result, err
	}
	result.Organization = org
	result.IssueTypes = map[string]string{}
	typeIDs := map[string]bool{}
	for _, typ := range schema.IssueTypes {
		if strings.TrimSpace(typ.Name) == "" || strings.TrimSpace(typ.ID) == "" || result.IssueTypes[typ.Name] != "" || typeIDs[typ.ID] {
			return result, fmt.Errorf("issue_types: blank or duplicate discovery record")
		}
		result.IssueTypes[typ.Name] = typ.ID
		typeIDs[typ.ID] = true
	}
	for i, name := range s.Repositories {
		path := fmt.Sprintf("repositories[%d] %q", i, name)
		repo, err := unique(schema.Repositories, func(r Repository) bool { return r.Owner == s.Organization && r.Name == name }, path)
		if err != nil {
			return result, err
		}
		if err := requireID(path, repo.ID); err != nil {
			return result, err
		}
		for _, previous := range result.Repositories {
			if previous.ID == repo.ID {
				return result, fmt.Errorf("%s: discovered ID duplicates repository %q", path, previous.Name)
			}
		}
		result.Repositories = append(result.Repositories, repo)
	}
	for _, profile := range profiles {
		b := s.Projects[profile]
		path := fmt.Sprintf("projects.%s (owner %q, number %d)", profile, s.Organization, b.Number)
		project, err := unique(schema.Projects, func(p Project) bool { return p.Owner == s.Organization && p.Number == b.Number }, path)
		if err != nil {
			return result, err
		}
		if err := requireID(path, project.ID); err != nil {
			return result, err
		}
		resolved := ResolvedProject{ID: project.ID, Number: project.Number, Fields: map[FieldRole]string{}, StatusOptions: map[StatusRole]string{}, FieldOptions: map[FieldRole]map[string]string{}}
		for _, role := range fieldRoles(profile) {
			fieldPath := fmt.Sprintf("%s.fields.%s (%q)", path, role, b.Fields[role])
			field, err := unique(project.Fields, func(f Field) bool { return f.Name == b.Fields[role] }, fieldPath)
			if err != nil {
				return result, err
			}
			kind := SingleSelect
			switch role {
			case Estimate, PriorityScore:
				kind = Number
			case StartDate:
				kind = Date
			}
			if field.Kind != kind {
				return result, fmt.Errorf("%s: incompatible kind %q; require %q", fieldPath, field.Kind, kind)
			}
			if err := requireID(fieldPath, field.ID); err != nil {
				return result, err
			}
			for _, other := range fieldRoles(profile) {
				if resolved.Fields[other] == field.ID {
					return result, fmt.Errorf("%s: discovered ID duplicates field role %s", fieldPath, other)
				}
			}
			resolved.Fields[role] = field.ID
			if field.Kind == SingleSelect {
				options := map[string]string{}
				ids := map[string]bool{}
				for _, option := range field.Options {
					if strings.TrimSpace(option.Name) == "" || strings.TrimSpace(option.ID) == "" {
						return result, fmt.Errorf("%s: blank option", fieldPath)
					}
					if options[option.Name] != "" {
						return result, fmt.Errorf("%s: expected exactly one option match, found 2", fieldPath)
					}
					if ids[option.ID] {
						return result, fmt.Errorf("%s: discovered option ID duplicates %s option", fieldPath, role)
					}
					options[option.Name], ids[option.ID] = option.ID, true
				}
				resolved.FieldOptions[role] = options
			}
			if role == Status {
				for _, status := range statusRoles(profile) {
					optionPath := fmt.Sprintf("%s.status_options.%s (%q)", path, status, b.StatusOptions[status])
					option, err := unique(field.Options, func(o Option) bool { return o.Name == b.StatusOptions[status] }, optionPath)
					if err != nil {
						return result, err
					}
					if err := requireID(optionPath, option.ID); err != nil {
						return result, err
					}
					for _, other := range statusRoles(profile) {
						if resolved.StatusOptions[other] == option.ID {
							return result, fmt.Errorf("%s: discovered ID duplicates status role %s", optionPath, other)
						}
					}
					resolved.StatusOptions[status] = option.ID
				}
			}
		}
		if profile == Engineering {
			result.Engineering = resolved
		} else {
			result.BugTracker = resolved
		}
	}
	if result.Engineering.ID == result.BugTracker.ID {
		return result, fmt.Errorf("projects.bug_tracker: discovered ID duplicates engineering Project")
	}
	return result, nil
}
