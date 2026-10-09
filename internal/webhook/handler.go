// Package webhook authenticates notifications and durably accepts their evidence.
// It does not resolve resources, execute work, or start an HTTP server.
package webhook

import (
	"bytes"
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"mime"
	"net/http"
	"reflect"
	"regexp"
	"strings"
	"time"

	"github.com/parametron-io/parametron-workflow/internal/storage"
)

// MaxBodyBytes accommodates GitHub's documented 25 MB payload cap, including
// the binary interpretation of MB. Reads are bounded independently of length.
const MaxBodyBytes int64 = 25 * 1024 * 1024

type DeliveryStore interface {
	InsertDelivery(context.Context, storage.Delivery) (bool, error)
}

// Config requires a store and an explicitly supplied nonempty secret. Clock
// defaults to time.Now and Logger to slog.Default. Neither loads configuration.
type Config struct {
	Store  DeliveryStore
	Secret []byte
	Clock  func() time.Time
	Logger *slog.Logger
}

type Handler struct {
	store  DeliveryStore
	secret []byte
	clock  func() time.Time
	logger *slog.Logger
}

func New(cfg Config) (*Handler, error) {
	if cfg.Store == nil || nilValue(cfg.Store) || len(cfg.Secret) == 0 {
		return nil, errors.New("webhook: store and nonempty secret required")
	}
	if cfg.Clock == nil {
		cfg.Clock = time.Now
	}
	if cfg.Logger == nil {
		cfg.Logger = slog.Default()
	}
	return &Handler{cfg.Store, bytes.Clone(cfg.Secret), cfg.Clock, cfg.Logger}, nil
}

func nilValue(v any) bool {
	r := reflect.ValueOf(v)
	switch r.Kind() {
	case reflect.Chan, reflect.Func, reflect.Interface, reflect.Map, reflect.Pointer, reflect.Slice:
		return r.IsNil()
	}
	return false
}

var deliveryPattern = regexp.MustCompile(`^[A-Za-z0-9-]{1,128}$`)
var eventPattern = regexp.MustCompile(`^[a-z][a-z0-9_]{0,127}$`)

// singleHeader rejects repeated fields, including differently cased map keys.
// Comma-joined values are subsequently rejected by the field's grammar.
func singleHeader(header http.Header, name string) (string, bool) {
	var values []string
	for key, entries := range header {
		if strings.EqualFold(key, name) {
			values = append(values, entries...)
		}
	}
	returnValue := ""
	if len(values) == 1 {
		returnValue = values[0]
	}
	return returnValue, len(values) == 1 && returnValue != ""
}

func (h *Handler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	var id, event string
	finish := func(status int, outcome string) {
		attrs := []any{"outcome", outcome}
		if id != "" {
			attrs = append(attrs, "delivery_id", id)
		}
		if event != "" {
			attrs = append(attrs, "event_name", event)
		}
		h.logger.InfoContext(r.Context(), "webhook reception", attrs...)
		w.WriteHeader(status)
	}
	if r.Method != http.MethodPost {
		w.Header().Set("Allow", http.MethodPost)
		finish(http.StatusMethodNotAllowed, "rejected_method")
		return
	}
	contentType, ok := singleHeader(r.Header, "Content-Type")
	mediaType, _, err := mime.ParseMediaType(contentType)
	if !ok || err != nil || mediaType != "application/json" {
		finish(http.StatusUnsupportedMediaType, "rejected_media_type")
		return
	}
	value, ok := singleHeader(r.Header, "X-GitHub-Delivery")
	if !ok || !deliveryPattern.MatchString(value) {
		finish(http.StatusBadRequest, "rejected_metadata")
		return
	}
	id = value
	value, ok = singleHeader(r.Header, "X-GitHub-Event")
	if !ok || !eventPattern.MatchString(value) {
		finish(http.StatusBadRequest, "rejected_metadata")
		return
	}
	event = value
	signature, ok := singleHeader(r.Header, "X-Hub-Signature-256")
	if !ok || !strings.HasPrefix(signature, "sha256=") || len(signature) != 7+sha256.Size*2 {
		finish(http.StatusUnauthorized, "rejected_authentication")
		return
	}
	digest, err := hex.DecodeString(signature[7:])
	if err != nil {
		finish(http.StatusUnauthorized, "rejected_authentication")
		return
	}
	if r.ContentLength > MaxBodyBytes {
		finish(http.StatusRequestEntityTooLarge, "rejected_oversized")
		return
	}
	body, err := io.ReadAll(http.MaxBytesReader(w, r.Body, MaxBodyBytes))
	if err != nil {
		var oversized *http.MaxBytesError
		if errors.As(err, &oversized) {
			finish(http.StatusRequestEntityTooLarge, "rejected_oversized")
		} else {
			finish(http.StatusBadRequest, "rejected_body")
		}
		return
	}
	mac := hmac.New(sha256.New, h.secret)
	mac.Write(body)
	if !hmac.Equal(digest, mac.Sum(nil)) {
		finish(http.StatusUnauthorized, "rejected_authentication")
		return
	}
	trimmed := bytes.TrimSpace(body)
	if !json.Valid(body) || len(trimmed) == 0 || trimmed[0] != '{' {
		finish(http.StatusBadRequest, "rejected_json")
		return
	}
	inserted, err := h.store.InsertDelivery(r.Context(), storage.Delivery{
		ID: id, EventName: event, Payload: body, ReceivedAt: h.clock().UTC(), Resource: nil,
	})
	if errors.Is(err, storage.ErrConflict) {
		finish(http.StatusConflict, "conflict")
		return
	}
	if err != nil {
		finish(http.StatusInternalServerError, "persistence_failure")
		return
	}
	if inserted {
		finish(http.StatusNoContent, "accepted")
	} else {
		finish(http.StatusNoContent, "duplicate")
	}
}
