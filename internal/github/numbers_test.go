package github

import (
	"context"
	"encoding/json"
	"errors"
	"math"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"testing"
)

func numberTransport(t *testing.T, handler func(request) any) *Transport {
	t.Helper()
	s := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var q request
		if err := json.NewDecoder(r.Body).Decode(&q); err != nil {
			t.Error(err)
			return
		}
		if err := json.NewEncoder(w).Encode(map[string]any{"data": handler(q)}); err != nil {
			t.Error(err)
		}
	}))
	t.Cleanup(s.Close)
	c, err := NewTransport(s.URL, s.Client(), TokenFunc(func(context.Context) (string, error) { return "private-token", nil }))
	if err != nil {
		t.Fatal(err)
	}
	return c
}

func numberIdentity() map[string]any {
	return map[string]any{"item": map[string]any{"__typename": "ProjectV2Item", "id": "I", "project": map[string]any{"id": "P"}}, "project": map[string]any{"__typename": "ProjectV2", "id": "P"}}
}

func TestProjectNumberExactSetClearAndPagination(t *testing.T) {
	for _, clear := range []bool{false, true} {
		t.Run(map[bool]string{false: "set", true: "clear"}[clear], func(t *testing.T) {
			calls := []string{}
			value := math.Nextafter(123.25, math.Inf(1))
			c := numberTransport(t, func(q request) any {
				switch {
				case strings.Contains(q.Query, "item:node"):
					calls = append(calls, "identity")
					if !reflect.DeepEqual(q.Variables, map[string]any{"item": "I", "project": "P"}) {
						t.Error(q)
					}
					return numberIdentity()
				case strings.Contains(q.Query, "result:fields"):
					calls = append(calls, "field")
					if q.Variables["id"] != "P" {
						t.Error(q)
					}
					return connectionJSON("ProjectV2", []any{map[string]any{"__typename": "ProjectV2Field", "id": "F", "dataType": "NUMBER"}}, false, "")
				case strings.Contains(q.Query, "mutation"):
					calls = append(calls, "mutation")
					operation := "updateProjectV2ItemFieldValue"
					want := map[string]any{"projectId": "P", "itemId": "I", "fieldId": "F", "value": map[string]any{"number": value}}
					if clear {
						operation = "clearProjectV2ItemFieldValue"
						delete(want, "value")
					}
					if !strings.Contains(q.Query, operation+"(input:$input)") || strings.Contains(q.Query, "afterId") || !reflect.DeepEqual(q.Variables, map[string]any{"input": want}) {
						t.Error(q)
					}
					return map[string]any{"result": map[string]any{"projectV2Item": map[string]any{"id": "I", "project": map[string]any{"id": "P"}}}}
				case strings.Contains(q.Query, "result:fieldValues"):
					if q.Variables["id"] != "I" {
						t.Error(q)
					}
					if q.Variables["cursor"] == nil {
						calls = append(calls, "values page 1")
						return connectionJSON("ProjectV2Item", []any{map[string]any{"__typename": "ProjectV2ItemFieldNumberValue", "number": 0, "field": map[string]any{"id": "other"}}}, true, "next")
					}
					calls = append(calls, "values page 2")
					if q.Variables["cursor"] != "next" {
						t.Error(q)
					}
					values := []any{}
					if !clear {
						values = append(values, map[string]any{"__typename": "ProjectV2ItemFieldNumberValue", "number": value, "field": map[string]any{"id": "F"}})
					}
					return connectionJSON("ProjectV2Item", values, false, "")
				default:
					t.Error(q)
					return nil
				}
			})
			var err error
			if clear {
				err = c.ClearProjectNumber(context.Background(), "P", "I", "F")
			} else {
				err = c.SetProjectNumber(context.Background(), "P", "I", "F", value)
			}
			want := []string{"identity", "field", "mutation", "identity", "field", "values page 1", "values page 2"}
			if err != nil || !reflect.DeepEqual(calls, want) {
				t.Fatal(err, calls)
			}
		})
	}
}

func TestProjectNumberRejectsInvalidArgumentsBeforeIO(t *testing.T) {
	c := numberTransport(t, func(request) any { t.Error("unexpected network"); return nil })
	for _, ids := range [][3]string{{"", "I", "F"}, {"P", " I", "F"}, {"P", "I", "F\n"}} {
		category(t, c.SetProjectNumber(context.Background(), ids[0], ids[1], ids[2], 1), Permanent)
		category(t, c.ClearProjectNumber(context.Background(), ids[0], ids[1], ids[2]), Permanent)
	}
	for _, v := range []float64{math.NaN(), math.Inf(1), math.Inf(-1)} {
		category(t, c.SetProjectNumber(context.Background(), "P", "I", "F", v), Permanent)
	}
}

