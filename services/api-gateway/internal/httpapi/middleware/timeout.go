package middleware

import (
	"bytes"
	"context"
	"maps"
	"net/http"
	"sync"
	"time"

	"github.com/n0ah-n0wa/VitalMesh/services/api-gateway/internal/httpapi/respond"
)

// Timeout bounds the time a handler may take. The request context carries
// the deadline so that well-behaved handlers stop early; if the handler is
// still running when the deadline passes, the client receives a 504 error
// envelope and whatever the handler writes afterwards is discarded.
//
// The handler's output is buffered until it completes, so responses cannot
// be streamed through this middleware.
func Timeout(d time.Duration) Middleware {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			ctx, cancel := context.WithTimeout(r.Context(), d)
			defer cancel()
			r = r.WithContext(ctx)

			bw := &bufferedWriter{header: make(http.Header)}
			done := make(chan struct{})
			panicked := make(chan any, 1)
			go func() {
				defer func() {
					if p := recover(); p != nil {
						panicked <- p
					}
				}()
				next.ServeHTTP(bw, r)
				close(done)
			}()

			select {
			case p := <-panicked:
				panic(p)
			case <-done:
				bw.flushTo(w)
			case <-ctx.Done():
				bw.timeOut()
				respond.ErrorStatus(w, r, http.StatusGatewayTimeout, respond.TimeoutCode, respond.TimeoutMessage)
			}
		})
	}
}

// bufferedWriter collects a handler's response so it can be forwarded whole
// or dropped after a timeout.
type bufferedWriter struct {
	mu          sync.Mutex
	header      http.Header
	status      int
	body        bytes.Buffer
	wroteHeader bool
	timedOut    bool
}

func (b *bufferedWriter) Header() http.Header { return b.header }

func (b *bufferedWriter) WriteHeader(status int) {
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.timedOut || b.wroteHeader {
		return
	}
	b.status = status
	b.wroteHeader = true
}

func (b *bufferedWriter) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.timedOut {
		return 0, http.ErrHandlerTimeout
	}
	if !b.wroteHeader {
		b.status = http.StatusOK
		b.wroteHeader = true
	}
	return b.body.Write(p)
}

func (b *bufferedWriter) timeOut() {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.timedOut = true
}

// flushTo copies the buffered response to w. It is only called after the
// handler goroutine has finished, so no lock is needed.
func (b *bufferedWriter) flushTo(w http.ResponseWriter) {
	maps.Copy(w.Header(), b.header)
	status := b.status
	if !b.wroteHeader {
		status = http.StatusOK
	}
	w.WriteHeader(status)
	_, _ = w.Write(b.body.Bytes())
}
