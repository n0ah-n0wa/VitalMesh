package middleware

import (
	"context"
	"net/http"
	"sync/atomic"
)

// The registered route pattern is the only bounded-cardinality name for a
// request ("GET /api/v1/patients/{patient_id}" rather than a path with an
// id in it). net/http sets it on the request the matched handler receives,
// which is a copy of the one the outer middleware holds. A holder placed in
// the context by the outer middleware and filled in by the router bridges
// the two. The router may fill it from the goroutine the Timeout
// middleware runs handlers on while the outer middleware reads it after a
// deadline, so the value is atomic.

// UnmatchedRoute labels requests no route matched (404, 405).
const UnmatchedRoute = "unmatched"

type routeHolderKey struct{}

type routeHolder struct{ pattern atomic.Pointer[string] }

// ensureRouteHolder returns the request carrying a route holder, creating
// one when the context has none yet, and the holder.
func ensureRouteHolder(r *http.Request) (*http.Request, *routeHolder) {
	if h, ok := r.Context().Value(routeHolderKey{}).(*routeHolder); ok {
		return r, h
	}
	h := &routeHolder{}
	return r.WithContext(context.WithValue(r.Context(), routeHolderKey{}, h)), h
}

// RecordRoute publishes the pattern net/http matched for r to the outer
// middleware. Routers wrap every registered handler with it.
func RecordRoute(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if h, ok := r.Context().Value(routeHolderKey{}).(*routeHolder); ok {
			pattern := r.Pattern
			h.pattern.Store(&pattern)
		}
		next.ServeHTTP(w, r)
	})
}

// route returns the matched pattern for a request that has finished, or
// UnmatchedRoute.
func (h *routeHolder) route() string {
	if h == nil {
		return UnmatchedRoute
	}
	if p := h.pattern.Load(); p != nil && *p != "" {
		return *p
	}
	return UnmatchedRoute
}
