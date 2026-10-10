package intent

import (
	"errors"
	"reflect"
	"strings"
	"testing"
)

var testContext = Context{"parametron-io", "parametron-workflow"}

func parse(t *testing.T, body string) Intent {
	t.Helper()
	got, err := Parse(body, testContext)
	if err != nil {
		t.Fatal(err)
	}
	return got
}
func TestDefaults(t *testing.T) {
	got := parse(t, "")
	if !reflect.DeepEqual(got, Intent{}) {
		t.Fatalf("explicit defaults: %+v", got)
	}
	want := Effective{Automation: true, Classification: true, SetStatus: true, SetPosition: true, AutomaticEstimate: true}
	if got.Effective() != want {
		t.Fatalf("effective: %+v", got.Effective())
	}
	if parse(t, "Type: Task\nA Feature or Bug").CreateBranch.Explicit {
		t.Fatal("inferred type-sensitive default")
	}
}
func boolean(i Intent, name string) Boolean {
	switch name {
	case "Automation":
		return i.Explicit.Automation
	case "Classification":
		return i.Explicit.Classification
	case "Triage":
		return i.Explicit.Triage
	case "Audit":
		return i.Explicit.Audit
	case "Validation":
		return i.Explicit.Validation
	case "Review":
		return i.Explicit.Review
	case "Set-Status":
		return i.Explicit.SetStatus
	case "Set-Position":
		return i.Explicit.SetPosition
	default:
		return i.CreateBranch
	}
}
func TestBooleans(t *testing.T) {
	for _, name := range []string{"Automation", "Classification", "Triage", "Audit", "Validation", "Review", "Set-Status", "Set-Position", "Create-Branch"} {
		t.Run(name, func(t *testing.T) {
			for _, value := range []string{"true", "false"} {
				body := name + ": " + value
				got := parse(t, body+"\n"+body)
				if boolean(got, name) != (Boolean{value == "true", true}) {
					t.Fatalf("value: %+v", got)
				}
			}
			if boolean(parse(t, strings.ToLower(name)+": true"), name).Explicit {
				t.Fatal("wrong name case accepted")
			}
			for _, value := range []string{"", "TRUE", "False", "yes", "no", "on", "off", "1", "0", "true: false", "true junk"} {
				_, err := Parse(name+": "+value, testContext)
				if !errors.Is(err, ErrDirective) {
					t.Fatalf("%q: %v", value, err)
				}
			}
			_, err := Parse(name+": true\n"+name+": false", testContext)
			if !errors.Is(err, ErrConflict) {
				t.Fatalf("conflict: %v", err)
			}
		})
	}
}
func TestAutomation(t *testing.T) {
	got := parse(t, "Automation: false\nClassification: true\nTriage: true\nAudit: true\nValidation: true\nReview: true\nSet-Status: true\nSet-Position: false\nCreate-Branch: true\nParent: #1\nBlocks: #2")
	e := got.Effective()
	if e.Automation || e.Classification || e.Triage || e.Audit || e.Validation || e.Review || e.AutomaticEstimate {
		t.Fatalf("suppression: %+v", e)
	}
	for _, name := range []string{"Classification", "Triage", "Audit", "Validation", "Review"} {
		if boolean(got, name) != (Boolean{true, true}) {
			t.Fatalf("lost %s", name)
		}
	}
	if !e.SetStatus || e.SetPosition || got.CreateBranch != (Boolean{true, true}) || got.Parent == nil || len(got.Blocks) != 1 {
		t.Fatalf("deterministic intent affected: %+v", got)
	}
	if parse(t, "Automation: false").Effective().Classification {
		t.Fatal("default classification escaped suppression")
	}
}

