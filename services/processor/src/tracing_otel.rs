//! Distributed tracing (SPECIFICATIONS.md section 40).
//!
//! This service is the far end of a trace that starts in a client and
//! passes through the gateway. What makes it one trace rather than two is
//! W3C trace context: the gateway injects `traceparent`, this service
//! extracts it, and the spans it opens become children of the gateway's.
//!
//! # Failing gracefully
//!
//! A telemetry backend is never on the critical path. Spans are handed to a
//! batch processor and exported on its own task, so a collector that is
//! slow, refused or absent costs a request nothing. With no endpoint
//! configured the service still reads and honours incoming trace context,
//! and still puts the trace id in its logs; what it does not do is export.
//!
//! # What a span may carry
//!
//! Safe operational metadata only: the route pattern, the method, the
//! status, the job id, counts and durations. Never a reading, never a
//! payload, never the internal token. A span leaves this process for a
//! backend that is not the record's custodian.

use std::collections::HashMap;

use opentelemetry::global;
use opentelemetry::propagation::{Extractor, TextMapPropagator};
use opentelemetry::trace::TracerProvider as _;
use opentelemetry_otlp::WithExportConfig;
use opentelemetry_sdk::Resource;
use opentelemetry_sdk::propagation::TraceContextPropagator;
use opentelemetry_sdk::trace::SdkTracerProvider;
use tracing_subscriber::Layer;
use tracing_subscriber::registry::LookupSpan;

use crate::config::Tracing;
use crate::error::Error;

/// The W3C header carrying trace context between services.
pub const TRACEPARENT_HEADER: &str = "traceparent";
/// The optional companion header carrying vendor state.
pub const TRACESTATE_HEADER: &str = "tracestate";

/// Installs the W3C propagator. It is done whether or not spans are
/// exported, because propagation and export are separate concerns: a trace
/// must survive this hop even when this service records nothing.
pub fn install_propagator() {
    global::set_text_map_propagator(TraceContextPropagator::new());
}

/// Builds the tracing layer, or `None` when no collector is configured.
///
/// The provider is returned alongside so that the caller can flush it on
/// shutdown; dropping it without flushing loses whatever was recorded but
/// last.
pub fn layer<S>(
    cfg: &Tracing,
    service: crate::telemetry::ServiceInfo,
) -> Result<Option<(impl Layer<S>, SdkTracerProvider)>, Error>
where
    S: tracing::Subscriber + for<'a> LookupSpan<'a>,
{
    if !cfg.enabled() {
        return Ok(None);
    }

    let exporter = opentelemetry_otlp::SpanExporter::builder()
        .with_http()
        .with_endpoint(cfg.endpoint.clone())
        .with_timeout(cfg.timeout)
        .build()
        .map_err(Error::internal)?;

    let provider = SdkTracerProvider::builder()
        // Batching is what keeps export off the request path; redaction
        // sits between the batch and the wire.
        .with_batch_exporter(RedactingExporter::new(exporter))
        .with_resource(
            Resource::builder()
                .with_service_name(service.name)
                .with_attributes([
                    opentelemetry::KeyValue::new("service.version", service.version),
                    opentelemetry::KeyValue::new("deployment.environment", service.environment),
                ])
                .build(),
        )
        .build();

    let tracer = provider.tracer(service.name);
    global::set_tracer_provider(provider.clone());
    // Probe spans are never exported. A liveness probe every few seconds
    // per replica would outnumber real traffic in any trace store and bury
    // the traces that matter; probes stay visible in metrics and logs.
    let layer = tracing_opentelemetry::layer()
        .with_tracer(tracer)
        .with_filter(tracing_subscriber::filter::filter_fn(|meta| {
            meta.name() != PROBE_SPAN
        }));
    Ok(Some((layer, provider)))
}

/// The name of the span a probe request runs under. It exists so that the
/// exporter filter can drop probes by name, which is the one thing a
/// filter can see about a span before its fields are recorded.
pub const PROBE_SPAN: &str = "probe";

/// Whether a path is one the platform polls rather than a client calls.
pub fn is_probe(path: &str) -> bool {
    matches!(path, "/health" | "/ready" | "/metrics")
}

/// An exporter that applies [`crate::redact`] to every span before handing
/// it on. Log records are redacted by the JSON formatter, but the
/// OpenTelemetry layer reads span fields directly and would export them
/// untouched; this is the equivalent gate on the trace exit. Attribute
/// names are checked the same way log field names are, and every string
/// value is scrubbed for token shapes and bounded in length, on the span's
/// attributes and on each event's.
#[derive(Debug)]
pub struct RedactingExporter<E> {
    inner: E,
}

impl<E> RedactingExporter<E> {
    pub fn new(inner: E) -> Self {
        Self { inner }
    }
}

