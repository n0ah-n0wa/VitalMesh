package middleware

import (
	"bytes"
	"context"
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
//
// # The Redis lock
//
// `locks`, when given, is a short-lived Redis lock taken before the
// PostgreSQL record is claimed (SPECIFICATIONS.md section 23, OQ-10). It is
// the ephemeral coordination this design needs and nothing more: it lets a
// concurrent replay be refused without a database round trip. Correctness
// never rests on it. The unique constraint on (account, method, path, key)
// is what actually serialises replays, so when Redis is unavailable the
// lock is skipped and the outcome is identical, only slower to discover.
func Idempotency(store idempotency.Store, ttl time.Duration, locks Locker, logger *slog.Logger) Middleware {
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
				Fingerprint: idempotency.Fingerprint(r.Method, r.URL.RequestURI(), body),
				ExpiresAt:   time.Now().Add(ttl),
			}

			// A concurrent replay is refused here, before the database is
			// touched. A lock that cannot be consulted because Redis is
			// gone changes nothing: the claim below still serialises.
			switch release, outcome := acquireLock(r, locks, req); outcome {
			case lockTaken:
				defer release()
			case lockBusy:
				w.Header().Set("Retry-After", "1")
				respond.Error(w, r, logger, idempotency.ErrInProgress())
				return
			case lockUnavailable:
				// Degrade: PostgreSQL serialises this on its own.
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
					ctx, cancel := settle(r)
					defer cancel()
					_ = store.Delete(ctx, record.ID)
				}
			}()
			next.ServeHTTP(rec, r)
			finished = true

			status := rec.status
			if !rec.wroteHeader {
				status = http.StatusOK
			}
			if status >= 500 {
				ctx, cancel := settle(r)
				defer cancel()
				if err := store.Delete(ctx, record.ID); err != nil {
					logger.WarnContext(ctx, "idempotency record not released", "error", err)
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
					ctx, cancel := settle(r)
					defer cancel()
					logger.ErrorContext(ctx, "idempotency: non-JSON response not stored")
					_ = store.Delete(ctx, record.ID)
					rec.flushTo(w)
					return
				}
			}
			ctx, cancel := settle(r)
			defer cancel()
			if err := store.Complete(ctx, record.ID, status, storedHeaders(rec.header), stored); err != nil {
				// The state change happened; the client gets its response and
				// a replay will be refused as in progress until expiry rather
				// than duplicated.
				logger.ErrorContext(ctx, "idempotency record not completed", "error", err)
			}
			rec.flushTo(w)
		})
	}
}

// settleTimeout bounds the writes that record what a request did. It is
// generous compared with the writes themselves, which are single-row
// statements on a primary key.
const settleTimeout = 5 * time.Second

// settle returns the context for the writes that must happen after the
// handler has run: storing the response, or releasing the key of a request
// that failed.
//
// Those writes are detached from the request's context on purpose. A client
// that hangs up, or a request timeout that fires, ends r.Context() the
// moment the handler returns; running these writes on it would mean a
// failed request keeps its key claimed and a successful one never stores
// its response, so every retry is refused as in progress until the key
// expires. The operation already happened, so recording it is no longer the
// client's to cancel.
func settle(r *http.Request) (context.Context, context.CancelFunc) {
	return context.WithTimeout(context.WithoutCancel(r.Context()), settleTimeout)
}

// replayedHeaders are the response headers stored with a record and set
// again on a replay, so that a replay is the same response rather than the
// same status and body.
//
// It is an allow-list rather than a copy of everything: a response header
// is only stored when replaying it is meaningful and safe. Per-request
// headers (request ids, rate-limit budgets) would be wrong to replay, and
// anything carrying a credential must never be persisted at all.
var replayedHeaders = []string{"Content-Type", "Location"}

// storedHeaders picks the headers worth keeping out of a response.
func storedHeaders(h http.Header) map[string]string {
	out := make(map[string]string, len(replayedHeaders))
	for _, name := range replayedHeaders {
		if value := h.Get(name); value != "" {
			out[name] = value
		}
	}
	return out
}

// Locker is the ephemeral lock the middleware uses to fail fast. Every
// method may report that the lock could not be taken; that is never an
// error the request should fail on.
type Locker interface {
	// Acquire reports whether the lock was taken. A false with a nil error
	// means someone else holds it; any error means the lock could not be
	// consulted at all.
	Acquire(ctx context.Context, key string, ttl time.Duration) (bool, error)
	// Release drops a lock this process took.
	Release(ctx context.Context, key string) error
	// Key builds the namespaced lock key.
	Key(parts ...string) string
}

// lockTTL bounds how long a lock survives a process that dies holding it.
// It only has to outlast a single request, which the HTTP request timeout
// already bounds.
const lockTTL = 30 * time.Second

// lockOutcome is what one attempt at the lock established.
type lockOutcome int

const (
	// lockTaken: this request holds the lock and must release it.
	lockTaken lockOutcome = iota
	// lockBusy: another request holds it, so this one is a concurrent
	// replay and is refused.
	lockBusy
	// lockUnavailable: the lock could not be consulted, or there is no
	// locker at all. The request proceeds and the database serialises it.
	lockUnavailable
)

// acquireLock makes exactly one attempt. The three outcomes are kept apart
// deliberately: only "another request holds it" may refuse a request, and
// "Redis could not answer" must never be mistaken for it.
func acquireLock(r *http.Request, locks Locker, req idempotency.Request) (func(), lockOutcome) {
	if locks == nil {
		return nil, lockUnavailable
	}
	key := locks.Key("idempotency", req.UserID.String(), req.Method, req.Path, req.Key)
	taken, err := locks.Acquire(r.Context(), key, lockTTL)
	switch {
	case err != nil:
		return nil, lockUnavailable
	case !taken:
		return nil, lockBusy
	}
	return func() {
		// Released on a context detached from the request's: the request
		// may be finishing precisely because the client gave up, and
		// holding the lock for its full time to live would refuse a
		// legitimate retry.
		ctx, cancel := context.WithTimeout(context.WithoutCancel(r.Context()), time.Second)
		defer cancel()
		_ = locks.Release(ctx, key)
	}, lockTaken
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
	for name, value := range record.ResponseHeaders {
		w.Header().Set(name, value)
	}
	w.Header().Set(ReplayedHeader, "true")
	if len(record.ResponseBody) == 0 {
		w.WriteHeader(*record.ResponseStatus)
		return
	}
	if w.Header().Get("Content-Type") == "" {
		w.Header().Set("Content-Type", "application/json")
	}
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
