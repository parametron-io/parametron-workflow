package github

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"
)

// Transport implements GraphQL and narrow exact-SHA REST comparison. Decoding
// and request mechanics
// remain private. It performs no retries and refuses redirects.
type Transport struct {
	endpoint string
	http     *http.Client
	tokens   TokenSource
}

func NewTransport(endpoint string, client *http.Client, tokens TokenSource) (*Transport, error) {
	if endpoint == "" {
		endpoint = "https://api.github.com/graphql"
	}
	u, err := url.Parse(endpoint)
	if err != nil || u.Host == "" || (u.Scheme != "https" && u.Scheme != "http") || u.User != nil || u.RawQuery != "" || u.Fragment != "" || tokens == nil {
		return nil, failure(Permanent)
	}
	if client == nil {
		client = &http.Client{Timeout: 30 * time.Second}
	}
	copyClient := *client
	copyClient.CheckRedirect = func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }
	return &Transport{endpoint: endpoint, http: &copyClient, tokens: tokens}, nil
}
func contextFailure(ctx context.Context, err error, category Category) error {
	if ctx.Err() != nil {
		return &Error{Category: category, Cause: ctx.Err()}
	}
	if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
		return &Error{Category: category, Cause: err}
	}
	return failure(category)
}
func (t *Transport) query(ctx context.Context, query string, variables map[string]any, out any) error {
	if err := ctx.Err(); err != nil {
		return contextFailure(ctx, err, Transient)
	}
	body, err := json.Marshal(map[string]any{"query": query, "variables": variables})
	if err != nil {
		return failure(Permanent)
	}
	data, err := t.request(ctx, http.MethodPost, t.endpoint, body)
	if err != nil {
		return err
	}
	var envelope struct {
		Data   json.RawMessage
		Errors []struct {
			Type       string
			Extensions struct{ Code string }
		}
	}
	if json.Unmarshal(data, &envelope) != nil {
		return failure(Malformed)
	}
	if len(envelope.Errors) > 0 {
		c := Permanent
		for _, e := range envelope.Errors {
			kind := e.Type
			if kind == "" {
				kind = e.Extensions.Code
			}
			switch kind {
			case "NOT_FOUND":
				c = NotFound
			case "UNAUTHORIZED", "UNAUTHENTICATED":
				c = Unauthorized
			case "FORBIDDEN":
				c = Forbidden
			case "RATE_LIMITED":
				c = RateLimited
			case "INTERNAL", "INTERNAL_SERVER_ERROR", "SERVICE_UNAVAILABLE":
				c = Transient
			}
		}
		return failure(c)
	}
	if len(envelope.Data) == 0 || string(envelope.Data) == "null" || json.Unmarshal(envelope.Data, out) != nil {
		return failure(Malformed)
	}
	return nil
}

type page struct {
	HasNextPage *bool
	EndCursor   string
}
type connection struct {
	Nodes    []json.RawMessage
	PageInfo *page
}

// connection follows every page and rejects missing metadata or cursor cycles.
func (t *Transport) connection(ctx context.Context, id, typ, field, selection string) ([]json.RawMessage, error) {
	return t.connectionWithExtra(ctx, id, typ, field, selection, "")
}

