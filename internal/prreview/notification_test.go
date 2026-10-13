package prreview

import (
	"github.com/parametron-io/parametron-workflow/internal/storage"
	"testing"
)

func TestNotificationSelectorsAndUnsupportedActions(t *testing.T) {
	for _, tc := range []struct {
		name, event, payload string
		ok, bad              bool
	}{
		{"ordinary_missing_action", "pull_request", `{}`, false, false},
		{"opened", "pull_request", `{"action":"opened","pull_request":{"draft":false}}`, false, false},
		{"draft_opened", "pull_request", `{"action":"opened","pull_request":{"draft":true}}`, false, false},
		{"edited_review", "pull_request_review", `{"action":"edited"}`, false, false},
		{"dismissed_review", "pull_request_review", `{"action":"dismissed"}`, false, false},
		{"ready_missing_native_selector", "pull_request", `{"action":"ready_for_review","pull_request":{"node_id":"PR","number":49,"updated_at":"2026-10-13T10:01:00Z"},"sender":{"node_id":"U","login":"dev","type":"User"}}`, true, false},
		{"duplicate_action", "pull_request", `{"action":"opened","action":"ready_for_review"}`, false, true},
		{"null_pr", "pull_request", `{"action":"ready_for_review","pull_request":null}`, true, true},
		{"malformed_time", "pull_request", `{"action":"ready_for_review","pull_request":{"node_id":"PR","number":49,"updated_at":"yesterday"},"sender":{"node_id":"U","login":"dev","type":"User"}}`, true, true},
		{"pending", "pull_request_review", `{"action":"submitted","pull_request":{"node_id":"PR","number":49},"review":{"node_id":"REV","state":"pending"}}`, false, false},
		{"unknown_state", "pull_request_review", `{"action":"submitted","pull_request":{"node_id":"PR","number":49},"review":{"node_id":"REV","state":"unknown"}}`, false, false},
		{"missing_actor", "pull_request_review", `{"action":"submitted","pull_request":{"node_id":"PR","number":49},"review":{"node_id":"REV","state":"changes_requested","submitted_at":"2026-10-13T10:02:00Z"}}`, true, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			c, ok, err := Notification(storage.Delivery{EventName: tc.event, Payload: []byte(tc.payload)})
			if ok != tc.ok || (err != nil) != tc.bad {
				t.Fatal(c, ok, err)
			}
			if tc.name == "ready_missing_native_selector" {
				i := policyInput(t)
				i.Candidate = c
				i.Facts.Transitions = nil
				d, err := Evaluate(i)
				if err == nil {
					t.Fatal("updated_at granted authority")
				}
				assertNoEffect(t, d)
			}
		})
	}
}
