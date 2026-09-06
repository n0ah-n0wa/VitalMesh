package httpapi

import (
	"fmt"
	"log/slog"
	"net/http"

	"github.com/n0ah-n0wa/VitalMesh/services/api-gateway/internal/authz"
	"github.com/n0ah-n0wa/VitalMesh/services/api-gateway/internal/config"
	"github.com/n0ah-n0wa/VitalMesh/services/api-gateway/internal/httpapi/handler"
	"github.com/n0ah-n0wa/VitalMesh/services/api-gateway/internal/httpapi/middleware"
)

// APIv1 is the prefix of the current public API version. Breaking changes
// require a new prefix.
const APIv1 = "/api/v1"

// Handlers are the feature handlers and guards the route table wires.
type Handlers struct {
	Health *handler.Health
	Auth   *handler.Auth
	// Authenticate establishes the principal on routes that need one.
	Authenticate middleware.Middleware
	// Policy decides what each role may do.
	Policy *authz.Policy
}

// operations binds implemented handlers to the operations of
// authz.Routes. Feature handlers join this map as they are implemented;
// the table decides how each is guarded.
func (h Handlers) operations() map[authz.RouteKey]http.Handler {
	ops := make(map[authz.RouteKey]http.Handler)
	if h.Auth != nil {
		ops[authz.RouteKey{Method: http.MethodPost, Path: "/auth/login"}] = http.HandlerFunc(h.Auth.Login)
		ops[authz.RouteKey{Method: http.MethodGet, Path: "/auth/me"}] = http.HandlerFunc(h.Auth.Me)
	}
	return ops
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

	if err := Mount(rt.Group(APIv1), h.operations(), h.Authenticate, h.Policy, logger); err != nil {
		return nil, err
	}
	return Wrap(cfg, logger, rt), nil
}

// Mount registers every operation of authz.Routes that has a handler in
// ops on g, guarded by its rule: public operations are registered bare,
// every other one behind authenticate and a permission check under policy.
// It fails when ops contains an operation the table does not list.
func Mount(g *Group, ops map[authz.RouteKey]http.Handler, authenticate middleware.Middleware, policy *authz.Policy, logger *slog.Logger) error {
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
			g.Handle(rule.Method, rule.Path, h)
			continue
		}
		if authenticate == nil {
			return fmt.Errorf("mount routes: %s %s needs authentication but none is configured", rule.Method, rule.Path)
		}
		g.With(authenticate, middleware.Require(policy, rule.Permission, logger)).Handle(rule.Method, rule.Path, h)
	}
	for k := range remaining {
		return fmt.Errorf("mount routes: %s %s has a handler but no authorization rule", k.Method, k.Path)
	}
	return nil
}

// Wrap applies the standard middleware chain to h, outermost first: request
// identification, security headers, request logging, the request timeout,
// panic recovery and the body size limit.
func Wrap(cfg config.HTTP, logger *slog.Logger, h http.Handler) http.Handler {
	return middleware.Chain(h,
		middleware.RequestID(),
		middleware.SecureHeaders(),
		middleware.Logging(logger),
		middleware.Timeout(cfg.RequestTimeout),
		middleware.Recover(logger),
		middleware.BodyLimit(cfg.MaxBodyBytes),
	)
}