func (t *Transport) connectionWithExtra(ctx context.Context, id, typ, field, selection, override string) ([]json.RawMessage, error) {
	var result []json.RawMessage
	var cursor any
	seen := map[string]bool{}
	for {
		var data struct {
			Node *struct {
				Typename string `json:"__typename"`
				Result   *connection
			}
		}
		extra := ""
		if field == "closedByPullRequestsReferences" {
			extra = ",includeClosedPrs:true"
		}
		if field == "projectItems" {
			extra = ",includeArchived:true"
		}
		if field == "timelineItems" {
			extra = ",itemTypes:[READY_FOR_REVIEW_EVENT,CONVERT_TO_DRAFT_EVENT]"
		}
		if override != "" {
			extra = override
		}
		q := `query($id:ID!,$cursor:String){node(id:$id){__typename ... on ` + typ + ` {result:` + field + `(first:100,after:$cursor` + extra + `){nodes{` + selection + `} pageInfo{hasNextPage endCursor}}}}}`
		if err := t.query(ctx, q, map[string]any{"id": id, "cursor": cursor}, &data); err != nil {
			return nil, err
		}
		if data.Node == nil {
			return nil, failure(NotFound)
		}
		if data.Node.Typename != typ || data.Node.Result == nil || data.Node.Result.PageInfo == nil || data.Node.Result.PageInfo.HasNextPage == nil || data.Node.Result.Nodes == nil {
			return nil, failure(Malformed)
		}
		for _, node := range data.Node.Result.Nodes {
			if string(node) == "null" {
				return nil, failure(Malformed)
			}
		}
		result = append(result, data.Node.Result.Nodes...)
		p := data.Node.Result.PageInfo
		if !*p.HasNextPage {
			return result, nil
		}
		if p.EndCursor == "" || seen[p.EndCursor] {
			return nil, failure(Malformed)
		}
		seen[p.EndCursor] = true
		cursor = p.EndCursor
	}
}

func (t *Transport) request(ctx context.Context, method, endpoint string, body []byte) ([]byte, error) {
	if err := ctx.Err(); err != nil {
		return nil, contextFailure(ctx, err, Transient)
	}
	if err := consumeRequest(ctx); err != nil {
		return nil, err
	}
	token, err := t.tokens.Token(ctx)
	if err != nil {
		// App-owned credential verification shares the caller's request budget.
		// Exhaustion is incomplete observation, not invalid authentication.
		if errors.Is(err, ErrRequestLimit) {
			return nil, ErrRequestLimit
		}
		var classified *Error
		if errors.As(err, &classified) {
			switch classified.Category {
			case NotFound, Unauthorized, Forbidden, RateLimited, Conflict, Malformed, Transient, Permanent:
				return nil, contextFailure(ctx, err, classified.Category)
			}
		}
		return nil, contextFailure(ctx, err, Unauthorized)
	}
	if strings.TrimSpace(token) == "" || strings.ContainsAny(token, "\r\n") {
		return nil, failure(Unauthorized)
	}
	req, err := http.NewRequestWithContext(ctx, method, endpoint, bytes.NewReader(body))
	if err != nil {
		return nil, contextFailure(ctx, err, Permanent)
	}
	req.Header.Set("Authorization", "Bearer "+token)
	req.Header.Set("User-Agent", "parametron-workflow")
	req.Header.Set("Accept", "application/vnd.github+json")
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("X-GitHub-Api-Version", "2022-11-28")
	resp, err := t.http.Do(req)
	if err != nil {
		return nil, contextFailure(ctx, err, Transient)
	}
	defer resp.Body.Close()
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		// Revoked or no-longer-authorized installation credentials must be
		// freshly verified/minted on the next attempt, without hidden replay.
		if resp.StatusCode == 401 || resp.StatusCode == 403 {
			if source, ok := t.tokens.(*AppCredentials); ok {
				source.invalidate(ctx, token)
			}
		}
		c := Permanent
		switch {
		case resp.StatusCode == 401:
			c = Unauthorized
		case resp.StatusCode == 429 || ((resp.StatusCode == 403) && (resp.Header.Get("X-RateLimit-Remaining") == "0" || resp.Header.Get("Retry-After") != "")):
			c = RateLimited
		case resp.StatusCode == 403:
			c = Forbidden
		case resp.StatusCode == 404:
			c = NotFound
		case resp.StatusCode == 409 || resp.StatusCode == 412:
			c = Conflict
		case resp.StatusCode >= 500:
			c = Transient
		}
		return nil, &Error{Category: c, Status: resp.StatusCode}
	}
	const limit = 8 << 20
	data, err := io.ReadAll(io.LimitReader(resp.Body, limit+1))
	if err != nil {
		return nil, contextFailure(ctx, err, Transient)
	}
	if len(data) > limit {
		return nil, failure(Malformed)
	}
	return data, nil
}
