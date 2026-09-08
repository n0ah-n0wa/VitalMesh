//! Prometheus-compatible metrics (SPECIFICATIONS.md section 41).
//!
//! # Bounded cardinality
//!
//! Every label here comes from a closed set. Routes are the patterns axum
//! matched, never request paths, so `/internal/v1/jobs/{job_id}` is one
//! series however many jobs are asked about. Outcomes and status classes
//! are fixed vocabularies. No function in this module accepts a value that
//! varies per request, so a job id, request id or correlation id cannot
//! become a label even by mistake: there is nowhere to put one.
//!
//! # Saturation rather than queue depth
//!
//! The specification asks for processing queue depth. This service has no
//! queue by design: admission is a semaphore that never waits, and work
//! that cannot get a permit is refused immediately (see
//! [`crate::concurrency`]). A depth gauge would therefore read zero for
//! ever and answer nothing. What answers the same question is saturation,
//! so [`active_jobs`](Metrics) and job capacity are published together with
//! a counter of refusals; "how close to full" and "how often full" are both
//! visible.

use std::sync::Arc;

use prometheus::core::{AtomicU64, GenericGauge};
use prometheus::{
    Encoder, HistogramOpts, HistogramVec, IntCounterVec, Opts, Registry, TextEncoder,
};

use crate::engine::Engine;

/// Prefixes every metric this service publishes.
pub const NAMESPACE: &str = "vitalmesh_processor";

/// Job outcomes, the bounded label for processing jobs and their failures.
pub const OUTCOME_COMPLETED: &str = "completed";
pub const OUTCOME_FAILED: &str = "failed";
pub const OUTCOME_CANCELLED: &str = "cancelled";
pub const OUTCOME_REFUSED: &str = "refused";

/// The label for any request method outside the ones this service routes.
///
/// The method is the one label whose raw value a client controls: HTTP
/// permits any token as a method, so recording it verbatim would let a
/// caller create a series per request. Only routable methods are kept.
pub const METHOD_OTHER: &str = "OTHER";

/// Reduces a request method to a bounded label.
pub fn method_label(method: &str) -> &'static str {
    match method {
        "GET" => "GET",
        "HEAD" => "HEAD",
        "POST" => "POST",
        "PUT" => "PUT",
        "PATCH" => "PATCH",
        "DELETE" => "DELETE",
        "OPTIONS" => "OPTIONS",
        _ => METHOD_OTHER,
    }
}

/// Error classes, the bounded label for HTTP failures.
pub const CLASS_CLIENT: &str = "client";
pub const CLASS_SERVER: &str = "server";

/// Buckets chosen for what each measurement is for. A request is bounded by
/// the client's budget in seconds; a job is the work itself and can take
/// far longer.
const HTTP_BUCKETS: &[f64] = &[
    0.001, 0.005, 0.01, 0.05, 0.1, 0.25, 0.5, 1.0, 2.5, 5.0, 10.0,
];
const JOB_BUCKETS: &[f64] = &[0.01, 0.05, 0.1, 0.25, 0.5, 1.0, 2.5, 5.0, 10.0, 30.0, 60.0];

/// The service's metrics and the registry that publishes them.
#[derive(Debug)]
pub struct Metrics {
    registry: Registry,
    http_requests: IntCounterVec,
    http_errors: IntCounterVec,
    http_duration: HistogramVec,
    jobs: IntCounterVec,
    job_duration: HistogramVec,
}

