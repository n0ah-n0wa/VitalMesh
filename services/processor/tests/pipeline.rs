//! Integration tests of complete processing flows: raw readings in, results
//! out, under the execution engine's limits.

use std::num::NonZeroUsize;
use std::sync::Arc;
use std::time::{Duration, Instant};

use tokio_util::sync::CancellationToken;

use processor::anomaly::{ALGORITHM_VERSION, Detector, RuleSet};
use processor::config::Processing;
use processor::domain::{
    AlgorithmVersion, JobId, MeasurementType, PatientId, Percentile, ProcessingJob,
    ProcessingParameters, Severity, Timestamp, TimestampBounds, Window,
};
use processor::engine::Engine;
use processor::error::Kind;
use processor::pipeline::{
    Control, Limits, Outcome, Pipeline, Processor, RawReading, RejectionCode, Request,
};

const REQUESTED_AT: &str = "2026-09-06T12:00:00Z";

fn rules() -> RuleSet {
    RuleSet::from_json(
        r#"{"rules": [
          {"name": "hr-bounds", "kind": "THRESHOLD", "measurement_types": ["HEART_RATE"],
           "tiers": {"warning": {"lower": 40, "upper": 180}, "critical": {"lower": 30, "upper": 220}}},
          {"name": "outliers", "kind": "Z_SCORE", "min_count": 5, "tiers": {"warning": 2.5, "critical": 3}}
        ]}"#,
    )
    .unwrap()
}

fn limits(max: usize) -> Limits {
    Limits {
        max_job_measurements: NonZeroUsize::new(max).unwrap(),
        timestamp_bounds: TimestampBounds {
            max_future_skew: Duration::from_secs(300),
            max_age: Duration::from_secs(30 * 24 * 3600),
        },
    }
}

fn pipeline(max: usize) -> Pipeline {
    Pipeline::new(Detector::new(rules()), limits(max), "v-test")
}

fn job(id: &str, windows: &[Window], version: AlgorithmVersion) -> ProcessingJob {
    ProcessingJob::new(
        JobId::parse(id).unwrap(),
        PatientId::parse("patient-1").unwrap(),
        ProcessingParameters::new(
            [MeasurementType::HeartRate, MeasurementType::Spo2],
            windows.iter().copied(),
            [Percentile::new(50).unwrap(), Percentile::new(95).unwrap()],
        )
        .unwrap(),
        version,
        Timestamp::parse(REQUESTED_AT).unwrap(),
    )
}

fn reading(id: &str, kind: &str, value: f64, unit: &str, at: &str) -> RawReading {
    RawReading {
        id: id.into(),
        measurement_type: kind.into(),
        value,
        unit: unit.into(),
        recorded_at: at.into(),
    }
}

/// Heart-rate readings every 10 seconds from 11:00, with one outlier.
fn heart_rate_series(n: usize) -> Vec<RawReading> {
    let base = Timestamp::parse("2026-09-06T11:00:00Z").unwrap();
    (0..n)
        .map(|i| {
            let at = base
                .checked_add(Duration::from_secs(i as u64 * 10))
                .unwrap();
            let value = if i == n / 2 {
                190.0
            } else {
                60.0 + (i % 7) as f64
            };
            reading(
                &format!("hr-{i:05}"),
                "HEART_RATE",
                value,
                "bpm",
                &at.to_rfc3339(),
            )
        })
        .collect()
}

fn engine(max_jobs: usize, timeout: Duration) -> (Arc<Engine>, CancellationToken) {
    let shutdown = CancellationToken::new();
    let processing = Processing {
        max_concurrent_jobs: NonZeroUsize::new(max_jobs).unwrap(),
        max_batch_size: NonZeroUsize::new(1000).unwrap(),
        max_job_measurements: NonZeroUsize::new(1_000_000).unwrap(),
        max_measurement_age: Duration::from_secs(3600),
        max_future_skew: Duration::from_secs(60),
        timeout,
        job_retention: Duration::from_secs(900),
        rules: RuleSet::empty(),
    };
    (
        Arc::new(Engine::new(&processing, shutdown.clone())),
        shutdown,
    )
}

