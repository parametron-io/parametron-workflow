package github

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

func TestRequestBudgetTransport(t *testing.T) {
	for _, maximum := range []int{-1, 0, 1, 3} {
		t.Run(fmt.Sprint(maximum), func(t *testing.T) {
			var tokens, requests atomic.Int64
			s := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { requests.Add(1); w.Write([]byte("{}")) }))
			defer s.Close()
			tr, err := NewTransport(s.URL, s.Client(), TokenFunc(func(context.Context) (string, error) { tokens.Add(1); return "token", nil }))
			if err != nil {
				t.Fatal(err)
			}
			ctx := WithRequestLimit(context.Background(), maximum)
			allowed := maximum
			if allowed < 0 {
				allowed = 0
			}
			for i := 0; i < allowed; i++ {
				if _, err := tr.request(ctx, http.MethodPost, s.URL, nil); err != nil {
					t.Fatal(err)
				}
			}
			for i := 0; i < 3; i++ {
				_, err := tr.request(ctx, http.MethodPost, s.URL, []byte("mutation"))
				if !errors.Is(err, ErrRequestLimit) {
					t.Fatalf("exhaustion: %v", err)
				}
				var typed *Error
				if errors.As(err, &typed) {
					t.Fatalf("budget became transport category: %v", err)
				}
			}
			if tokens.Load() != int64(allowed) || requests.Load() != int64(allowed) {
				t.Fatal(tokens.Load(), requests.Load())
			}
			if _, err := tr.request(context.Background(), http.MethodGet, s.URL, nil); err != nil {
				t.Fatal(err)
			}
		})
	}
}
func TestRequestBudgetNestedAndIndependent(t *testing.T) {
	parent := WithRequestLimit(context.Background(), 3)
	child := WithRequestLimit(parent, 2)
	for i := 0; i < 2; i++ {
		if err := consumeRequest(child); err != nil {
			t.Fatal(err)
		}
	}
	if !errors.Is(consumeRequest(child), ErrRequestLimit) {
		t.Fatal("child exceeded")
	}
	if err := consumeRequest(parent); err != nil {
		t.Fatal("failed child consumed parent", err)
	}
	if !errors.Is(consumeRequest(parent), ErrRequestLimit) {
		t.Fatal("parent exceeded")
	}
	other := WithRequestLimit(context.Background(), 1)
	if err := consumeRequest(other); err != nil {
		t.Fatal(err)
	}
	exhaustedParent := WithRequestLimit(context.Background(), 1)
	if err := consumeRequest(exhaustedParent); err != nil {
		t.Fatal(err)
	}
	if !errors.Is(consumeRequest(WithRequestLimit(exhaustedParent, 10)), ErrRequestLimit) {
		t.Fatal("child bypassed parent")
	}
}
func TestRequestBudgetConcurrent(t *testing.T) {
	for _, nested := range []bool{false, true} {
		ctx := WithRequestLimit(context.Background(), 17)
		if nested {
			ctx = WithRequestLimit(ctx, 100)
		}
		var successes atomic.Int64
		var wg sync.WaitGroup
		for i := 0; i < 128; i++ {
			wg.Add(1)
			go func() {
				defer wg.Done()
				err := consumeRequest(ctx)
				if err == nil {
					successes.Add(1)
				} else if !errors.Is(err, ErrRequestLimit) {
					t.Error(err)
				}
			}()
		}
		wg.Wait()
		if successes.Load() != 17 {
			t.Fatal(successes.Load())
		}
	}
}
func TestRequestBudgetCancellationPrecedence(t *testing.T) {
	tr := adapter(t, func(request) any { t.Error("HTTP after cancellation"); return nil })
	for _, deadline := range []bool{false, true} {
		var ctx context.Context
		var cancel context.CancelFunc
		want := context.Canceled
		if deadline {
			ctx, cancel = context.WithDeadline(context.Background(), time.Time{})
			want = context.DeadlineExceeded
		} else {
			ctx, cancel = context.WithCancel(context.Background())
		}
		cancel()
		_, err := tr.request(WithRequestLimit(ctx, 0), http.MethodPost, tr.endpoint, nil)
		if !errors.Is(err, want) {
			t.Fatal(err)
		}
	}
}
func TestRequestBudgetNestedLister(t *testing.T) {
	calls := 0
	tr := adapter(t, func(q request) any {
		calls++
		if calls == 1 {
			return map[string]any{"repository": repoJSON()}
		}
		t.Error("budget did not stop nested query")
		return nil
	})
	got, err := tr.ListPullRequests(WithRequestLimit(context.Background(), 1), Repository{ID: "R", Owner: "org", Name: "repo"})
	if !errors.Is(err, ErrRequestLimit) || got != nil || calls != 1 {
		t.Fatal(got, err, calls)
	}
}

func TestRequestBudgetCountsFailedAttempts(t *testing.T) {
	tokens := 0
	tr := adapter(t, func(request) any { t.Error("HTTP after token failure"); return nil })
	tr.tokens = TokenFunc(func(context.Context) (string, error) { tokens++; return "", failure(Transient) })
	ctx := WithRequestLimit(context.Background(), 1)
	_, err := tr.Repository(ctx, "org", "repo")
	category(t, err, Transient)
	_, err = tr.Repository(ctx, "org", "repo")
	if !errors.Is(err, ErrRequestLimit) || tokens != 1 {
		t.Fatal(err, tokens)
	}
}
func TestRequestBudgetConcurrentTransport(t *testing.T) {
	var tokens, requests, successes atomic.Int64
	s := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { requests.Add(1); w.Write([]byte("{}")) }))
	defer s.Close()
	tr, err := NewTransport(s.URL, s.Client(), TokenFunc(func(context.Context) (string, error) { tokens.Add(1); return "token", nil }))
	if err != nil {
		t.Fatal(err)
	}
	parent := WithRequestLimit(context.Background(), 11)
	var wg sync.WaitGroup
	for i := 0; i < 64; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			ctx := WithRequestLimit(parent, 2)
			_, err := tr.request(ctx, http.MethodPost, s.URL, nil)
			if err == nil {
				successes.Add(1)
			} else if !errors.Is(err, ErrRequestLimit) {
				t.Error(err)
			}
		}()
	}
	wg.Wait()
	if tokens.Load() != 11 || requests.Load() != 11 || successes.Load() != 11 {
		t.Fatal(tokens.Load(), requests.Load(), successes.Load())
	}
}