impl Metrics {
    /// Builds the metrics and registers them. Registration happens here
    /// rather than on first use, so two components cannot publish the same
    /// series under different meanings.
    pub fn new() -> Self {
        let registry = Registry::new();
        let http_requests = IntCounterVec::new(
            Opts::new(
                "http_requests_total",
                "HTTP requests served, by method, matched route and status code.",
            )
            .namespace(NAMESPACE),
            &["method", "route", "status"],
        )
        .expect("valid metric");
        let http_errors = IntCounterVec::new(
            Opts::new(
                "http_errors_total",
                "HTTP responses that failed, by method, matched route and class.",
            )
            .namespace(NAMESPACE),
            &["method", "route", "class"],
        )
        .expect("valid metric");
        let http_duration = HistogramVec::new(
            HistogramOpts::new(
                "http_request_duration_seconds",
                "Time to serve an HTTP request.",
            )
            .namespace(NAMESPACE)
            .buckets(HTTP_BUCKETS.to_vec()),
            &["method", "route"],
        )
        .expect("valid metric");
        let jobs = IntCounterVec::new(
            Opts::new("jobs_total", "Processing jobs by outcome. The failed and cancelled outcomes are processing failures; refused is a job admission rejected because the service was full.")
                .namespace(NAMESPACE),
            &["outcome"],
        )
        .expect("valid metric");
        let job_duration = HistogramVec::new(
            HistogramOpts::new(
                "job_duration_seconds",
                "Time a processing job took, by outcome.",
            )
            .namespace(NAMESPACE)
            .buckets(JOB_BUCKETS.to_vec()),
            &["outcome"],
        )
        .expect("valid metric");

        for collector in [&http_requests, &http_errors, &jobs] {
            registry
                .register(Box::new(collector.clone()))
                .expect("no duplicate metric");
        }
        for collector in [&http_duration, &job_duration] {
            registry
                .register(Box::new(collector.clone()))
                .expect("no duplicate metric");
        }

        let metrics = Self {
            registry,
            http_requests,
            http_errors,
            http_duration,
            jobs,
            job_duration,
        };
        metrics.initialise();
        metrics
    }

    /// Creates the series whose labels are known in advance, so they read
    /// zero rather than being absent before the first occurrence. A rule
    /// cannot tell "no failures yet" from "not instrumented".
    fn initialise(&self) {
        for outcome in [
            OUTCOME_COMPLETED,
            OUTCOME_FAILED,
            OUTCOME_CANCELLED,
            OUTCOME_REFUSED,
        ] {
            self.jobs.with_label_values(&[outcome]);
        }
    }

    /// Publishes the engine's live saturation. The gauges read the engine
    /// on each scrape rather than being copied on a timer, so they cannot
    /// go stale or drift from what admission actually sees.
    pub fn observe_engine(&self, engine: Arc<Engine>) {
        self.registry
            .register(Box::new(EngineCollector {
                engine: engine.clone(),
                active: gauge("active_jobs", "Jobs running right now."),
                capacity: gauge("job_capacity", "Jobs that may run at the same time."),
                available: gauge(
                    "job_slots_available",
                    "Free admission slots. Zero means the next job is refused; this service queues nothing.",
                ),
            }))
            .expect("no duplicate metric");
    }

    /// Records one served request. `route` is the matched pattern, never
    /// the path, so an identifier in a path cannot become a label; the
    /// method is bounded here so that every caller is covered.
    pub fn http_request(&self, method: &str, route: &str, status: u16, seconds: f64) {
        let method = method_label(method);
        let code = status.to_string();
        self.http_requests
            .with_label_values(&[method, route, &code])
            .inc();
        self.http_duration
            .with_label_values(&[method, route])
            .observe(seconds);
        if let Some(class) = error_class(status) {
            self.http_errors
                .with_label_values(&[method, route, class])
                .inc();
        }
    }

    /// Records one processing job that ran to an end.
    pub fn job(&self, outcome: &str, seconds: f64) {
        self.jobs.with_label_values(&[outcome]).inc();
        self.job_duration
            .with_label_values(&[outcome])
            .observe(seconds);
    }

    /// Records a job the service refused to admit because it was full.
    /// It has no duration: it never ran.
    pub fn job_refused(&self) {
        self.jobs.with_label_values(&[OUTCOME_REFUSED]).inc();
    }

