package webhook

import (
	"bytes"
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/parametron-io/parametron-workflow/internal/storage"
)

var secret = []byte("test-private-secret")
var acceptedAt = time.Date(2026, 10, 9, 12, 0, 0, 123, time.FixedZone("test", 3600))

type storeFunc func(context.Context, storage.Delivery) (bool, error)

func (f storeFunc) InsertDelivery(ctx context.Context, d storage.Delivery) (bool, error) {
	return f(ctx, d)
}
func sign(body []byte) string {
	mac := hmac.New(sha256.New, secret)
	mac.Write(body)
	return "sha256=" + hex.EncodeToString(mac.Sum(nil))
}
func request(body []byte) *http.Request {
	r := httptest.NewRequest(http.MethodPost, "/", bytes.NewReader(body))
	r.Header.Set("Content-Type", "application/json")
	r.Header.Set("X-GitHub-Delivery", "abc-123")
	r.Header.Set("X-GitHub-Event", "issues")
	r.Header.Set("X-Hub-Signature-256", sign(body))
	return r
}
func handler(t *testing.T, store DeliveryStore, logs io.Writer) *Handler {
	t.Helper()
	h, err := New(Config{Store: store, Secret: secret, Clock: func() time.Time { return acceptedAt }, Logger: slog.New(slog.NewJSONHandler(logs, nil))})
	if err != nil {
		t.Fatal(err)
	}
	return h
}
func TestValidIngress(t *testing.T) {
	body := []byte(" {\n \"private_payload\": true }\n")
	w := &trackedWriter{ResponseRecorder: httptest.NewRecorder()}
	var logs bytes.Buffer
	called := false
	h := handler(t, storeFunc(func(ctx context.Context, d storage.Delivery) (bool, error) {
		called = true
		if w.wrote {
			t.Fatal("acknowledged before insertion")
		}
		if d.ID != "abc-123" || d.EventName != "issues" || !bytes.Equal(d.Payload, body) || d.Resource != nil || d.ReceivedAt != acceptedAt.UTC() {
			t.Fatalf("unexpected delivery: %+v", d)
		}
		return true, nil
	}), &logs)
	h.ServeHTTP(w, request(body))
	if !called || w.Code != 204 || w.Body.Len() != 0 {
		t.Fatalf("response: %d %s", w.Code, w.Body.String())
	}
	for _, forbidden := range []string{string(secret), sign(body), "private_payload", "abc-123", "issues", "delivery_id", "event_name"} {
		if strings.Contains(logs.String(), forbidden) {
			t.Fatal("sensitive log")
		}
	}
	if !strings.Contains(logs.String(), `"outcome":"accepted"`) {
		t.Fatal("missing outcome")
	}
}

