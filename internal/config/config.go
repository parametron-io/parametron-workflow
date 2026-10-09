// Package config validates deployment names and binds transport-independent
// GitHub schema data. It performs no discovery, policy transitions, or I/O
// other than reading the explicitly supplied JSON reader.
package config

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"sort"
	"strings"
)

type Profile string

const (
	Engineering Profile = "engineering"
	BugTracker  Profile = "bug_tracker"
)

type FieldRole string

const (
	Status        FieldRole = "status"
	Priority      FieldRole = "priority"
	Effort        FieldRole = "effort"
	Estimate      FieldRole = "estimate"
	StartDate     FieldRole = "start_date"
	PriorityScore FieldRole = "priority_score"
)

type StatusRole string

const (
	Backlog    StatusRole = "backlog"
	Ready      StatusRole = "ready"
	InProgress StatusRole = "in_progress"
	InReview   StatusRole = "in_review"
	Blocked    StatusRole = "blocked"
	Done       StatusRole = "done"
	ToTriage   StatusRole = "to_triage"
)

var profiles = []Profile{Engineering, BugTracker}

func fieldRoles(p Profile) []FieldRole {
	roles := []FieldRole{Status, Priority, Effort, Estimate, StartDate}
	if p == BugTracker {
		roles = append(roles, PriorityScore)
	}
	return roles
}
func statusRoles(p Profile) []StatusRole {
	roles := []StatusRole{Backlog, Ready, InProgress, InReview, Done}
	if p == Engineering {
		return append(roles, Blocked)
	}
	return append(roles, ToTriage)
}

// SourceConfig contains deployment names only. Map keys are fixed Go-owned
// semantic roles, checked by Validate; they cannot define new workflow policy.
type SourceConfig struct {
	Organization string                     `json:"organization"`
	Repositories []string                   `json:"repositories"`
	Projects     map[Profile]ProjectBinding `json:"projects"`
}
type ProjectBinding struct {
	Number        int                   `json:"number"`
	Fields        map[FieldRole]string  `json:"fields"`
	StatusOptions map[StatusRole]string `json:"status_options"`
}

// Parse strictly decodes one JSON value and validates its required bindings.
func Parse(r io.Reader) (SourceConfig, error) {
	data, err := io.ReadAll(r)
	if err != nil {
		return SourceConfig{}, fmt.Errorf("configuration read: %w", err)
	}
	// encoding/json otherwise silently accepts repeated object keys. Reject
	// them before decoding so conflicting bindings cannot overwrite each other.
	if err := checkKeys(json.NewDecoder(bytes.NewReader(data)), "configuration"); err != nil {
		return SourceConfig{}, fmt.Errorf("configuration JSON: %w", err)
	}
	var source SourceConfig
	d := json.NewDecoder(bytes.NewReader(data))
	d.DisallowUnknownFields()
	if err := d.Decode(&source); err != nil {
		return SourceConfig{}, fmt.Errorf("configuration JSON: %w", err)
	}
	var extra any
	if err := d.Decode(&extra); err != io.EOF {
		if err == nil {
			err = errors.New("trailing JSON value")
		}
		return SourceConfig{}, fmt.Errorf("configuration JSON: %w", err)
	}
	if err := source.Validate(); err != nil {
		return SourceConfig{}, err
	}
	return source, nil
}

func checkKeys(d *json.Decoder, path string) error {
	token, err := d.Token()
	if err != nil {
		return err
	}
	switch token {
	case json.Delim('{'):
		seen := map[string]bool{}
		for d.More() {
			key, err := d.Token()
			if err != nil {
				return err
			}
			name, ok := key.(string)
			if !ok {
				return fmt.Errorf("%s: invalid object key", path)
			}
			if seen[name] {
				return fmt.Errorf("%s.%s: duplicate JSON property", path, name)
			}
			seen[name] = true
			if err := checkKeys(d, path+"."+name); err != nil {
				return err
			}
		}
		_, err = d.Token()
		return err
	case json.Delim('['):
		for i := 0; d.More(); i++ {
			if err := checkKeys(d, fmt.Sprintf("%s[%d]", path, i)); err != nil {
				return err
			}
		}
		_, err = d.Token()
		return err
	default:
		return nil
	}
}

// Validate reports all independent errors in stable path order. Names are not
// trimmed or case-folded during matching; blank names are invalid.
func (s SourceConfig) Validate() error {
	var problems []string
	required := func(path, value string) {
		if strings.TrimSpace(value) == "" {
			problems = append(problems, path+": required non-empty name")
		}
	}
	required("organization", s.Organization)
	if len(s.Repositories) == 0 {
		problems = append(problems, "repositories: at least one repository required")
	}
	seen := map[string]bool{}
	for i, name := range s.Repositories {
		path := fmt.Sprintf("repositories[%d]", i)
		required(path, name)
		if seen[name] {
			problems = append(problems, fmt.Sprintf("%s: duplicate repository %q", path, name))
		}
		seen[name] = true
	}
	for p := range s.Projects {
		if p != Engineering && p != BugTracker {
			problems = append(problems, fmt.Sprintf("projects.%s: unknown profile", p))
		}
	}
	numbers := map[int]Profile{}
	for _, p := range profiles {
		path := "projects." + string(p)
		binding, ok := s.Projects[p]
		if !ok {
			problems = append(problems, path+": required project binding")
			continue
		}
		if binding.Number <= 0 {
			problems = append(problems, path+".number: must be positive")
		} else {
			if previous, ok := numbers[binding.Number]; ok {
				problems = append(problems, fmt.Sprintf("%s.number: project %d already bound to %s", path, binding.Number, previous))
			}
			numbers[binding.Number] = p
		}
		fields := map[FieldRole]bool{}
		names := map[string]FieldRole{}
		for _, role := range fieldRoles(p) {
			fields[role] = true
			name := binding.Fields[role]
			required(path+".fields."+string(role), name)
			if name != "" {
				if previous, ok := names[name]; ok {
					problems = append(problems, fmt.Sprintf("%s.fields.%s: name %q already bound to %s", path, role, name, previous))
				}
				names[name] = role
			}
		}
		for role := range binding.Fields {
			if !fields[role] {
				problems = append(problems, fmt.Sprintf("%s.fields.%s: unsupported field role", path, role))
			}
		}
		statuses := map[StatusRole]bool{}
		optionNames := map[string]StatusRole{}
		for _, role := range statusRoles(p) {
			statuses[role] = true
			name := binding.StatusOptions[role]
			required(path+".status_options."+string(role), name)
			if name != "" {
				if previous, ok := optionNames[name]; ok {
					problems = append(problems, fmt.Sprintf("%s.status_options.%s: name %q already bound to %s", path, role, name, previous))
				}
				optionNames[name] = role
			}
		}
		for role := range binding.StatusOptions {
			if !statuses[role] {
				problems = append(problems, fmt.Sprintf("%s.status_options.%s: unsupported status role", path, role))
			}
		}
	}
	if len(problems) == 0 {
		return nil
	}
	sort.Strings(problems)
	return errors.New(strings.Join(problems, "\n"))
}
