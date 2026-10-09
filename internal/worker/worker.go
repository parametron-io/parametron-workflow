// Package worker orchestrates durable execution without interpreting payloads.
package worker

import (
	"context"
	"errors"
	"reflect"
	"sync"
	"time"

	"github.com/parametron-io/parametron-workflow/internal/github"
	"github.com/parametron-io/parametron-workflow/internal/storage"
)

type ResourceResolver interface {
	Resolve(context.Context, storage.Event) (storage.Resource, error)
}
type Processor interface {
	Process(context.Context, storage.Event) error
}
type Classification struct {
	Retryable bool
	Category  string
}

// Classifier handles errors outside the GitHub taxonomy; it must return safe,
// stable categories, never error text. Invalid output stops execution safely.
type Classifier func(error) Classification
type RetrySchedule func(Classification, int64, time.Time) time.Time

type Config struct {
	Store         *storage.Store
	Resolver      ResourceResolver
	Processor     Processor
	Classifier    Classifier
	RetrySchedule RetrySchedule
	Concurrency   int
	Clock         func() time.Time
}
type Worker struct {
	cfg Config
	mu  sync.Mutex
}

func nilValue(v any) bool {
	if v == nil {
		return true
	}
	r := reflect.ValueOf(v)
	switch r.Kind() {
	case reflect.Pointer, reflect.Func, reflect.Interface:
		return r.IsNil()
	}
	return false
}
func New(c Config) (*Worker, error) {
	if c.Store == nil || nilValue(c.Resolver) || nilValue(c.Processor) || c.Classifier == nil || c.RetrySchedule == nil || c.Concurrency < 1 {
		return nil, errors.New("worker: explicit execution boundaries and positive concurrency required")
	}
	if c.Clock == nil {
		c.Clock = time.Now
	}
	return &Worker{cfg: c}, nil
}

// Recover must run with exclusive ownership of the database before execution.
func (w *Worker) Recover(ctx context.Context) error {
	w.mu.Lock()
	defer w.mu.Unlock()
	return w.cfg.Store.RecoverInterrupted(ctx)
}
func (w *Worker) classify(err error) Classification {
	var ge *github.Error
	if errors.As(err, &ge) {
		switch ge.Category {
		case github.RateLimited, github.Transient:
			return Classification{true, string(ge.Category)}
		case github.NotFound, github.Unauthorized, github.Forbidden, github.Conflict, github.Malformed, github.Permanent:
			return Classification{false, string(ge.Category)}
		}
	}
	return w.cfg.Classifier(err)
}
func (w *Worker) settle(ctx context.Context, e storage.Event, err error) error {
	cleanup := context.WithoutCancel(ctx)
	if ctx.Err() != nil {
		return w.cfg.Store.Settle(cleanup, e.Delivery.ID, e.State.Attempts, storage.Pending, nil, "")
	}
	if err == nil {
		return w.cfg.Store.Settle(cleanup, e.Delivery.ID, e.State.Attempts, storage.Completed, nil, "")
	}
	c := w.classify(err)
	status := storage.Failed
	var next *time.Time
	if c.Retryable {
		status = storage.Retryable
		now := w.cfg.Clock()
		t := w.cfg.RetrySchedule(c, e.State.Attempts, now)
		if !t.After(now) {
			return storage.ErrInvalid
		}
		next = &t
	}
	return w.cfg.Store.Settle(cleanup, e.Delivery.ID, e.State.Attempts, status, next, c.Category)
}

type key struct {
	owner, repo, kind string
	number            int64
}

func resourceKey(r *storage.Resource) key { return key{r.Owner, r.Repository, r.Kind, r.Number} }

type lane struct {
	events []storage.Event
}