func TestRejections(t *testing.T) {
	body := []byte(`{"ok":true}`)
	tests := []struct {
		name   string
		change func(*http.Request)
		status int
	}{
		{"method", func(r *http.Request) { r.Method = "GET" }, 405},
		{"method does not read body", func(r *http.Request) { r.Method = "PUT"; r.Body = io.NopCloser(unreadableBody{t}) }, 405},
		{"missing content type", func(r *http.Request) { r.Header.Del("Content-Type") }, 415},
		{"unsupported content type", func(r *http.Request) { r.Header.Set("Content-Type", "text/plain") }, 415},
		{"malformed content type", func(r *http.Request) { r.Header.Set("Content-Type", "application/json; charset") }, 415},
		{"multiple content type", func(r *http.Request) { r.Header.Add("Content-Type", "application/json") }, 415},
		{"missing signature", func(r *http.Request) { r.Header.Del("X-Hub-Signature-256") }, 401},
		{"empty signature", func(r *http.Request) { r.Header.Set("X-Hub-Signature-256", "") }, 401},
		{"wrong signature", func(r *http.Request) { r.Header.Set("X-Hub-Signature-256", "sha256="+strings.Repeat("0", 64)) }, 401},
		{"malformed hex", func(r *http.Request) { r.Header.Set("X-Hub-Signature-256", "sha256="+strings.Repeat("z", 64)) }, 401},
		{"wrong algorithm", func(r *http.Request) { r.Header.Set("X-Hub-Signature-256", "sha1="+strings.Repeat("0", 40)) }, 401},
		{"multiple signature", func(r *http.Request) { r.Header.Add("X-Hub-Signature-256", sign(body)) }, 401},
		{"changed body", func(r *http.Request) { r.Body = io.NopCloser(strings.NewReader(`{"ok":false}`)) }, 401},
		{"oversized length", func(r *http.Request) { r.ContentLength = MaxBodyBytes + 1 }, 413},
		{"oversized stream", func(r *http.Request) {
			r.ContentLength = -1
			r.TransferEncoding = []string{"chunked"}
			r.Body = io.NopCloser(io.LimitReader(repeatingReader{}, MaxBodyBytes+1))
		}, 413},
		{"read error", func(r *http.Request) { r.Body = io.NopCloser(errorReader{}) }, 400},
	}
	for _, header := range []string{"X-GitHub-Delivery", "X-GitHub-Event"} {
		for _, value := range []string{"", " ", "a,b", "bad\nvalue", "a b"} {
			tests = append(tests, struct {
				name   string
				change func(*http.Request)
				status int
			}{header + fmt.Sprintf(" %q", value), func(r *http.Request) { r.Header.Set(header, value) }, 400})
		}
		tests = append(tests, struct {
			name   string
			change func(*http.Request)
			status int
		}{header + " missing", func(r *http.Request) { r.Header.Del(header) }, 400})
		tests = append(tests, struct {
			name   string
			change func(*http.Request)
			status int
		}{header + " multiple", func(r *http.Request) { r.Header.Add(header, "other") }, 400})
		tests = append(tests, struct {
			name   string
			change func(*http.Request)
			status int
		}{header + " repeated identical", func(r *http.Request) { r.Header.Add(header, r.Header.Get(header)) }, 400})
	}
	for _, invalid := range []string{"", "{", "[]", "null", "true", `"string"`, "123", "{}{}"} {
		tests = append(tests, struct {
			name   string
			change func(*http.Request)
			status int
		}{"JSON " + invalid, func(r *http.Request) { *r = *request([]byte(invalid)) }, 400})
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			h := handler(t, storeFunc(func(context.Context, storage.Delivery) (bool, error) {
				t.Fatal("rejected request reached storage")
				return false, nil
			}), io.Discard)
			r := request(body)
			tt.change(r)
			w := httptest.NewRecorder()
			h.ServeHTTP(w, r)
			if w.Code != tt.status || w.Body.Len() != 0 {
				t.Fatalf("status %d want %d", w.Code, tt.status)
			}
			if tt.status == 405 && w.Header().Get("Allow") != "POST" {
				t.Fatal("missing Allow")
			}
		})
	}
}

type repeatingReader struct{}

func (repeatingReader) Read(p []byte) (int, error) {
	for i := range p {
		p[i] = ' '
	}
	return len(p), nil
}

type errorReader struct{}

func (errorReader) Read([]byte) (int, error) { return 0, errors.New("private read error") }

type unreadableBody struct{ t *testing.T }

func (r unreadableBody) Read([]byte) (int, error) {
	r.t.Fatal("method rejection read body")
	return 0, io.EOF
}

func TestConstruction(t *testing.T) {
	var typedNil *storage.Store
	for _, cfg := range []Config{{Store: storeFunc(func(context.Context, storage.Delivery) (bool, error) { return true, nil })}, {Secret: secret}, {Store: typedNil, Secret: secret}} {
		if _, err := New(cfg); err == nil {
			t.Fatal("invalid configuration accepted")
		}
	}
	copied := bytes.Clone(secret)
	h, err := New(Config{Store: storeFunc(func(context.Context, storage.Delivery) (bool, error) { return true, nil }), Secret: copied})
	if err != nil {
		t.Fatal(err)
	}
	copied[0] = 'x'
	if !bytes.Equal(h.secret, secret) {
		t.Fatal("secret not owned by handler")
	}
}

func TestStorageResponses(t *testing.T) {
	for _, tt := range []struct {
		name     string
		inserted bool
		err      error
		status   int
		outcome  string
	}{
		{"duplicate", false, nil, 204, "duplicate"},
		{"conflict", false, fmt.Errorf("private detail: %w", storage.ErrConflict), 409, "conflict"},
		{"failure", false, errors.New("private storage error"), 500, "persistence_failure"},
	} {
		t.Run(tt.name, func(t *testing.T) {
			var logs bytes.Buffer
			h := handler(t, storeFunc(func(context.Context, storage.Delivery) (bool, error) { return tt.inserted, tt.err }), &logs)
			w := httptest.NewRecorder()
			h.ServeHTTP(w, request([]byte(`{}`)))
			if w.Code != tt.status || w.Body.Len() != 0 || strings.Contains(logs.String(), "private") || !strings.Contains(logs.String(), tt.outcome) {
				t.Fatalf("response/log %d %s", w.Code, logs.String())
			}
		})
	}
}

