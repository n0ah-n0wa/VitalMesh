//! End-to-end pipeline benchmarks over representative workloads, plus a
//! stage-attribution group that isolates the cost of the public building
//! blocks (timestamp parsing, ordering + aggregation, anomaly detection,
//! serialisation) so that a slow stage can be found without a profiler.
//! Figures are hardware-specific; compare runs on one machine, never quote
//! them as targets.

use std::num::NonZeroUsize;
use std::time::Duration;

use criterion::{BenchmarkId, Criterion, Throughput, black_box, criterion_group, criterion_main};

use processor::anomaly::{ALGORITHM_VERSION, Detector, RuleSet, run as analyse};
use processor::domain::{
    JobId, Measurement, MeasurementId, MeasurementType, MeasurementValue, PatientId, Percentile,
    ProcessingJob, ProcessingParameters, Timestamp, TimestampBounds, Unit, Window,
};
use processor::pipeline::{Control, Limits, Pipeline, RawReading, Request};

fn rules() -> RuleSet {
    RuleSet::from_json(
        r#"{"rules": [
          {"name": "hr", "kind": "THRESHOLD", "measurement_types": ["HEART_RATE"],
           "tiers": {"warning": {"lower": 40, "upper": 180}}},
          {"name": "z", "kind": "Z_SCORE", "min_count": 5, "tiers": {"warning": 3}},
          {"name": "roll", "kind": "ROLLING_DEVIATION", "window_size": 30, "min_count": 10,
           "tiers": {"warning": 4}}
        ]}"#,
    )
    .unwrap()
}

fn limits() -> Limits {
    Limits {
        max_job_measurements: NonZeroUsize::new(2_000_000).unwrap(),
        timestamp_bounds: TimestampBounds {
            max_future_skew: Duration::from_secs(300),
            max_age: Duration::from_secs(400 * 24 * 3600),
        },
    }
}

fn pipeline(rules: RuleSet) -> Pipeline {
    Pipeline::new(Detector::new(rules), limits(), "bench")
}

/// `n` readings of two types at 10-second intervals from 2026-09-01.
fn readings(n: usize) -> Vec<RawReading> {
    let base = Timestamp::parse("2026-09-01T00:00:00Z").unwrap();
    let mut state: u64 = 0x2545_F491_4F6C_DD1D;
    (0..n)
        .map(|i| {
            state ^= state << 13;
            state ^= state >> 7;
            state ^= state << 17;
            let at = base
                .checked_add(Duration::from_secs(i as u64 * 10))
                .unwrap();
            if i % 2 == 0 {
                RawReading {
                    id: format!("hr-{i:07}"),
                    measurement_type: "HEART_RATE".into(),
                    value: 55.0 + (state % 6_000) as f64 / 100.0,
                    unit: "bpm".into(),
                    recorded_at: at.to_rfc3339(),
                }
            } else {
                RawReading {
                    id: format!("spo2-{i:07}"),
                    measurement_type: "SPO2".into(),
                    value: 94.0 + (state % 600) as f64 / 100.0,
                    unit: "%".into(),
                    recorded_at: at.to_rfc3339(),
                }
            }
        })
        .collect()
}

/// The same series as validated domain measurements, in arrival order.
fn measurements(n: usize) -> Vec<Measurement> {
    readings(n)
        .into_iter()
        .map(|r| {
            let (kind, unit) = match r.measurement_type.as_str() {
                "HEART_RATE" => (MeasurementType::HeartRate, Unit::BeatsPerMinute),
                _ => (MeasurementType::Spo2, Unit::Percent),
            };
            Measurement::new(
                MeasurementId::from_string(r.id).unwrap(),
                kind,
                MeasurementValue::new(r.value).unwrap(),
                unit,
                Timestamp::parse(&r.recorded_at).unwrap(),
            )
            .unwrap()
        })
        .collect()
}