impl<E: opentelemetry_sdk::trace::SpanExporter> opentelemetry_sdk::trace::SpanExporter
    for RedactingExporter<E>
{
    fn export(
        &self,
        mut batch: Vec<opentelemetry_sdk::trace::SpanData>,
    ) -> impl std::future::Future<Output = opentelemetry_sdk::error::OTelSdkResult> + Send {
        for span in &mut batch {
            redact_span(span);
        }
        self.inner.export(batch)
    }

    fn shutdown_with_timeout(
        &self,
        timeout: std::time::Duration,
    ) -> opentelemetry_sdk::error::OTelSdkResult {
        self.inner.shutdown_with_timeout(timeout)
    }

    fn force_flush(&self) -> opentelemetry_sdk::error::OTelSdkResult {
        self.inner.force_flush()
    }

    fn set_resource(&mut self, resource: &Resource) {
        self.inner.set_resource(resource);
    }
}

/// Applies the redaction rules to one span in place.
pub fn redact_span(span: &mut opentelemetry_sdk::trace::SpanData) {
    for attribute in &mut span.attributes {
        redact_attribute(attribute);
    }
    for event in &mut span.events.events {
        if let Some(clean) = crate::redact::scrub(&event.name) {
            event.name = std::borrow::Cow::Owned(clean);
        }
        for attribute in &mut event.attributes {
            redact_attribute(attribute);
        }
    }
}

fn redact_attribute(attribute: &mut opentelemetry::KeyValue) {
    use opentelemetry::Value;
    if crate::redact::is_sensitive(attribute.key.as_str()) {
        attribute.value = Value::String(crate::redact::REDACTED.into());
        return;
    }
    let Value::String(text) = &attribute.value else {
        return;
    };
    if let Some(clean) = crate::redact::scrub(text.as_str()) {
        attribute.value = Value::String(clean.into());
    }
}

/// Reads W3C trace context out of request headers into an OpenTelemetry
/// context, so that a span opened afterwards continues the caller's trace.
pub fn context_from_headers(headers: &axum::http::HeaderMap) -> opentelemetry::Context {
    let carrier = HeaderCarrier(headers);
    TraceContextPropagator::new().extract(&carrier)
}

/// Reads the trace id out of request headers without needing a subscriber,
/// so it can be attached to a request span as a field and appear in logs
/// even when nothing is exported.
pub fn trace_id_from_headers(headers: &axum::http::HeaderMap) -> Option<String> {
    use opentelemetry::trace::TraceContextExt;
    let context = context_from_headers(headers);
    let span = context.span();
    let id = span.span_context().trace_id();
    if span.span_context().is_valid() {
        Some(id.to_string())
    } else {
        None
    }
}

/// Adapts axum's headers to the propagator's reader.
struct HeaderCarrier<'a>(&'a axum::http::HeaderMap);

impl Extractor for HeaderCarrier<'_> {
    fn get(&self, key: &str) -> Option<&str> {
        self.0.get(key).and_then(|value| value.to_str().ok())
    }

    fn keys(&self) -> Vec<&str> {
        self.0.keys().map(|name| name.as_str()).collect()
    }
}

/// The headers a client may use to continue a trace, for tests and
/// documentation. A map rather than a struct because that is what a
/// propagator produces.
pub fn traceparent(trace_id: &str, span_id: &str, sampled: bool) -> HashMap<String, String> {
    let flags = if sampled { "01" } else { "00" };
    HashMap::from([(
        TRACEPARENT_HEADER.to_owned(),
        format!("00-{trace_id}-{span_id}-{flags}"),
    )])
}

#[cfg(test)]
mod tests {
    use super::*;
    use axum::http::{HeaderMap, HeaderValue};

    const TRACE_ID: &str = "4bf92f3577b34da6a3ce929d0e0e4736";
    const SPAN_ID: &str = "00f067aa0ba902b7";

    fn headers(value: &str) -> HeaderMap {
        let mut map = HeaderMap::new();
        map.insert(TRACEPARENT_HEADER, HeaderValue::from_str(value).unwrap());
        map
    }

    #[test]
    fn a_w3c_traceparent_is_read_back() {
        let map = headers(&format!("00-{TRACE_ID}-{SPAN_ID}-01"));
        assert_eq!(trace_id_from_headers(&map).as_deref(), Some(TRACE_ID));
    }

    #[test]
    fn a_request_without_trace_context_starts_no_trace_of_its_own_here() {
        assert_eq!(trace_id_from_headers(&HeaderMap::new()), None);
    }

    #[test]
    fn a_malformed_traceparent_is_ignored_rather_than_trusted() {
        for value in [
            "not-a-traceparent",
            "00-tooshort-00f067aa0ba902b7-01",
            // An all-zero trace id is the spec's "no trace".
            "00-00000000000000000000000000000000-00f067aa0ba902b7-01",
        ] {
            assert_eq!(
                trace_id_from_headers(&headers(value)),
                None,
                "{value} was accepted"
            );
        }
    }

