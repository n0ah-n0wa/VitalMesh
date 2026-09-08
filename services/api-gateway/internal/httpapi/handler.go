package httpapi

import (
	"fmt"
	"log/slog"
	"net/http"

	"github.com/n0ah-n0wa/VitalMesh/services/api-gateway/internal/authz"
	"github.com/n0ah-n0wa/VitalMesh/services/api-gateway/internal/config"
	"github.com/n0ah-n0wa/VitalMesh/services/api-gateway/internal/httpapi/handler"
	"github.com/n0ah-n0wa/VitalMesh/services/api-gateway/internal/httpapi/middleware"
	"github.com/n0ah-n0wa/VitalMesh/services/api-gateway/internal/observability/metrics"
)

// APIv1 is the prefix of the current public API version. Breaking changes
// require a new prefix.
const APIv1 = "/api/v1"

// Handlers are the feature handlers and guards the route table wires.
type Handlers struct {
	Health       *handler.Health
	Auth         *handler.Auth
	Patients     *handler.Patients
	Measurements *handler.Measurements
	Processing   *handler.Processing
	// Authenticate establishes the principal on routes that need one.
	Authenticate middleware.Middleware
	// Idempotency applies Idempotency-Key handling to the write operations
	// listed in idempotentOperations; nil disables it.
	Idempotency middleware.Middleware
	// RateLimit bounds how often one caller may use the API; nil disables
	// it. It runs after authentication so that an authenticated request is
	// counted against its account rather than its address.
	RateLimit middleware.Middleware
	// Policy decides what each role may do.
	Policy *authz.Policy
	// Metrics receives request measurements; nil means none.
	Metrics metrics.Recorder
}

// operations binds implemented handlers to the operations of
// authz.Routes. Feature handlers join this map as they are implemented;
// the table decides how each is guarded.
func (h Handlers) operations() map[authz.RouteKey]http.Handler {
	ops := make(map[authz.RouteKey]http.Handler)
	bind := func(method, path string, f http.HandlerFunc) {
		ops[authz.RouteKey{Method: method, Path: path}] = f
	}
	if h.Auth != nil {
		bind(http.MethodPost, "/auth/login", h.Auth.Login)
		bind(http.MethodGet, "/auth/me", h.Auth.Me)
	}
	if h.Patients != nil {
		bind(http.MethodPost, "/patients", h.Patients.Create)
		bind(http.MethodGet, "/patients", h.Patients.List)
		bind(http.MethodGet, "/patients/{patient_id}", h.Patients.Get)
		bind(http.MethodDelete, "/patients/{patient_id}", h.Patients.Delete)
	}
	if h.Measurements != nil {
		bind(http.MethodPost, "/measurements", h.Measurements.Create)
		bind(http.MethodPost, "/measurements/batch", h.Measurements.CreateBatch)
		bind(http.MethodGet, "/measurements/{measurement_id}", h.Measurements.Get)
		bind(http.MethodDelete, "/measurements/{measurement_id}", h.Measurements.Delete)
		bind(http.MethodGet, "/patients/{patient_id}/measurements", h.Measurements.ListByPatient)
	}
	if h.Processing != nil {
		bind(http.MethodPost, "/processing/jobs", h.Processing.CreateJob)
		bind(http.MethodGet, "/processing/jobs/{job_id}", h.Processing.GetJob)
		bind(http.MethodGet, "/patients/{patient_id}/processing-results", h.Processing.ListResults)
	}
	return ops
}

// idempotentOperations are the write operations that accept an
// Idempotency-Key (SPECIFICATIONS.md section 24): those where a repeated
// request could create duplicate state.
var idempotentOperations = map[authz.RouteKey]bool{
	{Method: http.MethodPost, Path: "/patients"}:           true,
	{Method: http.MethodPost, Path: "/measurements"}:       true,
	{Method: http.MethodPost, Path: "/measurements/batch"}: true,
	{Method: http.MethodPost, Path: "/processing/jobs"}:    true,
}

// NewHandler assembles the route table and wraps it in the middleware chain.
// Health routes are unversioned and public. Every /api/v1 operation comes
// from authz.Routes and is guarded according to its rule; an implemented
// handler for an operation the table does not list is an error, so no
// route can be served without an authorization decision.
func NewHandler(cfg config.HTTP, logger *slog.Logger, h Handlers) (http.Handler, error) {
	rt := NewRouter(logger)
	rt.HandleFunc(http.MethodGet, "/health", h.Health.Live)
	rt.HandleFunc(http.MethodGet, "/ready", h.Health.Ready)

	if err := Mount(rt.Group(APIv1), h.operations(), h.Authenticate, h.Policy, h.Idempotency, h.RateLimit, logger); err != nil {
		return nil, err
	}
	return Wrap(cfg, logger, h.Metrics, rt), nil
}

// Mount registers every operation of authz.Routes that has a handler in
// ops on g, guarded by its rule: public operations are registered bare,
// every other one behind authenticate and a permission check under policy,
// and the idempotent write operations additionally behind idempotent when
// it is given. It fails when ops contains an operation the table does not
// list.
func Mount(g *Group, ops map[authz.RouteKey]http.Handler, authenticate middleware.Middleware, policy *authz.Policy, idempotent, rateLimit middleware.Middleware, logger *slog.Logger) error {
	if policy == nil {
		return fmt.Errorf("mount routes: no authorization policy")
	}
	remaining := make(map[authz.RouteKey]http.Handler, len(ops))
	for k, v := range ops {
		remaining[k] = v
	}
	for _, rule := range authz.Routes {
		h, ok := remaining[rule.Key()]
		if !ok {
			continue
		}
		delete(remaining, rule.Key())
		if rule.Permission == authz.Public {
			// A public route is rate-limited by client address: it is the
			// only identity there is, and an unauthenticated endpoint is
			// the one most in need of a bound.
			public := g
			if rateLimit != nil {
				public = g.With(rateLimit)
			}
			public.Handle(rule.Method, rule.Path, h)
			continue
		}
		if authenticate == nil {
			return fmt.Errorf("mount routes: %s %s needs authentication but none is configured", rule.Method, rule.Path)
		}
		// Rate limiting runs after authentication so that the caller's
		// account, not their address, is what the budget belongs to, and
		// before authorization so that a caller cannot spend the budget of
		// an endpoint they may not use.
		chain := []middleware.Middleware{authenticate}
		if rateLimit != nil {
			chain = append(chain, rateLimit)
		}
		chain = append(chain, middleware.Require(policy, rule.Permission, logger))
		guarded := g.With(chain...)
		if idempotent != nil && idempotentOperations[rule.Key()] {
			guarded = guarded.With(idempotent)
		}
		guarded.Handle(rule.Method, rule.Path, h)
	}
	for k := range remaining {
		return fmt.Errorf("mount routes: %s %s has a handler but no authorization rule", k.Method, k.Path)
	}
	return nil
}

// Wrap applies the standard middleware chain to h, outermost first: request
// identification, security headers, request logging, request metrics, the
// request timeout, panic recovery and the body size limit.
func Wrap(cfg config.HTTP, logger *slog.Logger, rec metrics.Recorder, h http.Handler) http.Handler {
	return middleware.Chain(h,
		middleware.RequestID(),
		middleware.SecureHeaders(),
		middleware.Logging(logger),
		middleware.Metrics(rec),
		middleware.Timeout(cfg.RequestTimeout),
		middleware.Recover(logger),
		middleware.BodyLimit(cfg.MaxBodyBytes),
	)
}