func TestProjectNumberRejectsIdentityAndFieldBeforeMutation(t *testing.T) {
	for _, bad := range []string{"wrong project", "wrong item", "missing field", "duplicate field", "wrong type", "missing project", "malformed identity"} {
		t.Run(bad, func(t *testing.T) {
			mutations := 0
			c := numberTransport(t, func(q request) any {
				if strings.Contains(q.Query, "mutation") {
					mutations++
					return nil
				}
				if strings.Contains(q.Query, "item:node") {
					d := numberIdentity()
					i := d["item"].(map[string]any)
					switch bad {
					case "wrong project":
						i["project"] = map[string]any{"id": "other"}
					case "wrong item":
						i["id"] = "other"
					case "missing project":
						d["project"] = nil
					case "malformed identity":
						i["__typename"] = "Issue"
					}
					return d
				}
				field := map[string]any{"__typename": "ProjectV2Field", "id": "F", "dataType": "NUMBER"}
				fields := []any{field}
				if bad == "missing field" {
					fields = nil
				}
				if bad == "duplicate field" {
					fields = append(fields, field)
				}
				if bad == "wrong type" {
					field["dataType"] = "TEXT"
				}
				return connectionJSON("ProjectV2", fields, false, "")
			})
			want := Malformed
			if bad == "missing project" {
				want = NotFound
			}
			category(t, c.SetProjectNumber(context.Background(), "P", "I", "F", 1), want)
			category(t, c.ClearProjectNumber(context.Background(), "P", "I", "F"), want)
			if mutations != 0 {
				t.Fatal("invalid target mutated", mutations)
			}
		})
	}
}

func TestProjectNumberPostWriteMustConvergeExactly(t *testing.T) {
	for _, bad := range []string{"wrong returned item", "wrong returned project", "missing result", "wrong current number", "epsilon drift", "duplicate current", "wrong typed value", "null number", "clear retains zero", "clear retains nonzero"} {
		t.Run(bad, func(t *testing.T) {
			mutations := 0
			c := numberTransport(t, func(q request) any {
				if strings.Contains(q.Query, "item:node") {
					return numberIdentity()
				}
				if strings.Contains(q.Query, "result:fields") {
					return connectionJSON("ProjectV2", []any{map[string]any{"id": "F", "dataType": "NUMBER"}}, false, "")
				}
				if strings.Contains(q.Query, "mutation") {
					mutations++
					i := map[string]any{"id": "I", "project": map[string]any{"id": "P"}}
					if bad == "wrong returned item" {
						i["id"] = "other"
					}
					if bad == "wrong returned project" {
						i["project"] = map[string]any{"id": "other"}
					}
					if bad == "missing result" {
						return map[string]any{"result": nil}
					}
					return map[string]any{"result": map[string]any{"projectV2Item": i}}
				}
				v := map[string]any{"__typename": "ProjectV2ItemFieldNumberValue", "number": 1.0, "field": map[string]any{"id": "F"}}
				values := []any{v}
				switch bad {
				case "wrong current number":
					v["number"] = 2
				case "epsilon drift":
					v["number"] = math.Nextafter(1, 2)
				case "duplicate current":
					values = append(values, v)
				case "wrong typed value":
					v["__typename"] = "ProjectV2ItemFieldTextValue"
				case "null number":
					v["number"] = nil
				case "clear retains zero":
					v["number"] = 0
				}
				return connectionJSON("ProjectV2Item", values, false, "")
			})
			var err error
			if strings.HasPrefix(bad, "clear") {
				err = c.ClearProjectNumber(context.Background(), "P", "I", "F")
			} else {
				err = c.SetProjectNumber(context.Background(), "P", "I", "F", 1)
			}
			category(t, err, Malformed)
			if mutations != 1 {
				t.Fatal("hidden retry", mutations)
			}
		})
	}
}

func TestProjectNumberTransportErrorsAndCancellation(t *testing.T) {
	for _, status := range []int{404, 409, 429, 500} {
		calls := 0
		s := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			calls++
			w.WriteHeader(status)
			w.Write([]byte("private-body private-token"))
		}))
		c, err := NewTransport(s.URL, s.Client(), TokenFunc(func(context.Context) (string, error) { return "private-token", nil }))
		if err != nil {
			t.Fatal(err)
		}
		for _, clear := range []bool{false, true} {
			if clear {
				err = c.ClearProjectNumber(context.Background(), "P", "I", "F")
			} else {
				err = c.SetProjectNumber(context.Background(), "P", "I", "F", 1)
			}
			category(t, err, map[int]Category{404: NotFound, 409: Conflict, 429: RateLimited, 500: Transient}[status])
			if strings.Contains(err.Error(), "private") {
				t.Fatal(err)
			}
		}
		s.Close()
		if calls != 2 {
			t.Fatal(calls)
		}
	}
	c := numberTransport(t, func(request) any { t.Error("canceled request performed IO"); return nil })
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if err := c.SetProjectNumber(ctx, "P", "I", "F", 1); !errors.Is(err, context.Canceled) {
		t.Fatal(err)
	}
	if err := c.ClearProjectNumber(ctx, "P", "I", "F"); !errors.Is(err, context.Canceled) {
		t.Fatal(err)
	}
}
