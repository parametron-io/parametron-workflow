package storage

import (
	"context"
	"errors"
	"path/filepath"
	"reflect"
	"testing"
	"time"
)

func TestProvenanceRecordsStableNamespaceIsolationAndRestart(t *testing.T) {
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "records.sqlite")
	s, err := Open(ctx, path)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { s.Close() }()
	ps, err := s.ProvenanceRecords(ctx, "review_a")
	if err != nil || len(ps) != 0 {
		t.Fatal(ps, err)
	}
	at := time.Date(2026, 10, 13, 10, 0, 0, 0, time.UTC)
	_, err = s.InsertDelivery(ctx, Delivery{ID: "origin", EventName: "pull_request", Payload: []byte(`{}`), ReceivedAt: at})
	if err != nil {
		t.Fatal(err)
	}
	for _, p := range []Provenance{{Namespace: "review_a", Key: "z", DeliveryID: "origin", Metadata: []byte(`{"value":1}`), CreatedAt: at}, {Namespace: "review_b", Key: "a", DeliveryID: "origin", Metadata: []byte(`{}`), CreatedAt: at}, {Namespace: "review_a", Key: "a", DeliveryID: "origin", Metadata: []byte(`{"value":2}`), CreatedAt: at}} {
		if _, err = s.RecordProvenance(ctx, p); err != nil {
			t.Fatal(err)
		}
	}
	ps, err = s.ProvenanceRecords(ctx, "review_a")
	if err != nil || len(ps) != 2 || ps[0].Key != "a" || ps[1].Key != "z" || ps[0].DeliveryID != "origin" {
		t.Fatal(ps, err)
	}
	if err = s.Close(); err != nil {
		t.Fatal(err)
	}
	s, err = Open(ctx, path)
	if err != nil {
		t.Fatal(err)
	}
	again, err := s.ProvenanceRecords(ctx, "review_a")
	if err != nil || !reflect.DeepEqual(ps, again) {
		t.Fatal(again, err)
	}
	if _, err = s.ProvenanceRecords(ctx, "bad namespace"); !errors.Is(err, ErrInvalid) {
		t.Fatal(err)
	}
	cancelled, cancel := context.WithCancel(ctx)
	cancel()
	if _, err = s.ProvenanceRecords(cancelled, "review_a"); !errors.Is(err, context.Canceled) {
		t.Fatal(err)
	}
}
