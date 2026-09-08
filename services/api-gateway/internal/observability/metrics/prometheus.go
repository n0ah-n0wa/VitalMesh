package metrics

import (
	"net/http"
	"strconv"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/collectors"
	"github.com/prometheus/client_golang/prometheus/promhttp"
)

// Namespace prefixes every metric this application publishes, so a scrape
// target's own series are never confused with the runtime's.
const Namespace = "vitalmesh"

// Prometheus implements [Recorder] over a Prometheus registry.
//
// Every family is registered when the recorder is built, so two components
// cannot publish the same series under different meanings. A vector still
// exposes nothing until it has a label set, which matters for alerting:
// a rule cannot tell "none yet" from "not instrumented". Where the labels
// form a closed set small enough to enumerate, [Prometheus.initialise]
// therefore creates the series at zero, so a scrape taken before any
// traffic already carries them. Where they do not, such as route by method
// by status, the series appears with the first request; that is a property
// of the label space rather than a choice.
type Prometheus struct {
	registry *prometheus.Registry

	httpRequests *prometheus.CounterVec
	httpErrors   *prometheus.CounterVec
	httpLatency  *prometheus.HistogramVec

	operations       *prometheus.CounterVec
	operationLatency *prometheus.HistogramVec
	inFlight         *prometheus.GaugeVec

	databaseLatency *prometheus.HistogramVec
	redisLatency    *prometheus.HistogramVec

	batchSize  *prometheus.HistogramVec
	cacheReads *prometheus.CounterVec
	rateLimit  *prometheus.CounterVec
}

// Buckets are chosen for what each measurement is used for rather than
// copied from a default. A request budget is ten seconds, so the HTTP
// buckets straddle it; a database or Redis call that takes a second is
// already pathological, so those buckets are finer and stop sooner.
var (
	httpBuckets       = []float64{0.005, 0.01, 0.025, 0.05, 0.1, 0.25, 0.5, 1, 2.5, 5, 10}
	dependencyBuckets = []float64{0.001, 0.0025, 0.005, 0.01, 0.025, 0.05, 0.1, 0.25, 0.5, 1, 5}
	// Batches are bounded by configuration in the hundreds to the hundred
	// thousands, so the buckets are exponential across that range.
	batchBuckets = []float64{1, 10, 50, 100, 500, 1000, 5000, 10000, 50000, 100000}
)

// NewPrometheus builds a recorder and registers every metric on its own
// registry. The registry also carries the standard process and Go runtime
// collectors, because "is the service healthy" is rarely answerable from
// application metrics alone.
func NewPrometheus() *Prometheus {
	registry := prometheus.NewRegistry()
	p := &Prometheus{
		registry: registry,

		httpRequests: prometheus.NewCounterVec(prometheus.CounterOpts{
			Namespace: Namespace, Subsystem: "http", Name: "requests_total",
			Help: "HTTP requests served, by method, matched route and status code.",
		}, []string{"method", "route", "status"}),
		httpErrors: prometheus.NewCounterVec(prometheus.CounterOpts{
			Namespace: Namespace, Subsystem: "http", Name: "errors_total",
			Help: "HTTP responses that failed, by method, matched route and class (client or server).",
		}, []string{"method", "route", "class"}),
		httpLatency: prometheus.NewHistogramVec(prometheus.HistogramOpts{
			Namespace: Namespace, Subsystem: "http", Name: "request_duration_seconds",
			Help: "Time to serve an HTTP request, by method and matched route.", Buckets: httpBuckets,
		}, []string{"method", "route"}),

		operations: prometheus.NewCounterVec(prometheus.CounterOpts{
			Namespace: Namespace, Subsystem: "operation", Name: "total",
			Help: "Application operations, by name and outcome. Processing jobs and their failures are the processing.* names.",
		}, []string{"operation", "outcome"}),
		operationLatency: prometheus.NewHistogramVec(prometheus.HistogramOpts{
			Namespace: Namespace, Subsystem: "operation", Name: "duration_seconds",
			Help: "Time an application operation took, by name. Processing duration is the processing.* names.", Buckets: httpBuckets,
		}, []string{"operation"}),
		inFlight: prometheus.NewGaugeVec(prometheus.GaugeOpts{
			Namespace: Namespace, Subsystem: "operation", Name: "in_flight",
			Help: "Operations running right now, by name. The processing.job series is the gateway's active jobs.",
		}, []string{"operation"}),

		databaseLatency: prometheus.NewHistogramVec(prometheus.HistogramOpts{
			Namespace: Namespace, Subsystem: "database", Name: "query_duration_seconds",
			Help: "Time a PostgreSQL statement took, by leading keyword and outcome.", Buckets: dependencyBuckets,
		}, []string{"statement", "outcome"}),
		redisLatency: prometheus.NewHistogramVec(prometheus.HistogramOpts{
			Namespace: Namespace, Subsystem: "redis", Name: "command_duration_seconds",
			Help: "Time a Redis command took, by command and outcome. An unavailable outcome is a call the client refused or that failed.", Buckets: dependencyBuckets,
		}, []string{"command", "outcome"}),

		batchSize: prometheus.NewHistogramVec(prometheus.HistogramOpts{
			Namespace: Namespace, Subsystem: "batch", Name: "size",
			Help: "Items in one batch operation, by name.", Buckets: batchBuckets,
		}, []string{"operation"}),
		cacheReads: prometheus.NewCounterVec(prometheus.CounterOpts{
			Namespace: Namespace, Subsystem: "cache", Name: "reads_total",
			Help: "Cache lookups, by the read they serve and whether they hit.",
		}, []string{"operation", "result"}),
		rateLimit: prometheus.NewCounterVec(prometheus.CounterOpts{
			Namespace: Namespace, Subsystem: "rate_limit", Name: "decisions_total",
			Help: "Rate-limit decisions, by outcome and by which counter decided. The caller is never a label.",
		}, []string{"decision", "backend"}),
	}

	p.initialise()
	registry.MustRegister(
		p.httpRequests, p.httpErrors, p.httpLatency,
		p.operations, p.operationLatency, p.inFlight,
		p.databaseLatency, p.redisLatency,
		p.batchSize, p.cacheReads, p.rateLimit,
		collectors.NewProcessCollector(collectors.ProcessCollectorOpts{}),
		collectors.NewGoCollector(),
	)
	return p
}

