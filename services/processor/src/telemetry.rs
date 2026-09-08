//! Structured logging. JSON records carry the repository's schema fields
//! (`timestamp`, `level`, `service`, `version`, `environment`, `message`)
//! plus the fields of every enclosing span, so an event logged inside a
//! request span carries `request_id` and `correlation_id` at the top level.
//!
//! Every record passes through [`crate::redact`] before it is written, so
//! a credential or a payload logged by mistake never leaves the process
//! (SPECIFICATIONS.md section 41).

use serde_json::{Map, Value};
use tracing::field::{Field, Visit};
use tracing::{Event, Subscriber};
use tracing_subscriber::filter::LevelFilter;
use tracing_subscriber::fmt::format::{JsonFields, Writer};
use tracing_subscriber::fmt::time::{FormatTime, SystemTime};
use tracing_subscriber::fmt::{self, FmtContext, FormatEvent, FormatFields, FormattedFields};
use tracing_subscriber::layer::{Layer, SubscriberExt};
use tracing_subscriber::registry::LookupSpan;
use tracing_subscriber::util::SubscriberInitExt;

use opentelemetry_sdk::trace::SdkTracerProvider;

use crate::config::{Log, LogFormat};
use crate::error::Error;
use crate::redact;

/// Identity of the running process, attached to every record.
#[derive(Debug, Clone, Copy)]
pub struct ServiceInfo {
    pub name: &'static str,
    pub version: &'static str,
    pub environment: &'static str,
}

/// Installs the global subscriber and, when a collector is configured, the
/// OpenTelemetry layer beside it. Fails if a subscriber is already
/// installed.
///
/// The W3C propagator is installed either way: propagation and export are
/// separate concerns, and a trace must cross this service even when it
/// records nothing of its own.
///
/// Returns the tracer provider when one was built, so the caller can flush
/// it on shutdown.
pub fn init(
    log: &Log,
    tracing_cfg: &crate::config::Tracing,
    info: ServiceInfo,
) -> Result<Option<SdkTracerProvider>, Error> {
    crate::tracing_otel::install_propagator();
    let filter = LevelFilter::from_level(log.level);
    let (traces, provider) = match crate::tracing_otel::layer(tracing_cfg, info)? {
        Some((layer, provider)) => (Some(layer), Some(provider)),
        None => (None, None),
    };

    let result = match log.format {
        LogFormat::Json => tracing_subscriber::registry()
            .with(traces)
            .with(
                fmt::layer()
                    .event_format(JsonFormat::new(info))
                    .fmt_fields(JsonFields::new())
                    .with_filter(filter),
            )
            .try_init(),
        LogFormat::Text => tracing_subscriber::registry()
            .with(traces)
            .with(fmt::layer().with_target(false).with_filter(filter))
            .try_init(),
    };
    result.map_err(Error::internal)?;
    Ok(provider)
}

/// Event formatter producing one flat JSON object per line. Use it with
/// [`JsonFields`] so that span fields can be merged into the record.
#[derive(Debug, Clone, Copy)]
pub struct JsonFormat {
    info: ServiceInfo,
}

impl JsonFormat {
    pub fn new(info: ServiceInfo) -> Self {
        Self { info }
    }
}

impl<S, N> FormatEvent<S, N> for JsonFormat
where
    S: Subscriber + for<'a> LookupSpan<'a>,
    N: for<'a> FormatFields<'a> + 'static,
{
    fn format_event(
        &self,
        ctx: &FmtContext<'_, S, N>,
        mut writer: Writer<'_>,
        event: &Event<'_>,
    ) -> std::fmt::Result {
        let mut record = Map::new();

        let mut timestamp = String::new();
        SystemTime.format_time(&mut Writer::new(&mut timestamp))?;
        record.insert("timestamp".into(), Value::String(timestamp));
        record.insert(
            "level".into(),
            Value::String(event.metadata().level().to_string()),
        );
        record.insert("service".into(), Value::String(self.info.name.into()));
        record.insert("version".into(), Value::String(self.info.version.into()));
        record.insert(
            "environment".into(),
            Value::String(self.info.environment.into()),
        );

        // Span fields, outermost first so inner spans override outer ones.
        if let Some(scope) = ctx.event_scope() {
            for span in scope.from_root() {
                let extensions = span.extensions();
                let Some(fields) = extensions.get::<FormattedFields<N>>() else {
                    continue;
                };
                if let Ok(Value::Object(map)) = serde_json::from_str::<Value>(&fields.fields) {
                    record.extend(map);
                }
            }
        }

        event.record(&mut JsonVisitor(&mut record));

        // Last gate before the record leaves the process. It runs over the
        // whole map, so span fields merged above are covered as well as the
        // event's own.
        redact::record(&mut record);

        let line = serde_json::to_string(&Value::Object(record)).map_err(|_| std::fmt::Error)?;
        writeln!(writer, "{line}")
    }
}

struct JsonVisitor<'a>(&'a mut Map<String, Value>);

impl JsonVisitor<'_> {
    fn insert(&mut self, field: &Field, value: Value) {
        self.0.insert(field.name().to_owned(), value);
    }
}

impl Visit for JsonVisitor<'_> {
    fn record_debug(&mut self, field: &Field, value: &dyn std::fmt::Debug) {
        self.insert(field, Value::String(format!("{value:?}")));
    }

    fn record_str(&mut self, field: &Field, value: &str) {
        self.insert(field, Value::String(value.to_owned()));
    }

    fn record_i64(&mut self, field: &Field, value: i64) {
        self.insert(field, Value::from(value));
    }

    fn record_u64(&mut self, field: &Field, value: u64) {
        self.insert(field, Value::from(value));
    }

    fn record_f64(&mut self, field: &Field, value: f64) {
        self.insert(field, Value::from(value));
    }

    fn record_bool(&mut self, field: &Field, value: bool) {
        self.insert(field, Value::Bool(value));
    }

    fn record_error(&mut self, field: &Field, value: &(dyn std::error::Error + 'static)) {
        self.insert(field, Value::String(value.to_string()));
    }
}

