// Package intent parses explicit body syntax only. It performs no observation,
// semantic judgement, lifecycle decision, or external side effect.
package intent

import (
	"errors"
	"fmt"
	"strconv"
	"strings"
)

// Context identifies the current repository; Organization must be parametron-io.
type Context struct{ Organization, Repository string }

// Boolean preserves presence and the written value independently of defaults
// and master-switch suppression. Value is meaningful only when Explicit is true.
type Boolean struct{ Value, Explicit bool }
type Booleans struct {
	Automation, Classification, Triage, Audit, Validation, Review Boolean
	SetStatus, SetPosition                                        Boolean
}

// Effective contains global defaults after Automation suppression. Estimate
// indicates only master-switch permission, not eligibility to generate an estimate.
type Effective struct {
	Automation, Classification, Triage, Audit, Validation, Review bool
	SetStatus, SetPosition, AutomaticEstimate                     bool
}

// IssueRef is comparable semantic identity. Repository is always a lowercase
// repository name, including for local references. It contains no transport ID.
type IssueRef struct {
	Repository string
	Number     int64
}
type Intent struct {
	Explicit Booleans
	// Absent CreateBranch remains unresolved until later type-aware policy.
	CreateBranch            Boolean
	Parent, Target          *IssueRef
	BlockedBy, Blocks, Refs []IssueRef
}

// Effective computes values from explicit intent without erasing it.
func (i Intent) Effective() Effective {
	value := func(b Boolean, fallback bool) bool {
		if b.Explicit {
			return b.Value
		}
		return fallback
	}
	a := value(i.Explicit.Automation, true)
	return Effective{
		Automation:        a,
		Classification:    a && value(i.Explicit.Classification, true),
		Triage:            a && value(i.Explicit.Triage, false),
		Audit:             a && value(i.Explicit.Audit, false),
		Validation:        a && value(i.Explicit.Validation, false),
		Review:            a && value(i.Explicit.Review, false),
		SetStatus:         value(i.Explicit.SetStatus, true),
		SetPosition:       value(i.Explicit.SetPosition, true),
		AutomaticEstimate: a,
	}
}

var (
	ErrContext      = errors.New("intent: invalid repository context")
	ErrDirective    = errors.New("intent: malformed directive")
	ErrConflict     = errors.New("intent: contradictory directive")
	ErrReference    = errors.New("intent: malformed reference")
	ErrNoncanonical = errors.New("intent: noncanonical reference")
)

// Error exposes a stable category through errors.Is and bounded line metadata
// through errors.As. It never includes input values or body text.
type Error struct {
	Line      int
	Directive string
	Kind      error
}

func (e *Error) Error() string {
	return fmt.Sprintf("intent: line %d %s: %v", e.Line, e.Directive, e.Kind)
}
func (e *Error) Unwrap() error { return e.Kind }

