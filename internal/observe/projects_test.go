package observe

import (
	"errors"
	"math"
	"reflect"
	"testing"

	"github.com/parametron-io/parametron-workflow/internal/config"
	"github.com/parametron-io/parametron-workflow/internal/github"
)

func TestProjectNormalization(t *testing.T) {
	for _, kind := range []string{"issue", "pull_request"} {
		for _, profile := range []config.Profile{config.Engineering, config.BugTracker} {
			t.Run(kind+"/"+string(profile), func(t *testing.T) {
				b := deployment().Engineering
				if profile == config.BugTracker {
					b = deployment().BugTracker
				}
				typ := "Issue"
				if kind == "pull_request" {
					typ = "PullRequest"
				}
				item := func(id string) github.ProjectItem {
					return github.ProjectItem{ID: id, ProjectID: b.ID, Content: &github.Content{ID: "node", Kind: typ}, Values: map[string]github.FieldValue{}}
				}
				a, z := item("a"), item("z")
				z.Archived = true
				z.Values[b.Fields[config.Estimate]] = github.FieldValue{Kind: config.Number, Number: 0}
				z.Values[b.Fields[config.Priority]] = github.FieldValue{Kind: config.SingleSelect, OptionID: "unmapped"}
				z.Values[b.Fields[config.StartDate]] = github.FieldValue{Kind: config.Date, Date: "2026-10-09"}
				if profile == config.BugTracker {
					z.Values[b.Fields[config.PriorityScore]] = github.FieldValue{Kind: config.Number, Number: 42}
				}
				first, err := normalizeProject(profile, b, "node", kind, []github.ProjectItem{z, a})
				if err != nil {
					t.Fatal(err)
				}
				second, err := normalizeProject(profile, b, "node", kind, []github.ProjectItem{a, z})
				if err != nil {
					t.Fatal(err)
				}
				if !reflect.DeepEqual(first, second) {
					t.Fatal("item ordering unstable")
				}
				if _, ok := first.Items[0].Fields[config.Estimate]; ok {
					t.Fatal("unset became zero")
				}
				if v, ok := first.Items[1].Fields[config.Estimate]; !ok || v.Number != 0 {
					t.Fatal("explicit zero lost")
				}
				if profile == config.BugTracker && first.Items[1].Fields[config.PriorityScore].Number != 42 {
					t.Fatal("score lost")
				}
				empty, err := normalizeProject(profile, b, "node", kind, nil)
				if err != nil || empty.Items == nil || len(empty.Items) != 0 {
					t.Fatal("absence not explicit")
				}
				cases := []struct {
					name  string
					alter func(*github.ProjectItem)
				}{
					{"project", func(i *github.ProjectItem) { i.ProjectID = "other" }},
					{"content", func(i *github.ProjectItem) { i.Content.ID = "other" }},
					{"kind", func(i *github.ProjectItem) { i.Content.Kind = "DraftIssue" }},
					{"nil content", func(i *github.ProjectItem) { i.Content = nil }},
					{"blank item", func(i *github.ProjectItem) { i.ID = "" }},
					{"wrong field kind", func(i *github.ProjectItem) {
						i.Values[b.Fields[config.Estimate]] = github.FieldValue{Kind: config.SingleSelect, OptionID: "option"}
					}},
					{"unknown field", func(i *github.ProjectItem) { i.Values["unconfigured"] = github.FieldValue{Kind: config.Number} }},
					{"empty option", func(i *github.ProjectItem) {
						i.Values[b.Fields[config.Status]] = github.FieldValue{Kind: config.SingleSelect}
					}},
					{"invalid date", func(i *github.ProjectItem) {
						i.Values[b.Fields[config.StartDate]] = github.FieldValue{Kind: config.Date, Date: "bad"}
					}},
					{"nan", func(i *github.ProjectItem) {
						i.Values[b.Fields[config.Estimate]] = github.FieldValue{Kind: config.Number, Number: math.NaN()}
					}},
					{"infinity", func(i *github.ProjectItem) {
						i.Values[b.Fields[config.Estimate]] = github.FieldValue{Kind: config.Number, Number: math.Inf(1)}
					}},
					{"contradictory value", func(i *github.ProjectItem) {
						i.Values[b.Fields[config.Estimate]] = github.FieldValue{Kind: config.Number, OptionID: "unexpected"}
					}},
				}
				for _, c := range cases {
					t.Run(c.name, func(t *testing.T) {
						i := item("bad")
						c.alter(&i)
						_, err := normalizeProject(profile, b, "node", kind, []github.ProjectItem{i})
						if !errors.Is(err, ErrObservation) {
							t.Fatal(err)
						}
					})
				}
				if _, err := normalizeProject(profile, b, "node", kind, []github.ProjectItem{a, a}); !errors.Is(err, ErrObservation) {
					t.Fatal("duplicate accepted")
				}
			})
		}
	}
}

func TestRoadmapNumberObservation(t *testing.T) {
	b := deployment().Engineering
	for _, tc := range []struct {
		name    string
		value   *github.FieldValue
		invalid bool
	}{
		{"unset", nil, false},
		{"zero", &github.FieldValue{Kind: config.Number, Number: 0}, false},
		{"roadmap value", &github.FieldValue{Kind: config.Number, Number: 10020}, false},
		{"non-number", &github.FieldValue{Kind: config.SingleSelect, OptionID: "option"}, true},
		{"NaN", &github.FieldValue{Kind: config.Number, Number: math.NaN()}, true},
		{"infinity", &github.FieldValue{Kind: config.Number, Number: math.Inf(1)}, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			item := github.ProjectItem{ID: "item", ProjectID: b.ID, Content: &github.Content{ID: "issue", Kind: "Issue"}, Values: map[string]github.FieldValue{}}
			if tc.value != nil {
				item.Values[b.Fields[config.RoadmapOrder]] = *tc.value
			}
			got, err := normalizeProject(config.Engineering, b, "issue", "issue", []github.ProjectItem{item})
			if tc.invalid {
				if !errors.Is(err, ErrObservation) {
					t.Fatal("invalid number accepted", got, err)
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			value, present := got.Items[0].Fields[config.RoadmapOrder]
			if present != (tc.value != nil) || present && (value.Kind != config.Number || value.Number != tc.value.Number) {
				t.Fatal("unset/zero/value distinction", value, present)
			}
		})
	}
}
