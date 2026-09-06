// Package httpapi is the HTTP transport: the router, the route table and the
// server lifecycle.
package httpapi

import (
	"log/slog"
	"net/http"
	"slices"
	"strings"
	"sync"

	"github.com/n0ah-n0wa/VitalMesh/services/api-gateway/internal/domain"
	"github.com/n0ah-n0wa/VitalMesh/services/api-gateway/internal/httpapi/middleware"
	"github.com/n0ah-n0wa/VitalMesh/services/api-gateway/internal/httpapi/respond"
)

// Router routes requests by method and path and answers unmatched requests
// with the JSON error envelope instead of net/http's plain-text defaults.
// Like http.ServeMux it is safe to register routes while serving.
type Router struct {
	// mux holds only method-qualified patterns plus the "/" fallback, so
	// literal and wildcard siblings ("/x/batch", "/x/{id}") never conflict.
	mux *http.ServeMux
	// paths indexes every registered path without a method, so the
	// fallback can tell "known path, wrong method" (405) from "unknown
	// path" (404).
	paths  *http.ServeMux
	logger *slog.Logger

	mu      sync.RWMutex
	allowed map[string][]string // path -> registered methods
}

// NewRouter returns an empty Router.
func NewRouter(logger *slog.Logger) *Router {
	rt := &Router{
		mux:     http.NewServeMux(),
		paths:   http.NewServeMux(),
		logger:  logger,
		allowed: make(map[string][]string),
	}
	rt.mux.HandleFunc("/", rt.fallback)
	return rt
}

// Handle registers h for method and path. Other methods on the same path
// receive 405 with an Allow header.
func (rt *Router) Handle(method, path string, h http.Handler) {
	rt.mu.Lock()
	_, registered := rt.allowed[path]
	rt.allowed[path] = append(rt.allowed[path], method)
	rt.mu.Unlock()

	if !registered {
		rt.paths.Handle(path, http.NotFoundHandler())
	}
	rt.mux.Handle(method+" "+path, h)
}

// HandleFunc is Handle for a handler function.
func (rt *Router) HandleFunc(method, path string, h http.HandlerFunc) {
	rt.Handle(method, path, h)
}

// Group returns a view of the router that registers every route under
// prefix, for example an API version.
func (rt *Router) Group(prefix string) *Group {
	return &Group{router: rt, prefix: strings.TrimSuffix(prefix, "/")}
}

func (rt *Router) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	rt.mux.ServeHTTP(w, r)
}

// fallback serves every request no method-qualified pattern matched.
func (rt *Router) fallback(w http.ResponseWriter, r *http.Request) {
	if _, pattern := rt.paths.Handler(r); pattern != "" {
		rt.mu.RLock()
		allow := strings.Join(rt.allowed[pattern], ", ")
		rt.mu.RUnlock()

		w.Header().Set("Allow", allow)
		respond.ErrorStatus(w, r, http.StatusMethodNotAllowed, "METHOD_NOT_ALLOWED", "The method is not allowed for this resource.")
		return
	}
	respond.Error(w, r, rt.logger, domain.New(domain.KindNotFound, "NOT_FOUND", "The requested resource does not exist."))
}

// Group registers routes under a common prefix, each wrapped in the group's
// middleware. Unmatched requests (404, 405) never reach that middleware.
type Group struct {
	router      *Router
	prefix      string
	middlewares []middleware.Middleware
}

// With returns a Group whose routes additionally pass through m, applied
// inside any middleware the receiver already has. The receiver is unchanged.
func (g *Group) With(m ...middleware.Middleware) *Group {
	return &Group{
		router:      g.router,
		prefix:      g.prefix,
		middlewares: append(slices.Clone(g.middlewares), m...),
	}
}

// Handle registers h for method and the prefixed path.
func (g *Group) Handle(method, path string, h http.Handler) {
	g.router.Handle(method, g.prefix+path, middleware.Chain(h, g.middlewares...))
}

// HandleFunc is Handle for a handler function.
func (g *Group) HandleFunc(method, path string, h http.HandlerFunc) {
	g.Handle(method, path, h)
}
