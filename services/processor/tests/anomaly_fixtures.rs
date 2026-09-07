//! Golden fixtures for the anomaly engine. Each JSON file under
//! `tests/fixtures/anomaly` holds a rule set, processing parameters,
//! measurements and the exact anomalies expected, computed by hand from
//! the rules in the module documentation. The harness also checks that the
//! output is independent of input order and that every result carries the
//! fields the specification requires.

use std::fs;
use std::path::PathBuf;

use serde::Deserialize;

use processor::anomaly::{ALGORITHM_VERSION, Detector, RuleSet, run};
use processor::domain::{Anomaly, Measurement, ProcessingParameters};

#[derive(Deserialize)]
#[serde(deny_unknown_fields)]
struct Fixture {
    description: String,
    rules: RuleSet,
    parameters: ProcessingParameters,
    measurements: Vec<Measurement>,
    expected: Vec<Anomaly>,
}

fn fixtures() -> Vec<(String, Fixture)> {
    let dir = PathBuf::from(env!("CARGO_MANIFEST_DIR")).join("tests/fixtures/anomaly");
    let mut paths: Vec<PathBuf> = fs::read_dir(&dir)
        .unwrap_or_else(|e| panic!("{}: {e}", dir.display()))
        .map(|entry| entry.unwrap().path())
        .filter(|p| p.extension().is_some_and(|e| e == "json"))
        .collect();
    paths.sort();
    assert!(!paths.is_empty(), "no fixtures in {}", dir.display());
    paths
        .into_iter()
        .map(|path| {
            let raw = fs::read_to_string(&path).unwrap();
            let fixture: Fixture =
                serde_json::from_str(&raw).unwrap_or_else(|e| panic!("{}: {e}", path.display()));
            (
                path.file_name().unwrap().to_string_lossy().into_owned(),
                fixture,
            )
        })
        .collect()
}

fn detected(fixture: &Fixture, reverse: bool) -> Vec<Anomaly> {
    let mut measurements = fixture.measurements.clone();
    if reverse {
        measurements.reverse();
    }
    let detector = Detector::new(fixture.rules.clone());
    run(&mut measurements, &fixture.parameters, &detector)
        .unwrap()
        .into_iter()
        .flat_map(|report| report.anomalies)
        .collect()
}

#[test]
fn fixtures_match_expected_anomalies_exactly() {
    for (name, fixture) in fixtures() {
        let got = detected(&fixture, false);
        assert_eq!(
            got,
            fixture.expected,
            "{name}: {}\n got: {}\nwant: {}",
            fixture.description,
            serde_json::to_string_pretty(&got).unwrap(),
            serde_json::to_string_pretty(&fixture.expected).unwrap()
        );
    }
}

#[test]
fn fixtures_are_deterministic_under_input_order() {
    for (name, fixture) in fixtures() {
        assert_eq!(
            detected(&fixture, false),
            detected(&fixture, true),
            "{name}"
        );
    }
}

#[test]
fn every_anomaly_carries_the_required_fields_and_the_engine_version() {
    let required = [
        "measurement_type",
        "window",
        "metric",
        "value",
        "threshold",
        "severity",
        "detected_at",
        "algorithm_version",
    ];
    let mut total = 0;
    for (name, fixture) in fixtures() {
        for anomaly in detected(&fixture, false) {
            total += 1;
            let json: serde_json::Value = serde_json::to_value(&anomaly).unwrap();
            let object = json.as_object().unwrap();
            for field in required {
                assert!(object.contains_key(field), "{name}: missing {field}");
            }
            assert_eq!(object.len(), required.len(), "{name}: unexpected fields");
            assert_eq!(anomaly.algorithm_version, ALGORITHM_VERSION, "{name}");
            assert_eq!(json["algorithm_version"], "1.0.0", "{name}");
            let severity = json["severity"].as_str().unwrap();
            assert!(
                ["INFO", "WARNING", "CRITICAL"].contains(&severity),
                "{name}: {severity}"
            );
            let metric = json["metric"].as_str().unwrap();
            assert!(
                ["THRESHOLD", "Z_SCORE", "ROLLING_DEVIATION"].contains(&metric),
                "{name}: {metric}"
            );
        }
    }
    assert!(total > 0, "fixtures produced no anomalies at all");
}

#[test]
fn detected_at_is_the_measurement_time_and_values_are_observed() {
    for (name, fixture) in fixtures() {
        for anomaly in detected(&fixture, false) {
            let source = fixture
                .measurements
                .iter()
                .find(|m| {
                    m.recorded_at() == anomaly.detected_at
                        && m.measurement_type() == anomaly.measurement_type
                        && m.value().finite() == anomaly.value
                })
                .unwrap_or_else(|| {
                    panic!("{name}: anomaly does not correspond to a measurement: {anomaly:?}")
                });
            assert_eq!(source.value().finite(), anomaly.value);
        }
    }
}