#[test]
fn complete_flow_produces_results_with_statistics_anomalies_and_versions() {
    let mut readings = heart_rate_series(60);
    readings.push(reading("spo2-1", "SPO2", 97.0, "%", "2026-09-06T11:00:05Z"));
    readings.push(reading("spo2-2", "SPO2", 96.0, "%", "2026-09-06T11:00:15Z"));
    let request = Request {
        job: job(
            "job-1",
            &[Window::OneMinute, Window::OneHour],
            ALGORITHM_VERSION,
        ),
        readings,
    };
    let outcome = pipeline(10_000)
        .run(request, &Control::unbounded())
        .unwrap();

    assert_eq!(outcome.job_id.as_str(), "job-1");
    assert_eq!(outcome.algorithm_version, ALGORITHM_VERSION);
    assert_eq!(outcome.service_version, "v-test");
    assert_eq!(outcome.accepted, 62);
    assert_eq!(outcome.skipped, 0);
    assert!(outcome.rejected.is_empty());

    // 60 readings at 10 s intervals span 10 one-minute windows plus one
    // hour window for heart rate; SPO2 has one minute window and one hour.
    let hr_minutes = outcome
        .results
        .iter()
        .filter(|r| {
            r.measurement_type() == MeasurementType::HeartRate && r.window() == Window::OneMinute
        })
        .count();
    assert_eq!(hr_minutes, 10);
    let hour = outcome
        .results
        .iter()
        .find(|r| {
            r.measurement_type() == MeasurementType::HeartRate && r.window() == Window::OneHour
        })
        .expect("hour window");
    assert_eq!(hour.statistics().count(), 60);
    assert_eq!(hour.statistics().max().get(), 190.0);
    assert_eq!(
        hour.window_start(),
        Timestamp::parse("2026-09-06T11:00:00Z").unwrap()
    );
    assert_eq!(hour.job_id().as_str(), "job-1");
    assert_eq!(hour.algorithm_version(), ALGORITHM_VERSION);
    assert_eq!(hour.service_version(), "v-test");
    assert_eq!(hour.statistics().percentiles().len(), 2);

    // The outlier breaches the threshold rule (warning, upper 180) and the
    // z-score rule in the hour window; both anomalies carry the version.
    let threshold: Vec<_> = hour
        .anomalies()
        .iter()
        .filter(|a| a.metric == processor::domain::AnomalyMetric::Threshold)
        .collect();
    assert_eq!(threshold.len(), 1);
    assert_eq!(threshold[0].value.get(), 190.0);
    assert_eq!(threshold[0].threshold.get(), 180.0);
    assert_eq!(threshold[0].severity, Severity::Warning);
    assert!(
        hour.anomalies()
            .iter()
            .all(|a| a.algorithm_version == ALGORITHM_VERSION)
    );
    assert!(
        hour.anomalies()
            .iter()
            .any(|a| a.metric == processor::domain::AnomalyMetric::ZScore)
    );

    let spo2: Vec<_> = outcome
        .results
        .iter()
        .filter(|r| r.measurement_type() == MeasurementType::Spo2)
        .collect();
    assert_eq!(spo2.len(), 2, "one minute window and one hour window");
    assert!(spo2.iter().all(|r| r.anomalies().is_empty()));
    assert!(
        serde_json::to_string(&outcome)
            .unwrap()
            .contains("\"results\"")
    );
}

#[test]
fn partial_failures_are_reported_and_the_rest_is_processed() {
    let mut readings = heart_rate_series(20);
    readings[3].unit = "mmHg".into();
    readings[7].recorded_at = "not a time".into();
    readings[11].value = 999.0;
    readings.push(reading(
        "hr-00005",
        "HEART_RATE",
        61.0,
        "bpm",
        "2026-09-06T11:30:00Z",
    )); // duplicate id
    readings.push(reading(
        "temp",
        "BODY_TEMPERATURE",
        36.6,
        "C",
        "2026-09-06T11:00:00Z",
    )); // not requested
    let request = Request {
        job: job("job-2", &[Window::OneHour], ALGORITHM_VERSION),
        readings,
    };
    let outcome = pipeline(10_000)
        .run(request, &Control::unbounded())
        .unwrap();

    assert_eq!(
        outcome.accepted,
        20 - 3 - 1,
        "three invalid, both hr-00005 occurrences dropped"
    );
    assert_eq!(outcome.skipped, 1);
    let rejected: Vec<(usize, RejectionCode)> =
        outcome.rejected.iter().map(|r| (r.index, r.code)).collect();
    assert_eq!(
        rejected,
        [
            (3, RejectionCode::UnitMismatch),
            (5, RejectionCode::DuplicateId),
            (7, RejectionCode::InvalidTimestamp),
            (11, RejectionCode::ValueOutOfRange),
            (20, RejectionCode::DuplicateId),
        ]
    );
    assert_eq!(outcome.results.len(), 1);
    assert_eq!(outcome.results[0].statistics().count(), 16);
}