// initialise creates the series whose labels are known in advance, so they
// read zero rather than being absent before the first occurrence. A rate
// limit that has refused nobody and a gateway processing no jobs are facts
// worth publishing.
func (p *Prometheus) initialise() {
	for _, decision := range []string{DecisionAllowed, DecisionLimited} {
		for _, backend := range []string{BackendShared, BackendLocal} {
			p.rateLimit.WithLabelValues(decision, backend)
		}
	}
	p.inFlight.WithLabelValues(ProcessingJob)
}

// Registry exposes the registry so that a component owning its own
// collectors, such as the database pool, can register them alongside.
func (p *Prometheus) Registry() *prometheus.Registry { return p.registry }

// Handler serves the exposition format at GET /metrics.
func (p *Prometheus) Handler() http.Handler {
	return promhttp.HandlerFor(p.registry, promhttp.HandlerOpts{
		// A collector that fails should not take the endpoint down: a
		// partial scrape is more useful than none, and the error is
		// reported as a metric of its own.
		ErrorHandling: promhttp.ContinueOnError,
	})
}

func (p *Prometheus) HTTPRequest(method, route string, status int, duration time.Duration) {
	// The one label a client controls is bounded here, at the recorder, so
	// that every caller is covered rather than each remembering to.
	method = Method(method)
	code := strconv.Itoa(status)
	p.httpRequests.WithLabelValues(method, route, code).Inc()
	p.httpLatency.WithLabelValues(method, route).Observe(duration.Seconds())
	if class := errorClass(status); class != "" {
		p.httpErrors.WithLabelValues(method, route, class).Inc()
	}
}

// errorClass names the kind of failure, or "" when the response succeeded.
// Two classes rather than the status code, because the question an error
// count answers is whether the fault is the caller's or ours.
func errorClass(status int) string {
	switch {
	case status >= 500:
		return "server"
	case status >= 400:
		return "client"
	default:
		return ""
	}
}

func (p *Prometheus) Operation(name string, outcome Outcome, duration time.Duration) {
	p.operations.WithLabelValues(name, string(outcome)).Inc()
	p.operationLatency.WithLabelValues(name).Observe(duration.Seconds())
}

func (p *Prometheus) Batch(name string, size int) {
	p.batchSize.WithLabelValues(name).Observe(float64(size))
}

func (p *Prometheus) Cache(name string, hit bool) {
	result := "miss"
	if hit {
		result = "hit"
	}
	p.cacheReads.WithLabelValues(name, result).Inc()
}

func (p *Prometheus) Database(verb string, outcome Outcome, duration time.Duration) {
	p.databaseLatency.WithLabelValues(verb, string(outcome)).Observe(duration.Seconds())
}

func (p *Prometheus) Redis(command string, outcome Outcome, duration time.Duration) {
	p.redisLatency.WithLabelValues(command, string(outcome)).Observe(duration.Seconds())
}

func (p *Prometheus) RateLimit(decision, backend string) {
	p.rateLimit.WithLabelValues(decision, backend).Inc()
}

func (p *Prometheus) InFlight(name string, delta int) {
	p.inFlight.WithLabelValues(name).Add(float64(delta))
}
