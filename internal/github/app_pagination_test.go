package github

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"strconv"
	"testing"
	"time"
)

func TestAppRepositoryPaginationCompleteAndBounded(t *testing.T) {
	for _, name := range []string{"two-pages", "five-pages", "short-page", "duplicate-id", "duplicate-node", "duplicate-name", "changed-total", "oversized-page", "cross-account"} {
		t.Run(name, func(t *testing.T) {
			f, s := newAppFixture(t)
			total := 101
			if name == "five-pages" {
				total = 500
			}
			s.cfg.Repositories = nil
			for n := 0; n < total; n++ {
				s.cfg.Repositories = append(s.cfg.Repositories, fmt.Sprintf("repo-%03d", n))
			}
			pages := 0
			f.hook = func(r *http.Request) {
				if r.URL.Path != "/installation/repositories" {
					t.Error("unexpected pagination endpoint")
					return
				}
				pages++
				page, err := strconv.Atoi(r.URL.Query().Get("page"))
				if err != nil || page != pages || r.URL.Query().Get("per_page") != "100" {
					t.Error("nonsequential or unbounded pagination")
				}
				start, end := (page-1)*100, page*100
				if end > total {
					end = total
				}
				repos := []any{}
				for n := start; n < end; n++ {
					repos = append(repos, map[string]any{"id": int64(1000 + n), "node_id": fmt.Sprintf("REPO_%d", n), "name": s.cfg.Repositories[n], "owner": f.installation["account"]})
				}
				if page == 1 && name == "short-page" {
					repos = repos[:99]
				}
				if page == 1 && name == "oversized-page" {
					repos = append(repos, repos[0])
				}
				if page == 2 {
					item := repos[0].(map[string]any)
					switch name {
					case "duplicate-id":
						item["id"] = int64(1000)
					case "duplicate-node":
						item["node_id"] = "REPO_0"
					case "duplicate-name":
						item["name"] = "repo-000"
					case "cross-account":
						item["owner"] = map[string]any{"id": 999, "node_id": "OTHER", "login": "other", "type": "Organization"}
					}
				}
				count := total
				if name == "changed-total" && page == 2 {
					count++
				}
				f.repositories = map[string]any{"total_count": count, "repositories": repos}
			}
			writer := *s.transport
			writer.tokens = appBearer("synthetic-installation-token")
			err := s.verifyRepositories(context.Background(), &writer)
			if name == "two-pages" || name == "five-pages" {
				want := (total + 99) / 100
				if err != nil || pages != want {
					t.Fatal("complete scope rejected or enumeration incomplete", pages, err)
				}
			} else if err == nil {
				t.Fatal("incomplete/conflicting scope accepted")
			}
			if pages > 5 {
				t.Fatal("repository request bound exceeded", pages)
			}
		})
	}
}

func TestAppCancellationDuringAuthentication(t *testing.T) {
	f, s := newAppFixture(t)
	started := make(chan struct{})
	f.hook = func(r *http.Request) {
		if r.URL.Path == "/app" {
			close(started)
			<-r.Context().Done()
		}
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := make(chan error, 1)
	go func() {
		token, err := s.Token(ctx)
		if token != "" {
			done <- errors.New("canceled authentication returned a token")
			return
		}
		done <- err
	}()
	select {
	case <-started:
	case <-time.After(5 * time.Second):
		t.Fatal("authentication did not start")
	}
	cancel()
	select {
	case err := <-done:
		if !errors.Is(err, context.Canceled) {
			t.Fatal("authentication cancellation lost", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("authentication ignored cancellation")
	}
	if s.token != "" {
		t.Fatal("canceled authentication cached credentials")
	}
}

func TestAppMalformedIdentitiesFailClosed(t *testing.T) {
	for _, name := range []string{"app-type", "missing-app-node", "installation-type", "missing-permissions", "malformed-permissions", "missing-suspension", "rest-type", "rest-missing-node", "graph-type", "graph-missing-login", "graph-wrong-node"} {
		t.Run(name, func(t *testing.T) {
			f, s := newAppFixture(t)
			switch name {
			case "app-type":
				f.app["id"] = "not-a-number"
			case "missing-app-node":
				delete(f.app, "node_id")
			case "installation-type":
				f.installation["id"] = []any{}
			case "missing-permissions":
				delete(f.installation, "permissions")
			case "malformed-permissions":
				f.installation["permissions"] = []any{}
			case "missing-suspension":
				delete(f.installation, "suspended_at")
			case "rest-type":
				f.bot["id"] = "not-a-number"
			case "rest-missing-node":
				delete(f.bot, "node_id")
			case "graph-type":
				f.graph["data"].(map[string]any)["node"].(map[string]any)["__typename"] = "User"
			case "graph-missing-login":
				delete(f.graph["data"].(map[string]any)["node"].(map[string]any), "login")
			case "graph-wrong-node":
				f.graph["data"].(map[string]any)["node"].(map[string]any)["id"] = "OTHER"
			}
			if token, err := s.Token(context.Background()); err == nil || token != "" || s.token != "" {
				t.Fatal("malformed identity became trusted authentication")
			}
		})
	}
}
