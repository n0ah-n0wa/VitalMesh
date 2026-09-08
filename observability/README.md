# Observability

The local observability stack: Prometheus scrapes both services, Grafana
renders the dashboards, and an OpenTelemetry collector receives their
traces (SPECIFICATIONS.md sections 39–43).

```bash
make observability-up      # Prometheus, Grafana, the collector
make observability-smoke   # check it is actually observing something
make observability-down    # stop it; the database and Redis keep running
```

| Service | Address | What it is for |
|---|---|---|
| Grafana | http://localhost:3000 | the dashboards; no login locally |
| Prometheus | http://localhost:9090 | queries, targets, rules and alerts |
| Collector | http://localhost:4318 | OTLP/HTTP, which both services export to |
| Collector metrics | http://localhost:8888/metrics | whether tracing is working |

## It is optional, and that is the point

Everything here sits behind the `observability` compose profile, so
`docker compose up -d --wait postgres redis` does not start it and no
build, test or verification gate depends on it. `make verify` never touches
it.

The services do not know whether any of it is running. Metrics are served
whether or not anyone scrapes them. Traces are exported to a collector that
may not exist, on a batch processor that drops what it cannot send, so a
collector that is absent, slow or refusing costs a request nothing. With
`OTEL_EXPORTER_OTLP_ENDPOINT` unset the services still propagate W3C trace
context and still log trace ids; they simply export nothing.

## Pointing the services at it

The services run as host processes during development, so Prometheus
reaches them through `host.docker.internal`, which `extra_hosts` in
`docker-compose.yml` makes resolvable on Linux as well as Docker Desktop.
Export traces by setting one variable before starting each service:

```bash
export OTEL_EXPORTER_OTLP_ENDPOINT=http://localhost:4318
```

When a service runs as a container on this network instead, replace the
scrape target in `prometheus/prometheus.yml` with its container name.

## Dashboards

Four dashboards, provisioned from `grafana/dashboards/*.json`. They are
files in the repository rather than state in Grafana's database: editing one
in the browser is fine for exploring, and keeping the change means saving
the JSON back.

| Dashboard | Covers |
|---|---|
| API traffic | request rates by route, method and status; rate-limit decisions |
| Latency and errors | latency percentiles, error rate by class, dependency latency |
| Rust processing and job status | jobs by outcome, duration, engine saturation, job sizes |
| Infrastructure and resources | targets, connection pool, memory, goroutines, CPU, cache, span flow |

Every panel reads a metric whose labels come from a closed vocabulary. No
panel can grow with the data, and no identifier appears in any of them.

## Rules

`prometheus/rules/recording.yml` precomputes what the dashboards and the
alerts both read, so the two cannot disagree about what "the error rate"
means. `prometheus/rules/alerts.yml` alerts on symptoms rather than causes:
requests failing pages, a pool filling up warns.

Two habits are worth copying when adding a rule. Guard a denominator with
`clamp_min`, or an idle service divides by zero. Give a numerator
`or vector(0)` when a zero is meaningful, or a service with no errors
reports nothing at all and a panel reads "No data" where it should read
zero — an alert then cannot tell a healthy service from an uninstrumented
one.

Check a rule change before reloading:

```bash
docker compose exec prometheus promtool check rules /etc/prometheus/rules/recording.yml
curl -X POST http://localhost:9090/-/reload
```

## Traces

The collector prints each trace it receives, so a request crossing both
services is visible without running a trace store:

```bash
docker compose logs -f otel-collector
```

Set `verbosity: detailed` in `otel/collector.yaml` to see every attribute,
which is how to check that a span carries what it should and nothing it
should not. A trace store such as Tempo or Jaeger plugs in by adding one
exporter and naming it in the traces pipeline; nothing about the services
changes, which is why they export to a collector rather than to a backend
directly.

## What the smoke test checks

`make observability-smoke` asserts what a person would otherwise verify by
clicking: that Prometheus is scraping both services and the collector, that
the rules loaded and evaluated without error, that every metric the
dashboards read has samples, that the recorded rules are producing series,
that Grafana has its datasource and all four dashboards, and that the
collector has accepted spans without failing to send any. It also sends
invented HTTP methods at both services and fails if either creates a
series for one, because the method is the one label a client controls and
a metric that grows with it is a metric anyone can exhaust.

It needs the stack and both services running. It is not part of `make
verify`, because a correctness gate must not depend on an optional stack.