#[test]
fn whole_job_failures_are_explicit() {
    let p = pipeline(5);

    let none = Request {
        job: job("job-3", &[Window::OneHour], ALGORITHM_VERSION),
        readings: vec![reading(
            "x",
            "HEART_RATE",
            72.0,
            "beats",
            "2026-09-06T11:00:00Z",
        )],
    };
    let err = p.run(none, &Control::unbounded()).unwrap_err();
    assert_eq!(
        (err.kind(), err.code()),
        (Kind::Validation, "NO_VALID_MEASUREMENTS")
    );

    let empty = Request {
        job: job("job-4", &[Window::OneHour], ALGORITHM_VERSION),
        readings: vec![],
    };
    let err = p.run(empty, &Control::unbounded()).unwrap_err();
    assert_eq!(err.code(), "NO_VALID_MEASUREMENTS");

    let too_large = Request {
        job: job("job-5", &[Window::OneHour], ALGORITHM_VERSION),
        readings: heart_rate_series(6),
    };
    let err = p.run(too_large, &Control::unbounded()).unwrap_err();
    assert_eq!(
        (err.kind(), err.code()),
        (Kind::Validation, "JOB_TOO_LARGE")
    );

    let wrong_version = Request {
        job: job("job-6", &[Window::OneHour], AlgorithmVersion::new(0, 9, 0)),
        readings: heart_rate_series(3),
    };
    let err = p.run(wrong_version, &Control::unbounded()).unwrap_err();
    assert_eq!(
        (err.kind(), err.code()),
        (Kind::Validation, "UNSUPPORTED_ALGORITHM_VERSION")
    );
    assert!(err.message().contains("1.0.0"));
}

#[test]
fn output_is_deterministic_across_input_orders_and_runs() {
    let mut readings = heart_rate_series(500);
    readings.push(reading("spo2-1", "SPO2", 97.0, "%", "2026-09-06T11:00:05Z"));
    let p = pipeline(10_000);
    let run = |r: Vec<RawReading>| -> String {
        let outcome = p
            .run(
                Request {
                    job: job("job-7", &Window::ALL, ALGORITHM_VERSION),
                    readings: r,
                },
                &Control::unbounded(),
            )
            .unwrap();
        serde_json::to_string(&outcome).unwrap()
    };
    // The 500-reading series runs past the future-skew bound, so its tail
    // is rejected; rejections are keyed by input index, which legitimately
    // moves with the input order. Results must not.
    let outcome = |r: Vec<RawReading>| -> Outcome {
        p.run(
            Request {
                job: job("job-7", &Window::ALL, ALGORITHM_VERSION),
                readings: r,
            },
            &Control::unbounded(),
        )
        .unwrap()
    };
    let forward = outcome(readings.clone());
    let mut shuffled = readings.clone();
    shuffled.reverse();
    shuffled.rotate_left(97);
    let reordered = outcome(shuffled);
    assert!(
        !forward.rejected.is_empty(),
        "the series should exceed the bound"
    );
    assert_eq!(forward.results, reordered.results);
    assert_eq!(forward.accepted, reordered.accepted);
    let ids = |o: &Outcome| {
        let mut ids: Vec<Option<String>> = o.rejected.iter().map(|r| r.id.clone()).collect();
        ids.sort();
        ids
    };
    assert_eq!(ids(&forward), ids(&reordered));
    assert_eq!(
        forward,
        outcome(readings.clone()),
        "repeat runs are identical"
    );

    // Within the bounds the whole outcome, indices included, is identical.
    let in_bounds: Vec<RawReading> = readings.into_iter().take(300).collect();
    let mut shuffled = in_bounds.clone();
    shuffled.reverse();
    shuffled.rotate_left(41);
    let forward = run(in_bounds);
    assert_eq!(forward, run(shuffled));
    assert!(forward.contains("\"algorithm_version\":\"1.0.0\""));
    assert!(forward.contains("\"rejected\":[]"));
}

#[test]
fn control_stops_the_core_between_stages_and_windows() {
    let p = pipeline(1_000_000);
    let request = || Request {
        job: job("job-8", &Window::ALL, ALGORITHM_VERSION),
        readings: heart_rate_series(20_000),
    };

    let cancelled = CancellationToken::new();
    cancelled.cancel();
    let err = p
        .run(request(), &Control::new(cancelled, None))
        .unwrap_err();
    assert_eq!(err.kind(), Kind::Cancelled);

    let expired = Control::new(
        CancellationToken::new(),
        Some(Instant::now() - Duration::from_millis(1)),
    );
    let err = p.run(request(), &expired).unwrap_err();
    assert_eq!(err.kind(), Kind::Timeout);
}

