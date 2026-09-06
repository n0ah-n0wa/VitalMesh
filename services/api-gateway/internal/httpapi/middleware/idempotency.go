package middleware

import (
	"bytes"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"strconv"
	"time"

	"github.com/n0ah-n0wa/VitalMesh/services/api-gateway/internal/auth"
	"github.com/n0ah-n0wa/VitalMesh/services/api-gateway/internal/domain"
	"github.com/n0ah-n0wa/VitalMesh/services/api-gateway/internal/httpapi/respond"
	"github.com/n0ah-n0wa/VitalMesh/services/api-gateway/internal/idempotency"
)

// ReplayedHeader marks a response served from the idempotency store.
const ReplayedHeader = "Idempotency-Replayed"

// Idempotency applies SPECIFICATIONS.md section 24 to a write route. A
// request without an Idempotency-Key runs normally. With a key:
//
//   - the key must be well-formed (422 INVALID_IDEMPOTENCY_KEY);
//   - the first request under (account, method, path, key) runs and its
//     response is stored for ttl;
//   - a repeat with the same body gets the stored response, marked with
//     Idempotency-Replayed: true;
//   - a repeat with a different body is refused (422 IDEMPOTENCY_KEY_REUSED);
//   - a repeat while the first is still running is refused (409
//     IDEMPOTENCY_IN_PROGRESS, Retry-After);
//   - a first request that failed with a 5xx leaves no record, so the
//     client can retry it.
//
// The route must be authenticated: the key is scoped to the account. The
// body is read in full here (it is already bounded by BodyLimit) and handed
// to the handler unchanged.
func Idempotency(store idempotency.Store, ttl time.Duration, logger *slog.Logger) Middleware {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			key := r.Header.Get(idempotency.Header)
			if key == "" {
				next.ServeHTTP(w, r)
				return
			}
			if !idempotency.ValidKey(key) {
				respond.Error(w, r, logger, idempotency.ErrInvalidKey())
				return
			}
			principal, ok := auth.FromContext(r.Context())
			if !ok {
				respond.Error(w, r, logger, auth.AuthenticationRequired())
				return
			}

			body, err := io.ReadAll(r.Body)
			if err != nil {
				respond.Error(w, r, logger, readError(err))
				return
			}
			r.Body = io.NopCloser(bytes.NewReader(body))

			req := idempotency.Request{
				UserID:      principal.UserID,
				Method:      r.Method,
				Path:        r.URL.Path,
				Key:         key,
				Fingerprint: idempotency.Fingerprint(r.Method, r.URL.Path, body),
				ExpiresAt:   time.Now().Add(ttl),
			}
			record, created, err := begin(r, store, req)
			if err != nil {
				respond.Error(w, r, logger, err)
				return
			}
			if !created {
				replay(w, r, logger, record, req)
				return
			}

			// First request under this key: run it and store the outcome.
			rec := &bufferedWriter{header: make(http.Header)}
			finished := false
			defer func() {
				if !finished {
					// The handler panicked: leave no record so a retry can run.
					_ = store.Delete(r.Context(), record.ID)
				}
			}()
			next.ServeHTTP(rec, r)
			finished = true

			status := rec.status
			if !rec.wroteHeader {
				status = http.StatusOK
			}
			if status >= 500 {
				if err := store.Delete(r.Context(), record.ID); err != nil {
					logger.WarnContext(r.Context(), "idempotency record not released", "error", err)
				}
				rec.flushTo(w)
				return
			}
			var stored json.RawMessage
			if rec.body.Len() > 0 {
				stored = json.RawMessage(rec.body.Bytes())
				if !json.Valid(stored) {
					// Responses are JSON; anything else cannot be stored in
					// the jsonb column and is a programming error.
					logger.ErrorContext(r.Context(), "idempotency: non-JSON response not stored")
					_ = store.Delete(r.Context(), record.ID)
					rec.flushTo(w)
					return
				}
			}
			if err := store.Complete(r.Context(), record.ID, status, stored); err != nil {
				// The state change happened; the client gets its response and
				// a replay will be refused as in progress until expiry rather
				// than duplicated.
				logger.ErrorContext(r.Context(), "idempotency record not completed", "error", err)
			}
			rec.flushTo(w)
		})
	}
}

// begin claims the key, discarding an expired record first.
func begin(r *http.Request, store idempotency.Store, req idempotency.Request) (domain.IdempotencyRecord, bool, error) {
	for attempt := 0; attempt < 2; attempt++ {
		record, created, err := store.Begin(r.Context(), req)
		if err != nil {
			return domain.IdempotencyRecord{}, false, err
		}
		if created || record.ExpiresAt.After(time.Now()) {
			return record, created, nil
		}
		if err := store.Delete(r.Context(), record.ID); err != nil {
			return domain.IdempotencyRecord{}, false, err
		}
	}
	return domain.IdempotencyRecord{}, false, errors.New("idempotency: could not claim key")
}

func replay(w http.ResponseWriter, r *http.Request, logger *slog.Logger, record domain.IdempotencyRecord, req idempotency.Request) {
	if record.RequestFingerprint != req.Fingerprint {
		respond.Error(w, r, logger, idempotency.ErrKeyReused())
		return
	}
	if record.Status != domain.IdempotencyCompleted || record.ResponseStatus == nil {
		w.Header().Set("Retry-After", "1")
		respond.Error(w, r, logger, idempotency.ErrInProgress())
		return
	}
	w.Header().Set(ReplayedHeader, "true")
	if len(record.ResponseBody) == 0 {
		w.WriteHeader(*record.ResponseStatus)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Content-Length", strconv.Itoa(len(record.ResponseBody)))
	w.WriteHeader(*record.ResponseStatus)
	_, _ = w.Write(record.ResponseBody)
}

func readError(err error) error {
	var tooLarge *http.MaxBytesError
	if errors.As(err, &tooLarge) {
		return domain.New(domain.KindTooLarge, "REQUEST_BODY_TOO_LARGE", "The request body must not exceed "+strconv.FormatInt(tooLarge.Limit, 10)+" bytes.")
	}
	return domain.Wrap(err, domain.KindInvalid, "INVALID_BODY", "The request body could not be read.")
}