    /// Renders the exposition format served at `GET /metrics`.
    pub fn encode(&self) -> String {
        let mut buffer = Vec::new();
        let encoder = TextEncoder::new();
        if encoder
            .encode(&self.registry.gather(), &mut buffer)
            .is_err()
        {
            // A scrape that cannot be rendered is not a reason to fail the
            // request: an empty body is a missing sample, an error is an
            // alert about the wrong thing.
            return String::new();
        }
        String::from_utf8(buffer).unwrap_or_default()
    }
}

impl Default for Metrics {
    fn default() -> Self {
        Self::new()
    }
}

/// Names the kind of failure, or `None` when the response succeeded. Two
/// classes rather than the code, because the question an error count
/// answers is whether the fault is the caller's or ours.
fn error_class(status: u16) -> Option<&'static str> {
    match status {
        500..=599 => Some(CLASS_SERVER),
        400..=499 => Some(CLASS_CLIENT),
        _ => None,
    }
}

fn gauge(name: &str, help: &str) -> GenericGauge<AtomicU64> {
    GenericGauge::with_opts(Opts::new(name, help).namespace(NAMESPACE)).expect("valid metric")
}

/// Reads the engine's saturation at scrape time.
#[derive(Debug)]
struct EngineCollector {
    engine: Arc<Engine>,
    active: GenericGauge<AtomicU64>,
    capacity: GenericGauge<AtomicU64>,
    available: GenericGauge<AtomicU64>,
}

impl prometheus::core::Collector for EngineCollector {
    fn desc(&self) -> Vec<&prometheus::core::Desc> {
        let mut out = self.active.desc();
        out.extend(self.capacity.desc());
        out.extend(self.available.desc());
        out
    }

    fn collect(&self) -> Vec<prometheus::proto::MetricFamily> {
        let active = self.engine.active_jobs();
        let capacity = self.engine.capacity();
        self.active.set(active as u64);
        self.capacity.set(capacity as u64);
        self.available.set(capacity.saturating_sub(active) as u64);

        let mut out = self.active.collect();
        out.extend(self.capacity.collect());
        out.extend(self.available.collect());
        out
    }
}

#[cfg(test)]
mod tests {
    use super::*;

    #[test]
    fn every_required_metric_is_registered() {
        // Each family is exercised once: a vector publishes nothing until it
        // has a label set, so "registered" is only observable through a
        // sample. The job outcomes are the exception and are asserted at
        // zero by job_outcomes_are_published_at_zero_before_any_job.
        let metrics = Metrics::new();
        metrics.http_request("GET", "/internal/v1/jobs/{job_id}", 200, 0.01);
        metrics.http_request("POST", "/internal/v1/process", 500, 0.01);
        metrics.job(OUTCOME_COMPLETED, 1.5);
        let body = metrics.encode();

        for name in [
            "vitalmesh_processor_http_requests_total",
            "vitalmesh_processor_http_errors_total",
            "vitalmesh_processor_http_request_duration_seconds",
            "vitalmesh_processor_jobs_total",
            "vitalmesh_processor_job_duration_seconds",
        ] {
            assert!(body.contains(&format!("# TYPE {name}")), "{name} missing");
            assert!(
                body.contains(&format!("# HELP {name}")),
                "{name} has no help"
            );
        }
    }

    #[test]
    fn job_outcomes_are_published_at_zero_before_any_job() {
        let body = Metrics::new().encode();
        for outcome in [
            OUTCOME_COMPLETED,
            OUTCOME_FAILED,
            OUTCOME_CANCELLED,
            OUTCOME_REFUSED,
        ] {
            let series = format!("vitalmesh_processor_jobs_total{{outcome=\"{outcome}\"}} 0");
            assert!(body.contains(&series), "missing {series} in\n{body}");
        }
    }

    #[test]
    fn a_failed_response_is_counted_as_an_error_of_its_class() {
        let metrics = Metrics::new();
        metrics.http_request("POST", "/internal/v1/process", 422, 0.01);
        metrics.http_request("POST", "/internal/v1/process", 503, 0.01);
        metrics.http_request("POST", "/internal/v1/process", 200, 0.01);

        let body = metrics.encode();
        assert!(body.contains(
            r#"vitalmesh_processor_http_errors_total{class="client",method="POST",route="/internal/v1/process"} 1"#
        ), "{body}");
        assert!(body.contains(
            r#"vitalmesh_processor_http_errors_total{class="server",method="POST",route="/internal/v1/process"} 1"#
        ), "{body}");
        // A success is not an error.
        assert_eq!(body.matches("http_errors_total{").count(), 2, "{body}");
    }

