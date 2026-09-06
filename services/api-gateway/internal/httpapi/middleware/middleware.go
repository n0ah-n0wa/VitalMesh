// Package middleware provides the cross-cutting HTTP handlers applied to
// every route: request identification, request logging and panic recovery.
package middleware

import "net/http"

// Middleware wraps a handler with additional behaviour.
type Middleware func(http.Handler) http.Handler

// Chain applies middlewares to h so that the first one listed is the
// outermost, i.e. the first to see a request and the last to see the response.
func Chain(h http.Handler, middlewares ...Middleware) http.Handler {
	for i := len(middlewares) - 1; i >= 0; i-- {
		h = middlewares[i](h)
	}
	return h
}