func TestDurableAcceptance(t *testing.T) {
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "events.db")
	store, err := storage.Open(ctx, path)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { store.Close() }()
	h := handler(t, store, io.Discard)
	body := []byte(" { \"evidence\": 1 }\n")
	r := request(body)
	r.Header.Set("Content-Type", "application/json; charset=utf-8")
	w := httptest.NewRecorder()
	h.ServeHTTP(w, r)
	if w.Code != 204 {
		t.Fatal(w.Code)
	}
	first, err := store.Event(ctx, "abc-123")
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(first.Delivery.Payload, body) || first.Delivery.Resource != nil || first.Resource != nil || first.Delivery.ReceivedAt != acceptedAt.UTC() || first.State.Status != storage.Pending {
		t.Fatalf("unexpected event %+v", first)
	}
	next := acceptedAt.UTC().Add(time.Hour)
	state := storage.ProcessState{Status: storage.Retryable, Attempts: 3, NextAttemptAt: &next, ErrorCategory: "transient"}
	if err = store.UpdateState(ctx, "abc-123", state); err != nil {
		t.Fatal(err)
	}
	h.clock = func() time.Time { return next }
	w = httptest.NewRecorder()
	h.ServeHTTP(w, request(body))
	if w.Code != 204 {
		t.Fatal(w.Code)
	}
	events, err := store.Work(ctx)
	if err != nil || len(events) != 1 {
		t.Fatalf("work %v %v", events, err)
	}
	if !reflect.DeepEqual(events[0].State, state) || events[0].Delivery.ReceivedAt != first.Delivery.ReceivedAt {
		t.Fatal("duplicate changed durable state")
	}
	changedEvent := request(body)
	changedEvent.Header.Set("X-GitHub-Event", "ping")
	for _, r := range []*http.Request{request([]byte(`{"evidence":2}`)), changedEvent} {
		w = httptest.NewRecorder()
		h.ServeHTTP(w, r)
		if w.Code != 409 {
			t.Fatal(w.Code)
		}
	}
	if err = store.Close(); err != nil {
		t.Fatal(err)
	}
	store, err = storage.Open(ctx, path)
	if err != nil {
		t.Fatal(err)
	}
	persisted, err := store.Event(ctx, "abc-123")
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(persisted, events[0]) {
		t.Fatal("restart changed evidence")
	}
}

type trackedWriter struct {
	*httptest.ResponseRecorder
	wrote bool
}

func (w *trackedWriter) WriteHeader(status int) {
	w.wrote = true
	w.ResponseRecorder.WriteHeader(status)
}

func (w *trackedWriter) Write(p []byte) (int, error) {
	w.wrote = true
	return w.ResponseRecorder.Write(p)
}

func TestBodyLimitInclusive(t *testing.T) {
	body := make([]byte, MaxBodyBytes)
	for i := range body {
		body[i] = ' '
	}
	copy(body, "{}")
	called := false
	h := handler(t, storeFunc(func(_ context.Context, d storage.Delivery) (bool, error) {
		called = true
		if !bytes.Equal(d.Payload, body) {
			t.Fatal("payload changed")
		}
		return true, nil
	}), io.Discard)
	r := request(body)
	r.ContentLength = -1
	w := httptest.NewRecorder()
	h.ServeHTTP(w, r)
	if !called || w.Code != 204 {
		t.Fatalf("limit rejected: %d", w.Code)
	}
}

func TestRejectedAuthenticationLogs(t *testing.T) {
	body := []byte(`{"private_payload":true}`)
	var logs bytes.Buffer
	h := handler(t, storeFunc(func(context.Context, storage.Delivery) (bool, error) { t.Fatal("storage called"); return false, nil }), &logs)
	r := request(body)
	r.Header["x-hub-signature-256"] = []string{"sha256=" + strings.Repeat("0", 64)}
	w := httptest.NewRecorder()
	h.ServeHTTP(w, r)
	if w.Code != 401 {
		t.Fatal(w.Code)
	}
	for _, value := range []string{string(secret), sign(body), "private_payload", strings.Repeat("0", 64), "abc-123", "issues", "delivery_id", "event_name"} {
		if strings.Contains(logs.String(), value) {
			t.Fatal("sensitive rejection log")
		}
	}
	if !strings.Contains(logs.String(), "rejected_authentication") {
		t.Fatal("missing rejection outcome")
	}
}