fn parameters(windows: &[Window]) -> ProcessingParameters {
    ProcessingParameters::new(
        [MeasurementType::HeartRate, MeasurementType::Spo2],
        windows.iter().copied(),
        [Percentile::new(50).unwrap(), Percentile::new(95).unwrap()],
    )
    .unwrap()
}

fn request(n: usize, windows: &[Window]) -> Request {
    Request {
        job: ProcessingJob::new(
            JobId::parse("bench").unwrap(),
            PatientId::parse("patient").unwrap(),
            parameters(windows),
            ALGORITHM_VERSION,
            Timestamp::parse("2026-09-30T00:00:00Z").unwrap(),
        ),
        readings: readings(n),
    }
}

fn bench_pipeline(c: &mut Criterion) {
    let p = pipeline(rules());
    let control = Control::unbounded();
    let mut group = c.benchmark_group("pipeline");
    group.sample_size(10);
    for &n in &[1_000usize, 10_000, 100_000] {
        let all = request(n, &Window::ALL);
        group.throughput(Throughput::Elements(n as u64));
        group.bench_with_input(BenchmarkId::new("all_windows", n), &all, |b, r| {
            b.iter_batched(
                || r.clone(),
                |r| p.run(black_box(r), &control).unwrap(),
                criterion::BatchSize::LargeInput,
            )
        });
        let hourly = request(n, &[Window::OneHour]);
        group.bench_with_input(BenchmarkId::new("one_hour", n), &hourly, |b, r| {
            b.iter_batched(
                || r.clone(),
                |r| p.run(black_box(r), &control).unwrap(),
                criterion::BatchSize::LargeInput,
            )
        });
    }
    group.finish();
}

/// Isolates the public building blocks at one size so that the whole can
/// be attributed to its parts: `pipeline/all_windows` minus `analyse`
/// is validation, normalisation and result generation; `analyse` minus
/// `analyse_no_rules` is anomaly detection; the rest is ordering,
/// windowing and statistics.
fn bench_stages(c: &mut Criterion) {
    const N: usize = 100_000;
    let control = Control::unbounded();
    let mut group = c.benchmark_group("stages");
    group.sample_size(10);
    group.throughput(Throughput::Elements(N as u64));

    let raw = readings(N);
    group.bench_function(BenchmarkId::new("parse_timestamps", N), |b| {
        b.iter(|| {
            raw.iter()
                .map(|r| Timestamp::parse(black_box(&r.recorded_at)).unwrap())
                .fold(0i128, |acc, t| acc.wrapping_add(t.unix_nanos()))
        })
    });

    let no_rules = pipeline(RuleSet::empty());
    let all = request(N, &Window::ALL);
    group.bench_function(BenchmarkId::new("pipeline_no_rules", N), |b| {
        b.iter_batched(
            || all.clone(),
            |r| no_rules.run(black_box(r), &control).unwrap(),
            criterion::BatchSize::LargeInput,
        )
    });

    let detector = Detector::new(rules());
    let none = Detector::new(RuleSet::empty());
    let params = parameters(&Window::ALL);
    let input = measurements(N);
    group.bench_function(BenchmarkId::new("analyse", N), |b| {
        b.iter_batched(
            || input.clone(),
            |mut m| analyse(black_box(&mut m), &params, &detector).unwrap(),
            criterion::BatchSize::LargeInput,
        )
    });
    group.bench_function(BenchmarkId::new("analyse_no_rules", N), |b| {
        b.iter_batched(
            || input.clone(),
            |mut m| analyse(black_box(&mut m), &params, &none).unwrap(),
            criterion::BatchSize::LargeInput,
        )
    });

    let outcome = pipeline(rules()).run(all.clone(), &control).unwrap();
    group.bench_function(BenchmarkId::new("serialize_outcome", N), |b| {
        b.iter(|| serde_json::to_vec(black_box(&outcome)).unwrap().len())
    });
    group.finish();
}

criterion_group!(benches, bench_pipeline, bench_stages);
criterion_main!(benches);