    /// The W3C specification requires a version it does not know to be
    /// parsed rather than discarded, so that a future version does not
    /// break the trace as it passes through an older service.
    #[test]
    fn a_future_version_is_accepted_rather_than_breaking_the_trace() {
        let map = headers(&format!("99-{TRACE_ID}-{SPAN_ID}-01"));
        assert_eq!(trace_id_from_headers(&map).as_deref(), Some(TRACE_ID));
    }

    /// Builds a span as the SDK would hand it to an exporter.
    fn span_with(attributes: Vec<opentelemetry::KeyValue>) -> opentelemetry_sdk::trace::SpanData {
        use opentelemetry::trace::{
            SpanContext, SpanId, SpanKind, Status, TraceFlags, TraceId, TraceState,
        };
        opentelemetry_sdk::trace::SpanData {
            span_context: SpanContext::new(
                TraceId::from(1_u128),
                SpanId::from(1_u64),
                TraceFlags::SAMPLED,
                false,
                TraceState::default(),
            ),
            parent_span_id: SpanId::INVALID,
            parent_span_is_remote: false,
            span_kind: SpanKind::Server,
            name: "request".into(),
            start_time: std::time::SystemTime::now(),
            end_time: std::time::SystemTime::now(),
            attributes,
            dropped_attributes_count: 0,
            events: opentelemetry_sdk::trace::SpanEvents::default(),
            links: opentelemetry_sdk::trace::SpanLinks::default(),
            status: Status::Unset,
            instrumentation_scope: Default::default(),
        }
    }

    fn value_of(span: &opentelemetry_sdk::trace::SpanData, key: &str) -> String {
        span.attributes
            .iter()
            .find(|kv| kv.key.as_str() == key)
            .map(|kv| kv.value.to_string())
            .unwrap_or_default()
    }

    /// The same rules the log formatter applies, at the trace exit: a span
    /// leaves the process for a backend that is not the record's custodian.
    #[test]
    fn exported_spans_are_redacted_by_name_and_by_shape() {
        use opentelemetry::KeyValue;
        let token = crate::redact::fake_jwt();
        let mut span = span_with(vec![
            KeyValue::new("token", "shared-secret"),
            KeyValue::new("internal_token", "also-secret"),
            KeyValue::new("message", format!("rejected {token}")),
            KeyValue::new("detail", "A".repeat(crate::redact::MAX_VALUE_LENGTH * 2)),
            KeyValue::new("job_id", "job-1"),
            KeyValue::new("readings", 5000_i64),
        ]);
        redact_span(&mut span);

        assert_eq!(value_of(&span, "token"), crate::redact::REDACTED);
        assert_eq!(value_of(&span, "internal_token"), crate::redact::REDACTED);
        assert!(
            !value_of(&span, "message").contains(&token),
            "a token survived"
        );
        assert!(
            value_of(&span, "message").contains("rejected"),
            "the text around it was lost"
        );
        assert!(
            value_of(&span, "detail").contains("more bytes omitted"),
            "an oversized value was exported whole"
        );
        // What makes a span useful is untouched.
        assert_eq!(value_of(&span, "job_id"), "job-1");
        assert_eq!(value_of(&span, "readings"), "5000");
    }

    /// An error recorded on a span becomes an event whose attributes carry
    /// the message; those must be scrubbed like any other value.
    #[test]
    fn exported_span_events_are_redacted_too() {
        use opentelemetry::KeyValue;
        let token = crate::redact::fake_jwt();
        let mut span = span_with(vec![]);
        span.events.events.push(opentelemetry::trace::Event::new(
            format!("exception {token}"),
            std::time::SystemTime::now(),
            vec![KeyValue::new(
                "exception.message",
                format!("bad token {token}"),
            )],
            0,
        ));
        redact_span(&mut span);

        let event = &span.events.events[0];
        assert!(
            !event.name.contains(&token),
            "a token survived in the event name"
        );
        let message = event.attributes[0].value.to_string();
        assert!(
            !message.contains(&token),
            "a token survived in the event: {message}"
        );
    }

    #[test]
    fn probes_are_the_platform_endpoints_and_nothing_else() {
        for path in ["/health", "/ready", "/metrics"] {
            assert!(is_probe(path), "{path}");
        }
        for path in [
            "/internal/v1/health",
            "/internal/v1/process",
            "/internal/v1/jobs/x",
            "/",
        ] {
            assert!(!is_probe(path), "{path}");
        }
    }

    #[test]
    fn the_sampled_flag_is_carried_in_the_header_this_module_builds() {
        let sampled = traceparent(TRACE_ID, SPAN_ID, true);
        assert!(sampled[TRACEPARENT_HEADER].ends_with("-01"));
        let unsampled = traceparent(TRACE_ID, SPAN_ID, false);
        assert!(unsampled[TRACEPARENT_HEADER].ends_with("-00"));
        // And what this module builds is what it reads.
        let map = headers(&sampled[TRACEPARENT_HEADER]);
        assert_eq!(trace_id_from_headers(&map).as_deref(), Some(TRACE_ID));
    }
}
