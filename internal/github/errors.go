package github

import "fmt"

type Category string

const (
	NotFound     Category = "not_found"
	Unauthorized Category = "unauthorized"
	Forbidden    Category = "forbidden"
	RateLimited  Category = "rate_limited"
	Conflict     Category = "conflict"
	Malformed    Category = "malformed_response"
	Transient    Category = "transient"
	Permanent    Category = "permanent_request"
)

// Error deliberately excludes API bodies, credentials and provider messages.
// Cause is retained only for context cancellation/deadline diagnostics.
type Error struct {
	Category Category
	Status   int
	Cause    error
}

func (e *Error) Error() string { return fmt.Sprintf("github: %s (HTTP %d)", e.Category, e.Status) }
func (e *Error) Unwrap() error { return e.Cause }
func failure(c Category) error { return &Error{Category: c} }
