package middleware

import (
	"log/slog"
	"net/http"
	"strconv"

	"github.com/n0ah-n0wa/VitalMesh/services/api-gateway/internal/domain"
	"github.com/n0ah-n0wa/VitalMesh/services/api-gateway/internal/httpapi/respond"
	"github.com/n0ah-n0wa/VitalMesh/services/api-gateway/internal/ratelimit"
)

// Rate-limit response headers, as the IETF draft names them. They are set on
// every rate-limited response, allowed or refused, so a client can pace
// itself rather than discover the limit by hitting it.
const (
	RateLimitLimitHeader     = "RateLimit-Limit"
	RateLimitRemainingHeader = "RateLimit-Remaining"
	RateLimitResetHeader     = "RateLimit-Reset"
)

// CodeRateLimited is the stable code of a refusal.
const CodeRateLimited = "RATE_LIMIT_EXCEEDED"

// RateLimit applies the distributed rate limit of SPECIFICATIONS.md section
// 29 and answers 429 with Retry-After when a caller is over budget.
//
// It runs after authentication, so an authenticated request is counted
// against its subject rather than its address, and one caller behind a
// shared address cannot spend another's budget.
//
// A limiter that cannot reach Redis still decides, using this replica's own
// counter, so this middleware has no failure path of its own: it either
// allows or refuses, never errors.
func RateLimit(limiter *ratelimit.Limiter, logger *slog.Logger) Middleware {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			identity := limiter.Identify(r)
			decision := limiter.Allow(r.Context(), identity)

			w.Header().Set(RateLimitLimitHeader, strconv.Itoa(decision.Limit))
			w.Header().Set(RateLimitRemainingHeader, strconv.Itoa(decision.Remaining))
			w.Header().Set(RateLimitResetHeader, strconv.Itoa(int(decision.Reset.Round(0).Seconds())))

			if decision.Allowed {
				next.ServeHTTP(w, r)
				return
			}

			retryAfter := decision.RetryAfter()
			w.Header().Set("Retry-After", strconv.Itoa(int(retryAfter.Seconds())))
			// The identity is logged, never the limit's subject value
			// itself beyond what identifies the caller operationally.
			logger.WarnContext(r.Context(), "rate limit exceeded",
				"role", identity.Role,
				"limit", decision.Limit,
				"backend", string(decision.Backend),
				"method", r.Method,
				"path", r.URL.Path)
			respond.Error(w, r, logger, domain.New(domain.KindRateLimited, CodeRateLimited,
				"Too many requests; retry after the period given in Retry-After."))
		})
	}
}