#[tokio::test]
async fn processor_runs_jobs_under_the_engine_limits() {
    let (engine, _shutdown) = engine(1, Duration::from_secs(30));
    let processor = Processor::new(pipeline(1_000_000), engine);
    let request = Request {
        job: job(
            "job-9",
            &[Window::OneMinute, Window::OneHour],
            ALGORITHM_VERSION,
        ),
        readings: heart_rate_series(120),
    };
    let outcome: Outcome = processor.process(request).await.unwrap();
    assert_eq!(outcome.accepted, 120);
    assert_eq!(processor.engine().active_jobs(), 0);

    // A second job while one is running is refused, not queued.
    let slow = Request {
        job: job("job-10", &Window::ALL, ALGORITHM_VERSION),
        readings: heart_rate_series(200_000),
    };
    let first = tokio::spawn({
        let processor = processor.clone();
        async move { processor.process(slow).await }
    });
    tokio::time::sleep(Duration::from_millis(50)).await;
    let second = processor
        .process(Request {
            job: job("job-11", &[Window::OneHour], ALGORITHM_VERSION),
            readings: heart_rate_series(3),
        })
        .await;
    let err = second.unwrap_err();
    assert!(
        matches!(err.kind(), Kind::Overloaded) || first.is_finished(),
        "second job should be refused while the first runs: {err}"
    );
    first.await.unwrap().unwrap();
}

#[tokio::test]
async fn processor_cancels_a_running_job_promptly() {
    let (engine, _shutdown) = engine(2, Duration::from_secs(60));
    let processor = Processor::new(pipeline(1_000_000), engine);
    let handle = tokio::spawn({
        let processor = processor.clone();
        async move {
            processor
                .process(Request {
                    job: job("job-12", &Window::ALL, ALGORITHM_VERSION),
                    readings: heart_rate_series(300_000),
                })
                .await
        }
    });
    tokio::time::sleep(Duration::from_millis(100)).await;
    let started = Instant::now();
    assert!(processor.engine().cancel(&JobId::parse("job-12").unwrap()));
    let err = handle.await.unwrap().unwrap_err();
    assert_eq!(err.kind(), Kind::Cancelled);
    assert!(
        started.elapsed() < Duration::from_secs(5),
        "cancellation took {:?}",
        started.elapsed()
    );
    // The core observes its control at the next checkpoint and exits; the
    // engine forgets the job as soon as the caller's future ends.
    tokio::time::timeout(Duration::from_secs(5), processor.engine().drained())
        .await
        .expect("engine drains after cancellation");
}

#[tokio::test]
async fn processor_enforces_the_time_bound() {
    let (engine, _shutdown) = engine(2, Duration::from_millis(20));
    let processor = Processor::new(pipeline(1_000_000), engine);
    // Built before the clock starts: constructing the series is not what
    // this test measures, and in a debug build it takes longer than the
    // bound being asserted.
    let request = Request {
        job: job("job-13", &Window::ALL, ALGORITHM_VERSION),
        readings: heart_rate_series(300_000),
    };
    let started = Instant::now();
    let err = processor.process(request).await.unwrap_err();
    assert_eq!(err.kind(), Kind::Timeout);
    assert!(
        started.elapsed() < Duration::from_secs(5),
        "timeout took {:?}",
        started.elapsed()
    );
    tokio::time::timeout(Duration::from_secs(5), processor.engine().drained())
        .await
        .expect("engine drains after timeout");
}

#[tokio::test]
async fn shutdown_refuses_new_jobs_and_stops_running_ones() {
    let (engine, shutdown) = engine(2, Duration::from_secs(60));
    let processor = Processor::new(pipeline(1_000_000), engine);
    let running = tokio::spawn({
        let processor = processor.clone();
        async move {
            processor
                .process(Request {
                    job: job("job-14", &Window::ALL, ALGORITHM_VERSION),
                    readings: heart_rate_series(300_000),
                })
                .await
        }
    });
    tokio::time::sleep(Duration::from_millis(100)).await;
    shutdown.cancel();
    let err = running.await.unwrap().unwrap_err();
    assert_eq!(err.kind(), Kind::Cancelled);
    let err = processor
        .process(Request {
            job: job("job-15", &[Window::OneHour], ALGORITHM_VERSION),
            readings: heart_rate_series(3),
        })
        .await
        .unwrap_err();
    assert_eq!(err.code(), "PROCESSOR_SHUTTING_DOWN");
}