// Step executes one acceptance-ordered snapshot. Only one dispatcher may own a
// database; concurrent Step/Run calls on this Worker serialize. Memory is bounded
// by the snapshot, and goroutines by Concurrency, never by the queue length.
func (w *Worker) Step(ctx context.Context) (result error) {
	w.mu.Lock()
	defer w.mu.Unlock()
	if ctx.Err() != nil {
		return nil
	}
	events, err := w.cfg.Store.Work(ctx)
	if err != nil {
		if ctx.Err() != nil {
			return nil
		}
		return err
	}
	lanes := []*lane{}
	byKey := map[key]*lane{}
	// Resolve sequentially before scheduling, so resolution latency cannot reorder
	// a resource. Unknown blocked identity is an ordering barrier for later work.
	var held []storage.Event
	owned := map[string]int64{}
	defer func() {
		for _, e := range held {
			releaseErr := w.cfg.Store.Settle(context.WithoutCancel(ctx), e.Delivery.ID, e.State.Attempts, storage.Pending, nil, "")
			if releaseErr != nil && !errors.Is(releaseErr, storage.ErrConflict) {
				result = errors.Join(result, releaseErr)
			}
		}
	}()
	for _, e := range events {
		if ctx.Err() != nil {
			return nil
		}
		eligible := e.State.Status == storage.Pending || (e.State.Status == storage.Retryable && (e.State.NextAttemptAt == nil || !e.State.NextAttemptAt.After(w.cfg.Clock())))
		if e.Resource == nil {
			if !eligible {
				break
			}
			e, err = w.cfg.Store.Claim(ctx, e.Delivery.ID, w.cfg.Clock())
			if err != nil {
				if ctx.Err() != nil {
					return nil
				}
				return err
			}
			held = append(held, e)
			owned[e.Delivery.ID] = e.State.Attempts
			r, resolveErr := w.cfg.Resolver.Resolve(ctx, e)
			if resolveErr != nil {
				if err = w.settle(ctx, e, resolveErr); err != nil {
					return err
				}
				break
			}
			if ctx.Err() != nil {
				return nil
			}
			if err = w.cfg.Store.BindResource(ctx, e.Delivery.ID, r); err != nil {
				if ctx.Err() != nil {
					return nil
				}
				return err
			}
			e, err = w.cfg.Store.Event(ctx, e.Delivery.ID)
			if err != nil {
				if ctx.Err() != nil {
					return nil
				}
				return err
			}
		}
		k := resourceKey(e.Resource)
		l := byKey[k]
		if l == nil {
			l = &lane{}
			byKey[k] = l
			lanes = append(lanes, l)
		}
		// A noneligible predecessor blocks the whole lane, including later events.
		l.events = append(l.events, e)
	}
	runCtx, cancel := context.WithCancel(ctx)
	defer cancel()
	jobs := make(chan *lane)
	results := make(chan error, w.cfg.Concurrency)
	var wg sync.WaitGroup
	for i := 0; i < w.cfg.Concurrency; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for l := range jobs {
				var laneErr error
				for _, e := range l.events {
					if runCtx.Err() != nil {
						break
					}
					generation, owns := owned[e.Delivery.ID]
					if !owns || generation != e.State.Attempts || e.State.Status != storage.Processing {
						var claimErr error
						e, claimErr = w.cfg.Store.Claim(runCtx, e.Delivery.ID, w.cfg.Clock())
						if errors.Is(claimErr, storage.ErrConflict) {
							break
						}
						if claimErr != nil {
							if runCtx.Err() == nil {
								laneErr = claimErr
							}
							break
						}
					}
					if runCtx.Err() != nil {
						laneErr = w.settle(runCtx, e, runCtx.Err())
						break
					}
					processErr := w.cfg.Processor.Process(runCtx, e)
					laneErr = w.settle(runCtx, e, processErr)
					if laneErr != nil || processErr != nil || runCtx.Err() != nil {
						break
					}
				}
				results <- laneErr
			}
		}()
	}
	// Dispatch only a bounded number of lanes; drain completions while dispatching.
	sent, done := 0, 0
	var first error
	for done < len(lanes) {
		if (runCtx.Err() != nil || first != nil) && done == sent {
			break
		}
		var out chan *lane
		var next *lane
		if sent < len(lanes) && runCtx.Err() == nil && first == nil {
			out = jobs
			next = lanes[sent]
		}
		select {
		case out <- next:
			sent++
		case result := <-results:
			done++
			if result != nil && first == nil {
				first = result
				cancel()
			}
		case <-runCtx.Done():
			if done == sent {
				break
			} /* drain in-flight below */
			for done < sent {
				result := <-results
				done++
				if first == nil && result != nil {
					first = result
				}
			}
		}
	}
	close(jobs)
	wg.Wait()
	return first
}

// Run recovers once, then executes snapshots separated by an explicit caller-
// supplied idle interval. There is no implicit production polling/retry policy.
func (w *Worker) Run(ctx context.Context, interval time.Duration) error {
	if interval < time.Second {
		return errors.New("worker: poll interval must be at least one second")
	}
	if ctx.Err() != nil {
		return nil
	}
	if err := w.Recover(ctx); err != nil {
		return err
	}
	for {
		if err := w.Step(ctx); err != nil {
			return err
		}
		if ctx.Err() != nil {
			return nil
		}
		timer := time.NewTimer(interval)
		select {
		case <-ctx.Done():
			timer.Stop()
			return nil
		case <-timer.C:
		}
	}
}
