//! Benchmarks of the statistical engine (SPECIFICATIONS.md section 50).
//!
//! Run with `cargo bench --bench stats`. Results are hardware-specific and
//! must not be quoted as absolute targets; compare runs on the same
//! machine to detect regressions.

use std::num::NonZeroUsize;

use criterion::{BenchmarkId, Criterion, Throughput, black_box, criterion_group, criterion_main};

use processor::domain::{
    Finite, Measurement, MeasurementId, MeasurementType, MeasurementValue, Percentile,
    ProcessingParameters, Timestamp, Window,
};
use processor::stats::{Rolling, aggregate, statistics, time_order, tumbling};

/// A deterministic pseudo-random walk within the heart-rate range.
fn values(n: usize) -> Vec<Finite> {
    let mut state: u64 = 0x9E37_79B9_7F4A_7C15;
    (0..n)
        .map(|_| {
            state ^= state << 13;
            state ^= state >> 7;
            state ^= state << 17;
            let v = 55.0 + (state % 6_000) as f64 / 100.0;
            Finite::new(v).unwrap()
        })
        .collect()
}

fn measurements(n: usize) -> Vec<Measurement> {
    let base = Timestamp::parse("2026-09-01T00:00:00Z").unwrap();
    values(n)
        .into_iter()
        .enumerate()
        .map(|(i, v)| {
            Measurement::new(
                MeasurementId::parse(&format!("m-{i:07}")).unwrap(),
                MeasurementType::HeartRate,
                MeasurementValue::from(v),
                MeasurementType::HeartRate.canonical_unit(),
                base.checked_add(std::time::Duration::from_secs(i as u64 * 10))
                    .unwrap(),
            )
            .unwrap()
        })
        .collect()
}

fn bench_statistics(c: &mut Criterion) {
    let ranks: Vec<Percentile> = [5, 50, 95, 99]
        .into_iter()
        .map(|p| Percentile::new(p).unwrap())
        .collect();
    let mut group = c.benchmark_group("statistics");
    for &n in &[100usize, 1_000, 10_000, 100_000] {
        let sample = values(n);
        group.throughput(Throughput::Elements(n as u64));
        group.bench_with_input(BenchmarkId::from_parameter(n), &sample, |b, sample| {
            b.iter(|| statistics(black_box(sample), black_box(&ranks)).unwrap())
        });
    }
    group.finish();
}

fn bench_rolling(c: &mut Criterion) {
    let sample = values(10_000);
    let mut group = c.benchmark_group("rolling");
    group.throughput(Throughput::Elements(sample.len() as u64));
    for &k in &[5usize, 60, 600] {
        group.bench_with_input(BenchmarkId::from_parameter(k), &k, |b, &k| {
            b.iter(|| {
                Rolling::new(
                    black_box(sample.iter().copied()),
                    NonZeroUsize::new(k).unwrap(),
                )
                .map(|p| p.unwrap().mean.get())
                .sum::<f64>()
            })
        });
    }
    group.finish();
}

fn bench_aggregate(c: &mut Criterion) {
    let parameters = ProcessingParameters::new(
        [MeasurementType::HeartRate],
        Window::ALL,
        [Percentile::new(50).unwrap(), Percentile::new(95).unwrap()],
    )
    .unwrap();
    let mut group = c.benchmark_group("aggregate");
    for &n in &[1_000usize, 100_000] {
        let input = measurements(n);
        group.throughput(Throughput::Elements(n as u64));
        group.bench_with_input(BenchmarkId::from_parameter(n), &input, |b, input| {
            b.iter_batched(
                || input.clone(),
                |mut owned| aggregate(black_box(&mut owned), black_box(&parameters)).unwrap(),
                criterion::BatchSize::LargeInput,
            )
        });
    }
    group.finish();
}

/// The two steps of aggregation that touch every measurement once per
/// window: the time ordering sort and the split into tumbling windows.
fn bench_ordering_and_windows(c: &mut Criterion) {
    const N: usize = 100_000;
    let mut group = c.benchmark_group("aggregate_steps");
    group.throughput(Throughput::Elements(N as u64));
    let input = measurements(N);
    let mut reversed = input.clone();
    reversed.reverse();
    group.bench_function(BenchmarkId::new("time_order_reversed", N), |b| {
        b.iter_batched(
            || reversed.clone(),
            |mut owned| time_order(black_box(&mut owned)),
            criterion::BatchSize::LargeInput,
        )
    });
    group.bench_function(BenchmarkId::new("tumbling_all_windows", N), |b| {
        b.iter(|| {
            Window::ALL
                .iter()
                .map(|&w| tumbling(black_box(&input), w, |m| m.recorded_at()).count())
                .sum::<usize>()
        })
    });
    group.finish();
}

criterion_group!(
    benches,
    bench_statistics,
    bench_rolling,
    bench_aggregate,
    bench_ordering_and_windows
);
criterion_main!(benches);