func TestEffectiveExplicitValues(t *testing.T) {
	for _, enabled := range []bool{true, false} {
		value := "false"
		if enabled {
			value = "true"
		}
		got := parse(t, "Classification: "+value+"\nTriage: "+value+"\nAudit: "+value+"\nValidation: "+value+"\nReview: "+value+"\nSet-Status: "+value+"\nSet-Position: "+value).Effective()
		want := Effective{
			Automation: true, AutomaticEstimate: true,
			Classification: enabled, Triage: enabled, Audit: enabled,
			Validation: enabled, Review: enabled, SetStatus: enabled, SetPosition: enabled,
		}
		if got != want {
			t.Fatalf("enabled=%v: %+v", enabled, got)
		}
	}
}
func TestReferences(t *testing.T) {
	for _, tc := range []struct {
		input, repo string
		n           int64
	}{
		{"#1", "parametron-workflow", 1}, {"#9223372036854775807", "parametron-workflow", 9223372036854775807},
		{"parametron-io/parametron-engine#41", "parametron-engine", 41},
		{"parametron-io/Engine.Test_1#2", "engine.test_1", 2},
	} {
		t.Run(tc.input, func(t *testing.T) {
			got := parse(t, "Parent: "+tc.input)
			if *got.Parent != (IssueRef{tc.repo, tc.n}) {
				t.Fatalf("%+v", got.Parent)
			}
		})
	}
	for _, s := range []string{"0", "1", "#0", "#-1", "#+1", "#1.0", "#", "##1", "#1junk", "#9223372036854775808", "owner/repo#1", "https://github.com/parametron-io/repo/issues/1", "parametron-io/repo1", "parametron-io/#1", "parametron-io/a/b#1", "parametron-io/a b#1", "parametron-io/..#1", "parametron-io/repo!#1", "(#1)", "#1\u00a0#2"} {
		t.Run(s, func(t *testing.T) {
			_, err := Parse("Parent: "+s, testContext)
			if !errors.Is(err, ErrReference) {
				t.Fatalf("%q: %v", s, err)
			}
		})
	}
	for _, s := range []string{"#01", "parametron-io/parametron-workflow#1", "parametron-io/Parametron-Workflow#1"} {
		_, err := Parse("Refs: "+s, testContext)
		if !errors.Is(err, ErrNoncanonical) {
			t.Fatalf("%q: %v", s, err)
		}
	}
}
func TestSingular(t *testing.T) {
	for _, name := range []string{"Parent", "Target"} {
		t.Run(name, func(t *testing.T) {
			got := parse(t, name+": #10\n"+name+": #10")
			ref := got.Parent
			if name == "Target" {
				ref = got.Target
			}
			if ref == nil || *ref != (IssueRef{"parametron-workflow", 10}) {
				t.Fatalf("%+v", got)
			}
			for _, body := range []string{name + ": #10\n" + name + ": #11", name + ": #10 #11"} {
				_, err := Parse(body, testContext)
				if !errors.Is(err, ErrConflict) {
					t.Fatalf("%v", err)
				}
			}
		})
	}
}
func TestMultiReferences(t *testing.T) {
	for _, name := range []string{"Blocked-By", "Blocks", "Refs"} {
		t.Run(name, func(t *testing.T) {
			got := parse(t, name+": #9 #9\t#10 parametron-io/Engine#41\n"+name+": #10 #11 parametron-io/engine#41\n"+name+": #9")
			refs := got.BlockedBy
			if name == "Blocks" {
				refs = got.Blocks
			}
			if name == "Refs" {
				refs = got.Refs
			}
			want := []IssueRef{{"parametron-workflow", 9}, {"parametron-workflow", 10}, {"engine", 41}, {"parametron-workflow", 11}}
			if !reflect.DeepEqual(refs, want) {
				t.Fatalf("%+v", refs)
			}
			single := parse(t, name+": #1")
			if single.Parent != nil || single.Target != nil {
				t.Fatal("wrong field")
			}
			for _, v := range []string{"#9,#10", "#9, #10", "#9;#10", "#9; #10", "#1 junk"} {
				_, err := Parse(name+": "+v, testContext)
				if !errors.Is(err, ErrReference) {
					t.Fatalf("%q: %v", v, err)
				}
			}
		})
	}
}
func TestMissingRelationships(t *testing.T) {
	for _, name := range []string{"Parent", "Target", "Blocked-By", "Blocks", "Refs"} {
		_, err := Parse(name+": \t", testContext)
		if !errors.Is(err, ErrDirective) {
			t.Fatalf("%s: %v", name, err)
		}
	}
}
func TestRecognition(t *testing.T) {
	for _, body := range []string{
		"Set Automation: false when needed", "The Parent: field is useful", "Unknown: true", "Automations: false", "Automation : false", "Reviewing: true", "automation: false",
		" Automation: false", "    Automation: false", "\tAutomation: false", "> Automation: false", "- Automation: false", "`Automation: false`", "\"Automation: false\"",
		"```text\nAutomation: false\n```", "~~~\nParent: invalid\n~~~", "   ```\nAudit: invalid\n   ```",
		"````\n```\nAutomation: false\n````", "```\nAutomation: false", "~~~\n```\nAutomation: false\n~~~",
	} {
		t.Run(body, func(t *testing.T) {
			if got := parse(t, body); !reflect.DeepEqual(got, Intent{}) {
				t.Fatalf("false positive: %+v", got)
			}
		})
	}
	got := parse(t, "```\nAutomation: false\n```\nReview:\ttrue \t\r\nParent:#1\r\n")
	if !got.Explicit.Review.Value || got.Explicit.Automation.Explicit || got.Parent == nil {
		t.Fatalf("%+v", got)
	}
}
func TestErrors(t *testing.T) {
	got, err := Parse("Parent: #1\nAudit: secret junk", testContext)
	var detail *Error
	if !errors.As(err, &detail) || detail.Line != 2 || detail.Directive != "Audit" || !errors.Is(err, ErrDirective) {
		t.Fatalf("%v", err)
	}
	if !reflect.DeepEqual(got, Intent{}) || strings.Contains(err.Error(), "secret") {
		t.Fatal("partial result or input leaked")
	}
	for _, ctx := range []Context{{}, {"other", "repo"}, {"parametron-io", ""}, {"parametron-io", "a/b"}} {
		_, err := Parse("", ctx)
		if !errors.Is(err, ErrContext) {
			t.Fatalf("%+v: %v", ctx, err)
		}
	}
}
func TestDeterminismAndOwnership(t *testing.T) {
	body := "Parent: #1\nRefs: #3 #2 #3\nBlocks: #4\nAudit: true"
	want := parse(t, body)
	for n := 0; n < 100; n++ {
		if got := parse(t, body); !reflect.DeepEqual(got, want) {
			t.Fatalf("%+v", got)
		}
	}
	mutated := parse(t, body)
	mutated.Refs[0].Number = 99
	mutated.Parent.Number = 99
	mutated.Explicit.Audit.Value = false
	if !reflect.DeepEqual(parse(t, body), want) {
		t.Fatal("shared parse state")
	}
	empty := parse(t, "")
	empty.Explicit.Automation = Boolean{false, true}
	if !parse(t, "").Effective().Automation {
		t.Fatal("shared defaults")
	}
}