#[cfg(test)]
mod tests {
    use super::*;
    use std::io;
    use std::sync::{Arc, Mutex};

    #[derive(Clone, Default)]
    struct Sink(Arc<Mutex<Vec<u8>>>);

    impl io::Write for Sink {
        fn write(&mut self, buf: &[u8]) -> io::Result<usize> {
            self.0.lock().unwrap().extend_from_slice(buf);
            Ok(buf.len())
        }

        fn flush(&mut self) -> io::Result<()> {
            Ok(())
        }
    }

    fn capture(emit: impl FnOnce()) -> Vec<Value> {
        let sink = Sink::default();
        let writer_sink = sink.clone();
        let subscriber = tracing_subscriber::registry().with(
            fmt::layer()
                .event_format(JsonFormat::new(ServiceInfo {
                    name: "processor",
                    version: "abc123",
                    environment: "test",
                }))
                .fmt_fields(JsonFields::new())
                .with_writer(move || writer_sink.clone()),
        );
        tracing::subscriber::with_default(subscriber, emit);

        let bytes = sink.0.lock().unwrap().clone();
        String::from_utf8(bytes)
            .unwrap()
            .lines()
            .map(|line| serde_json::from_str(line).expect("each line is JSON"))
            .collect()
    }

    #[test]
    fn records_are_flat_and_carry_schema_and_span_fields() {
        let lines = capture(|| {
            let span =
                tracing::info_span!("request", request_id = "req-1", correlation_id = "corr-1");
            let _guard = span.enter();
            tracing::info!(status = 200_u64, path = "/health", "request");
        });

        assert_eq!(lines.len(), 1);
        let rec = &lines[0];
        assert_eq!(rec["level"], "INFO");
        assert_eq!(rec["message"], "request");
        assert_eq!(rec["service"], "processor");
        assert_eq!(rec["version"], "abc123");
        assert_eq!(rec["environment"], "test");
        assert_eq!(rec["request_id"], "req-1");
        assert_eq!(rec["correlation_id"], "corr-1");
        assert_eq!(rec["status"], 200);
        assert_eq!(rec["path"], "/health");
        assert!(rec["timestamp"].as_str().unwrap().ends_with('Z'));
        assert!(
            rec.get("fields").is_none() && rec.get("span").is_none(),
            "record must be flat: {rec}"
        );
    }

    #[test]
    fn events_outside_spans_have_no_request_id() {
        let lines = capture(|| tracing::warn!(error = %io::Error::other("boom"), "plain"));
        assert_eq!(lines[0]["level"], "WARN");
        assert_eq!(lines[0]["error"], "boom");
        assert!(lines[0].get("request_id").is_none());
    }

    /// The formatter is the last thing to touch a record, so redaction has
    /// to happen there rather than at the call site: a field added by
    /// someone in a hurry is covered without their help.
    #[test]
    fn the_formatter_removes_credentials_from_event_fields() {
        let token = crate::redact::fake_jwt();
        let lines = capture(|| {
            tracing::warn!(
                token = %token,
                internal_token = "shared-secret",
                job_id = "job-1",
                readings = 5000_u64,
                "internal request rejected"
            );
        });

        let rec = &lines[0];
        assert_eq!(rec["token"], crate::redact::REDACTED);
        assert_eq!(rec["internal_token"], crate::redact::REDACTED);
        // What makes the record useful is untouched.
        assert_eq!(rec["job_id"], "job-1");
        assert_eq!(rec["readings"], 5000);
        assert!(
            !rec.to_string().contains("shared-secret"),
            "a secret survived: {rec}"
        );
    }

    /// Span fields are merged into the record from a separate formatter, so
    /// they are the path most likely to be missed.
    #[test]
    fn the_formatter_removes_credentials_from_span_fields() {
        let token = crate::redact::fake_jwt();
        let header = format!("Bearer {token}");
        let lines = capture(|| {
            let span =
                tracing::info_span!("request", request_id = "req-1", authorization = %header);
            let _guard = span.enter();
            tracing::info!("request");
        });

        let rec = &lines[0];
        assert_eq!(rec["request_id"], "req-1");
        assert_eq!(rec["authorization"], crate::redact::REDACTED);
        assert!(!rec.to_string().contains(&token), "a token survived: {rec}");
    }

    /// A token in a message, rather than in a field of its own, must go the
    /// same way.
    #[test]
    fn the_formatter_removes_a_token_from_a_message() {
        let token = crate::redact::fake_jwt();
        let lines = capture(|| tracing::error!("rejected {token}"));
        let rec = &lines[0];
        assert!(!rec.to_string().contains(&token), "a token survived: {rec}");
        assert!(rec["message"].as_str().unwrap().contains("rejected"));
    }

    /// A payload logged by mistake is bounded rather than stored whole.
    #[test]
    fn the_formatter_bounds_an_oversized_field() {
        let payload = "A".repeat(crate::redact::MAX_VALUE_LENGTH * 2);
        let lines = capture(|| tracing::info!(detail = %payload, "oops"));
        let got = lines[0]["detail"].as_str().unwrap();
        assert!(got.len() < payload.len(), "the payload was stored whole");
        assert!(got.contains("more bytes omitted"), "{got}");
    }
}
