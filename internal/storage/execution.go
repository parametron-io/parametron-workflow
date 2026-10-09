package storage

import (
	"context"
	"math"
	"time"
)

// Claim atomically starts an eligible attempt. ErrConflict means it is not eligible.
// Enumeration is never authority to execute. Attempts are the claim generation.
func (s *Store) Claim(ctx context.Context, id string, now time.Time) (Event, error) {
	var claimed Event
	if !validTime(now) {
		return claimed, ErrInvalid
	}
	err := s.Transaction(ctx, func(tx *Tx) error {
		e, err := tx.Event(id)
		if err != nil {
			return err
		}
		if e.State.Status != Pending && e.State.Status != Retryable {
			return ErrConflict
		}
		if e.State.Status == Retryable && e.State.NextAttemptAt != nil && e.State.NextAttemptAt.After(now) {
			return ErrConflict
		}
		if e.State.Attempts == math.MaxInt64 {
			return ErrInvalid
		}
		e.State = ProcessState{Status: Processing, Attempts: e.State.Attempts + 1}
		if err := tx.UpdateState(id, e.State); err != nil {
			return err
		}
		claimed = e
		return nil
	})
	return claimed, err
}

// Settle proves ownership of the active generation, preserving its attempt count.
// Pending releases interrupted work; Completed, Retryable and Failed settle it.
func (s *Store) Settle(ctx context.Context, id string, attempt int64, status Status, next *time.Time, category string) error {
	if attempt <= 0 || (status != Pending && status != Completed && status != Retryable && status != Failed) {
		return ErrInvalid
	}
	if status == Retryable {
		if next == nil || category == "" {
			return ErrInvalid
		}
	} else if next != nil {
		return ErrInvalid
	}
	if (status == Pending || status == Completed) && category != "" {
		return ErrInvalid
	}
	if status == Failed && category == "" {
		return ErrInvalid
	}
	return s.Transaction(ctx, func(tx *Tx) error {
		e, err := tx.Event(id)
		if err != nil {
			return err
		}
		if e.State.Status != Processing || e.State.Attempts != attempt {
			return ErrConflict
		}
		return tx.UpdateState(id, ProcessState{status, attempt, next, category})
	})
}

// RecoverInterrupted requires exclusive runtime ownership: no live attempts may
// exist. It retains generations and evidence and releases only processing rows.
func (s *Store) RecoverInterrupted(ctx context.Context) error {
	return s.Transaction(ctx, func(tx *Tx) error {
		_, err := tx.tx.ExecContext(ctx, "UPDATE processing SET status='pending',next_attempt_at=NULL,error_category='' WHERE status='processing'")
		return err
	})
}