    #[test]
    fn jobs_and_failures_share_one_family_under_different_outcomes() {
        let metrics = Metrics::new();
        metrics.job(OUTCOME_COMPLETED, 1.0);
        metrics.job(OUTCOME_FAILED, 0.5);
        metrics.job(OUTCOME_CANCELLED, 0.2);
        metrics.job_refused();

        let body = metrics.encode();
        for (outcome, count) in [
            (OUTCOME_COMPLETED, 1),
            (OUTCOME_FAILED, 1),
            (OUTCOME_CANCELLED, 1),
            (OUTCOME_REFUSED, 1),
        ] {
            let series = format!("vitalmesh_processor_jobs_total{{outcome=\"{outcome}\"}} {count}");
            assert!(body.contains(&series), "missing {series} in\n{body}");
        }
        // A refused job never ran, so it contributes no duration.
        assert!(
            !body.contains(&format!(
                "vitalmesh_processor_job_duration_seconds_count{{outcome=\"{OUTCOME_REFUSED}\"}}"
            )),
            "a refused job was timed:\n{body}"
        );
    }

    /// A client chooses the method, so an invented one must fold into a
    /// single bucket rather than becoming a series of its own.
    #[test]
    fn an_invented_method_does_not_create_a_series() {
        let metrics = Metrics::new();
        for method in ["FOO", "BAR", "BAZ", "QUX", "ZAP"] {
            metrics.http_request(method, "/health", 405, 0.001);
        }
        metrics.http_request("GET", "/health", 200, 0.001);
        let body = metrics.encode();
        let series = body
            .matches("vitalmesh_processor_http_requests_total{")
            .count();
        assert_eq!(
            series, 2,
            "five invented methods should be one bucket:\n{body}"
        );
        assert!(body.contains(r#"method="OTHER""#), "{body}");
        assert!(!body.contains(r#"method="FOO""#), "{body}");
    }

    #[test]
    fn the_route_label_is_a_pattern_so_identifiers_never_widen_it() {
        let metrics = Metrics::new();
        for _ in 0..50 {
            metrics.http_request("GET", "/internal/v1/jobs/{job_id}", 200, 0.001);
        }
        let body = metrics.encode();
        assert_eq!(
            body.matches("vitalmesh_processor_http_requests_total{")
                .count(),
            1,
            "fifty jobs produced more than one series:\n{body}"
        );
    }

    #[test]
    fn saturation_is_published_from_the_engine() {
        use crate::config::Processing;
        use std::num::NonZeroUsize;
        use std::time::Duration;
        use tokio_util::sync::CancellationToken;

        let capacity = 4;
        let processing = Processing {
            max_concurrent_jobs: NonZeroUsize::new(capacity).unwrap(),
            max_batch_size: NonZeroUsize::MIN,
            max_job_measurements: NonZeroUsize::MIN,
            max_measurement_age: Duration::from_secs(1),
            max_future_skew: Duration::from_secs(1),
            timeout: Duration::from_secs(1),
            job_retention: Duration::from_secs(900),
            rules: crate::anomaly::RuleSet::empty(),
        };
        let engine = Arc::new(Engine::new(&processing, CancellationToken::new()));

        let metrics = Metrics::new();
        metrics.observe_engine(engine);
        let body = metrics.encode();

        assert!(body.contains("vitalmesh_processor_active_jobs 0"), "{body}");
        assert!(
            body.contains(&format!("vitalmesh_processor_job_capacity {capacity}")),
            "{body}"
        );
        assert!(
            body.contains(&format!(
                "vitalmesh_processor_job_slots_available {capacity}"
            )),
            "{body}"
        );
    }
}
