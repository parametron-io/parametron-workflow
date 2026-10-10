package github

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"testing"
)

func mutationAdapter(t *testing.T, handler func(request) any) *Transport {
	t.Helper()
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var q request
		if err := json.NewDecoder(r.Body).Decode(&q); err != nil {
			t.Error(err)
		}
		if r.Header.Get("Authorization") != "Bearer secret-token" {
			t.Error("auth")
		}
		json.NewEncoder(w).Encode(map[string]any{"data": handler(q)})
	}))
	t.Cleanup(server.Close)
	c, err := NewTransport(server.URL, server.Client(), TokenFunc(func(context.Context) (string, error) { return "secret-token", nil }))
	if err != nil {
		t.Fatal(err)
	}
	return c
}

type mutationCase struct {
	name, query string
	input       map[string]any
	response    any
	call        func(*Transport) error
	invalid     func(*Transport) error
}

func mutationCases() []mutationCase {
	ctx := context.Background()
	return []mutationCase{
		{"type", `mutation($input:UpdateIssueInput!){result:updateIssue(input:$input){issue{id issueType{id}}}}`, map[string]any{"id": "I", "issueTypeId": "T"}, map[string]any{"result": map[string]any{"issue": map[string]any{"id": "I", "issueType": map[string]any{"id": "T"}}}}, func(c *Transport) error { return c.SetIssueType(ctx, "I", "T") }, func(c *Transport) error { return c.SetIssueType(ctx, "I", " ") }},
		{"add-labels", `mutation($input:AddLabelsToLabelableInput!){result:addLabelsToLabelable(input:$input){labelable{id}}}`, map[string]any{"labelableId": "I", "labelIds": []any{"L2", "L1"}}, map[string]any{"result": map[string]any{"labelable": map[string]any{"id": "I"}}}, func(c *Transport) error { return c.AddLabels(ctx, "I", []string{"L2", "L1"}) }, func(c *Transport) error { return c.AddLabels(ctx, "I", []string{"L1", ""}) }},
		{"remove-labels", `mutation($input:RemoveLabelsFromLabelableInput!){result:removeLabelsFromLabelable(input:$input){labelable{id}}}`, map[string]any{"labelableId": "I", "labelIds": []any{"L2", "L1"}}, map[string]any{"result": map[string]any{"labelable": map[string]any{"id": "I"}}}, func(c *Transport) error { return c.RemoveLabels(ctx, "I", []string{"L2", "L1"}) }, func(c *Transport) error { return c.RemoveLabels(ctx, "I", []string{"L1", "L1"}) }},
		{"add-item", `mutation($input:AddProjectV2ItemByIdInput!){result:addProjectV2ItemById(input:$input){item{id project{id} content{... on Issue{id} ... on PullRequest{id}}}}}`, map[string]any{"projectId": "P", "contentId": "I"}, map[string]any{"result": map[string]any{"item": map[string]any{"id": "ITEM", "project": map[string]any{"id": "P"}, "content": map[string]any{"id": "I"}}}}, func(c *Transport) error {
			id, err := c.AddProjectItem(ctx, "P", "I")
			if err == nil && id != "ITEM" {
				return fmt.Errorf("wrong ID")
			}
			return err
		}, func(c *Transport) error { _, err := c.AddProjectItem(ctx, "", "I"); return err }},
		{"remove-item", `mutation($input:DeleteProjectV2ItemInput!){result:deleteProjectV2Item(input:$input){deletedItemId}}`, map[string]any{"projectId": "P", "itemId": "ITEM"}, map[string]any{"result": map[string]any{"deletedItemId": "ITEM"}}, func(c *Transport) error { return c.RemoveProjectItem(ctx, "P", "ITEM") }, func(c *Transport) error { return c.RemoveProjectItem(ctx, "P", "item\n") }},
		{"option", `mutation($input:UpdateProjectV2ItemFieldValueInput!){result:updateProjectV2ItemFieldValue(input:$input){projectV2Item{id project{id}}}}`, map[string]any{"projectId": "P", "itemId": "ITEM", "fieldId": "F", "value": map[string]any{"singleSelectOptionId": "OPT"}}, map[string]any{"result": map[string]any{"projectV2Item": map[string]any{"id": "ITEM", "project": map[string]any{"id": "P"}}}}, func(c *Transport) error { return c.SetProjectOption(ctx, "P", "ITEM", "F", "OPT") }, func(c *Transport) error { return c.SetProjectOption(ctx, "P", "ITEM", "F", "bad id") }},
	}
}
func TestMutationConstructionAndResponses(t *testing.T) {
	for _, tc := range mutationCases() {
		t.Run(tc.name, func(t *testing.T) {
			calls := 0
			c := mutationAdapter(t, func(q request) any {
				calls++
				if q.Query != tc.query || !reflect.DeepEqual(q.Variables, map[string]any{"input": tc.input}) {
					t.Errorf("%s %+v", q.Query, q.Variables)
				}
				return tc.response
			})
			if err := tc.call(c); err != nil {
				t.Fatal(err)
			}
			category(t, tc.invalid(c), Permanent)
			if calls != 1 {
				t.Fatal("invalid input sent or retries", calls)
			}
			for _, response := range []any{map[string]any{}, map[string]any{"result": nil}, map[string]any{"result": map[string]any{}}, map[string]any{"result": map[string]any{"issue": map[string]any{"id": "WRONG", "issueType": map[string]any{"id": "T"}}, "labelable": map[string]any{"id": "WRONG"}, "item": map[string]any{"id": "ITEM", "project": map[string]any{"id": "WRONG"}, "content": map[string]any{"id": "I"}}, "deletedItemId": "WRONG", "projectV2Item": map[string]any{"id": "WRONG", "project": map[string]any{"id": "P"}}}}} {
				bad := mutationAdapter(t, func(request) any { return response })
				category(t, tc.call(bad), Malformed)
			}
		})
	}
}
func TestMutationErrorsNoRetryAndNoSecrets(t *testing.T) {
	for _, tc := range mutationCases() {
		for _, failure := range []struct {
			status   int
			body     string
			category Category
		}{{403, `secret-token`, Forbidden}, {429, `secret-token`, RateLimited}, {503, `secret-token`, Transient}, {200, `{"errors":[{"type":"INTERNAL","message":"secret-token"}]}`, Transient}} {
			t.Run(tc.name+fmt.Sprint(failure.status), func(t *testing.T) {
				calls := 0
				s := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
					calls++
					w.WriteHeader(failure.status)
					fmt.Fprint(w, failure.body)
				}))
				defer s.Close()
				c, _ := NewTransport(s.URL, s.Client(), TokenFunc(func(context.Context) (string, error) { return "secret-token", nil }))
				err := tc.call(c)
				category(t, err, failure.category)
				if calls != 1 || strings.Contains(err.Error(), "secret-token") {
					t.Fatal("retry or secret", err, calls)
				}
			})
		}
	}
}
func TestExactLabelResolutionWithoutCreation(t *testing.T) {
	calls := []string{}
	c := mutationAdapter(t, func(q request) any {
		if strings.Contains(q.Query, "mutation") {
			t.Error("label creation")
		}
		if q.Query != `query($owner:String!,$name:String!,$label:String!){repository(owner:$owner,name:$name){id label(name:$label){id name}}}` {
			t.Error(q.Query)
		}
		name := q.Variables["label"].(string)
		calls = append(calls, name)
		if name == "missing" {
			return map[string]any{"repository": map[string]any{"id": "R", "label": nil}}
		}
		return map[string]any{"repository": map[string]any{"id": "R", "label": map[string]any{"id": "L" + name, "name": name}}}
	})
	repo := Repository{ID: "R", Owner: "org", Name: "repo"}
	ids, err := c.ResolveLabels(context.Background(), repo, []string{"workflow", "docs"})
	if err != nil || !reflect.DeepEqual(ids, []string{"Lworkflow", "Ldocs"}) || !reflect.DeepEqual(calls, []string{"workflow", "docs"}) {
		t.Fatal(ids, err, calls)
	}
	_, err = c.ResolveLabels(context.Background(), repo, []string{"missing"})
	category(t, err, Permanent)
	bad := mutationAdapter(t, func(request) any {
		return map[string]any{"repository": map[string]any{"id": "R", "label": map[string]any{"id": "L", "name": "WORKFLOW"}}}
	})
	_, err = bad.ResolveLabels(context.Background(), repo, []string{"workflow"})
	category(t, err, Malformed)
}
func TestUnconfiguredMutationFake(t *testing.T) {
	f := MutationFake{}
	ctx := context.Background()
	for _, err := range []error{f.SetIssueType(ctx, "I", "T"), f.AddLabels(ctx, "I", []string{"L"}), f.RemoveLabels(ctx, "I", []string{"L"}), f.RemoveProjectItem(ctx, "P", "I"), f.SetProjectOption(ctx, "P", "I", "F", "O")} {
		if err == nil {
			t.Fatal("unconfigured success")
		}
	}
	if _, err := f.AddProjectItem(ctx, "P", "I"); err == nil {
		t.Fatal("unconfigured success")
	}
	if _, err := f.ResolveLabels(ctx, Repository{}, nil); err == nil {
		t.Fatal("unconfigured success")
	}
}