// Parse recognizes column-one exact Name: lines outside backtick/tilde fences.
// Only ASCII spaces/tabs around and between values are normalized; CRLF is
// accepted. Unknown names, indentation, quotes and prose are ignored.
// On any error the returned Intent is zero, never partially accepted intent.
func Parse(body string, ctx Context) (Intent, error) {
	if ctx.Organization != "parametron-io" || !repositoryName(ctx.Repository) {
		return Intent{}, ErrContext
	}
	ctx.Repository = strings.ToLower(ctx.Repository)
	var out Intent
	var fence byte
	var width int
	for n, line := range strings.Split(body, "\n") {
		line = strings.TrimSuffix(line, "\r")
		marker, count, rest := fenceLine(line)
		if fence != 0 {
			if marker == fence && count >= width && strings.Trim(rest, " \t") == "" {
				fence = 0
			}
			continue
		}
		if marker != 0 {
			fence, width = marker, count
			continue
		}
		name, raw, found := strings.Cut(line, ":")
		if !found {
			continue
		}
		var b *Boolean
		var singular **IssueRef
		var list *[]IssueRef
		switch name {
		case "Automation":
			b = &out.Explicit.Automation
		case "Classification":
			b = &out.Explicit.Classification
		case "Triage":
			b = &out.Explicit.Triage
		case "Audit":
			b = &out.Explicit.Audit
		case "Validation":
			b = &out.Explicit.Validation
		case "Review":
			b = &out.Explicit.Review
		case "Set-Status":
			b = &out.Explicit.SetStatus
		case "Set-Position":
			b = &out.Explicit.SetPosition
		case "Create-Branch":
			b = &out.CreateBranch
		case "Parent":
			singular = &out.Parent
		case "Target":
			singular = &out.Target
		case "Blocked-By":
			list = &out.BlockedBy
		case "Blocks":
			list = &out.Blocks
		case "Refs":
			list = &out.Refs
		default:
			continue
		}
		fail := func(kind error) (Intent, error) { return Intent{}, &Error{n + 1, name, kind} }
		raw = strings.Trim(raw, " \t")
		if raw == "" {
			return fail(ErrDirective)
		}
		if b != nil {
			if raw != "true" && raw != "false" {
				return fail(ErrDirective)
			}
			v := raw == "true"
			if b.Explicit && b.Value != v {
				return fail(ErrConflict)
			}
			*b = Boolean{v, true}
			continue
		}
		values := strings.FieldsFunc(raw, func(r rune) bool { return r == ' ' || r == '\t' })
		for _, v := range values {
			ref, err := reference(v, ctx)
			if err != nil {
				return fail(err)
			}
			if singular != nil {
				if *singular != nil && **singular != ref {
					return fail(ErrConflict)
				}
				copy := ref
				*singular = &copy
			} else {
				// Small ordered collections: equality scans avoid shared state and map order.
				duplicate := false
				for _, existing := range *list {
					if existing == ref {
						duplicate = true
						break
					}
				}
				if !duplicate {
					*list = append(*list, ref)
				}
			}
		}
	}
	return out, nil
}

func reference(s string, ctx Context) (IssueRef, error) {
	repo, number, ok := strings.Cut(s, "#")
	if !ok {
		return IssueRef{}, ErrReference
	}
	local := repo == ""
	if local {
		repo = ctx.Repository
	} else {
		owner, name, ok := strings.Cut(repo, "/")
		if !ok || owner != "parametron-io" || !repositoryName(name) {
			return IssueRef{}, ErrReference
		}
		repo = strings.ToLower(name)
	}
	if number == "" {
		return IssueRef{}, ErrReference
	}
	for _, c := range number {
		if c < '0' || c > '9' {
			return IssueRef{}, ErrReference
		}
	}
	n, err := strconv.ParseInt(number, 10, 64)
	if err != nil || n <= 0 {
		return IssueRef{}, ErrReference
	}
	if number[0] == '0' || (!local && repo == ctx.Repository) {
		return IssueRef{}, ErrNoncanonical
	}
	return IssueRef{repo, n}, nil
}
func repositoryName(s string) bool {
	if s == "" || s == "." || s == ".." || len(s) > 100 {
		return false
	}
	for _, c := range s {
		if !(c >= 'a' && c <= 'z' || c >= 'A' && c <= 'Z' || c >= '0' && c <= '9' || c == '-' || c == '_' || c == '.') {
			return false
		}
	}
	return true
}

// Fences use runs of >=3 backticks or tildes, with 0..3 leading spaces.
// Backtick openers cannot contain backticks in their info suffix.
func fenceLine(line string) (byte, int, string) {
	spaces := 0
	for spaces < len(line) && line[spaces] == ' ' {
		spaces++
	}
	if spaces > 3 || spaces == len(line) {
		return 0, 0, ""
	}
	s := line[spaces:]
	marker := s[0]
	if marker != '`' && marker != '~' {
		return 0, 0, ""
	}
	count := 0
	for count < len(s) && s[count] == marker {
		count++
	}
	if count < 3 || (marker == '`' && strings.Contains(s[count:], "`")) {
		return 0, 0, ""
	}
	return marker, count, s[count:]
}
