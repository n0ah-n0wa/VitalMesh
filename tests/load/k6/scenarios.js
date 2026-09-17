// VitalMesh load test (SPECIFICATIONS.md section 50), for k6.
//
// Run through scripts/load-test.sh (`make load-test`), which starts the
// environment, creates the accounts, records the environment and runs this
// file in the pinned k6 container. docs/LOAD_TESTING.md explains the
// workload, what the numbers mean and what they do not.
//
// Five scenarios run at the same time, each an open workload (a constant
// arrival rate: k6 keeps sending at the rate whether or not the system
// keeps up, and counts the iterations it had to drop), all against
// synthetic patients created in setup:
//
//   ingest      one reading per request, POST /measurements
//   batch       BATCH_SIZE readings per request, POST /measurements/batch
//   processing  a 60-reading batch, then a job over exactly those readings,
//               then the results: the processor's own time on the job is
//               `processing_latency`, the client's view is `job_create`
//   readers     GET a patient, its readings and its results
//   ratelimit   one account sent more than its budget on purpose; 429s are
//               expected here and counted, not treated as errors
//
// Every scenario has a pool of accounts of its own, used round-robin
// iteration by iteration (ingest's iterations rotate through ingest's
// accounts, and so on), every VU has a patient of its own and writes
// readings into a time window of its own, so no two requests ever describe
// the same reading and no account's rate budget is shared between
// scenarios. The rates are chosen so that an account stays under the
// gateway's limit (300 requests a minute, five a second) except in
// `ratelimit`, whose one account is pushed over it on purpose.
//
// Thresholds here are correctness gates, not performance targets: a run
// fails if requests fail or checks do not hold. The latency and throughput
// it reports are measurements of the environment it ran in and nothing
// more.
import http from 'k6/http';
import { check, sleep } from 'k6';
import exec from 'k6/execution';
import { Counter, Rate, Trend } from 'k6/metrics';

const BASE = (__ENV.BASE_URL || 'http://localhost:8080').replace(/\/$/, '');
const ACCOUNTS = parseInt(__ENV.ACCOUNTS || '32', 10);
const PASSWORD = __ENV.PASSWORD || 'load-test-password-not-a-secret';
const PROFILE = __ENV.PROFILE || 'standard';
const BATCH_SIZE = parseInt(__ENV.BATCH_SIZE || '100', 10);
const JOB_READINGS = 60;
const STAMP = __ENV.STAMP || 'run';
const ENV_INFO = __ENV.ENV_INFO || '';

// The profiles: rates are iterations per second for the scenario as a whole
// (an iteration is one request, except readers and processing, which make
// three), VUs are how many accounts the scenario has, one per VU. An
// account may make at most five requests a second before the limiter
// answers 429, so a scenario's rate times its requests per iteration,
// divided by its VUs, stays under five.
const PROFILES = {
  smoke: {
    duration: '15s', ramp: '0s',
    ingest: { rate: 5, vus: 4 }, batch: { rate: 1, vus: 2 }, processing: { rate: 1, vus: 2 },
    readers: { rate: 3, vus: 4 }, ratelimit: { rate: 25, vus: 3, duration: '15s' },
  },
  standard: {
    duration: '60s', ramp: '0s',
    ingest: { rate: 20, vus: 8 }, batch: { rate: 5, vus: 4 }, processing: { rate: 2, vus: 4 },
    readers: { rate: 10, vus: 10 }, ratelimit: { rate: 10, vus: 2, duration: '45s' },
  },
};
const P = PROFILES[PROFILE];
if (!P) {
  throw new Error(`unknown PROFILE ${PROFILE}; smoke or standard`);
}
if (__ENV.DURATION) {
  P.duration = __ENV.DURATION;
}

function arrival(name, cfg, extra) {
  return Object.assign({
    executor: 'constant-arrival-rate',
    exec: name,
    rate: cfg.rate,
    timeUnit: '1s',
    duration: cfg.duration || P.duration,
    // Twice the VUs are allowed so that a latency spike delays iterations
    // rather than dropping them; dropped_iterations in the report says
    // whether even that was not enough.
    preAllocatedVUs: cfg.vus || 1,
    maxVUs: 2 * (cfg.vus || 1),
    gracefulStop: '10s',
  }, extra || {});
}

// The rate-limit scenario starts a little later, so its 429s land while
// the others are in steady state and cannot be confused with warm-up.
export const options = {
  scenarios: {
    ingest: arrival('ingest', P.ingest),
    batch: arrival('batch', P.batch),
    processing: arrival('processing', P.processing),
    readers: arrival('readers', P.readers),
    ratelimit: arrival('ratelimit', P.ratelimit, { startTime: '5s' }),
  },
  summaryTrendStats: ['avg', 'p(50)', 'p(95)', 'p(99)', 'max', 'count'],
  thresholds: {
    // Correctness only: nothing here says how fast anything must be.
    errors: ['rate<0.01'],
    checks: ['rate>0.99'],
  },
  // Login and setup happen once; the accounts' tokens live 15 minutes.
  setupTimeout: '120s',
  teardownTimeout: '30s',
};

const TOTAL_VUS = 2 * (P.ingest.vus + P.batch.vus + P.processing.vus + P.readers.vus + P.ratelimit.vus);

// Account pools, one per scenario: [first index, size]. The rate-limit
// probe has exactly one account, shared by its VUs, which is the point.
const POOLS = {
  ingest: [0, P.ingest.vus],
  batch: [P.ingest.vus, P.batch.vus],
  processing: [P.ingest.vus + P.batch.vus, P.processing.vus],
  readers: [P.ingest.vus + P.batch.vus + P.processing.vus, P.readers.vus],
  ratelimit: [P.ingest.vus + P.batch.vus + P.processing.vus + P.readers.vus, 1],
};
const ACCOUNTS_NEEDED = POOLS.ratelimit[0] + 1;
if (ACCOUNTS_NEEDED > ACCOUNTS) {
  throw new Error(`profile ${PROFILE} needs ${ACCOUNTS_NEEDED} accounts, ACCOUNTS is ${ACCOUNTS}`);
}

// Per-operation latency, in milliseconds, one Trend each so the summary
// carries p50/p95/p99 for every operation separately.
const t = {
  ingest: new Trend('op_ingest', true),
  batch: new Trend('op_batch', true),
  jobCreate: new Trend('op_job_create', true),
  results: new Trend('op_results', true),
  readPatient: new Trend('op_read_patient', true),
  readMeasurements: new Trend('op_read_measurements', true),
  readResults: new Trend('op_read_results', true),
  ratelimit: new Trend('op_ratelimit_get', true),
};
// What the processor itself spent on a job, from the job's own timestamps
// (started_at to completed_at): the processing latency without the HTTP
// round trip, the gateway's validation or the database.
const processingLatency = new Trend('processing_latency', true);
const readings = new Counter('readings_ingested');
const jobs = new Counter('jobs_completed');
const jobsRefused = new Counter('jobs_refused');
const rateLimited = new Counter('rate_limited');
const rateLimitRequests = new Counter('rate_limit_requests');
// errors is every answer that was not expected: anything but the success
// the operation asked for, with the rate-limit scenario's 429 excluded.
const errors = new Rate('errors');

const JSON_HEADERS = { 'Content-Type': 'application/json' };

function headers(token, extra) {
  return Object.assign({ Authorization: `Bearer ${token}` }, JSON_HEADERS, extra || {});
}

// ---------------------------------------------------------------- setup

export function setup() {
  const tokens = [];
  for (let i = 1; i <= ACCOUNTS_NEEDED; i++) {
    let res;
    for (let attempt = 1; attempt <= 4; attempt++) {
      res = http.post(`${BASE}/api/v1/auth/login`,
        JSON.stringify({ email: `load-${i}@vitalmesh.invalid`, password: PASSWORD }),
        { headers: JSON_HEADERS, tags: { name: 'setup login' } });
      if (res.status === 200 || (res.status < 500 && res.status !== 429)) {
        break;
      }
      sleep(2 * attempt);
    }
    if (res.status !== 200) {
      throw new Error(`login load-${i}: HTTP ${res.status} ${res.body}`);
    }
    tokens.push(res.json('access_token'));
  }
  // One patient per VU, created by the account that VU will use.
  const patients = [];
  for (let vu = 1; vu <= TOTAL_VUS; vu++) {
    const token = tokens[(vu - 1) % tokens.length];
    const res = http.post(`${BASE}/api/v1/patients`,
      JSON.stringify({ external_reference: `load-${STAMP}-${vu}`, date_of_birth: '1980-01-01', sex: 'UNKNOWN' }),
      { headers: headers(token), tags: { name: 'setup patient' } });
    if (res.status !== 201) {
      throw new Error(`create patient for VU ${vu}: HTTP ${res.status} ${res.body}`);
    }
    patients.push(res.json('id'));
  }
  return { tokens, patients, base: Date.now() - 2 * 3600 * 1000 };
}

// ------------------------------------------------------------ the VU's own

// Each VU (its own JavaScript runtime) keeps a counter of the readings it
// has written; its window starts three hours earlier for every VU, so the
// windows never meet and every timestamp is unique to the microsecond.
let seq = 0;

function me(data) {
  const vu = exec.vu.idInTest;
  // The account is chosen per iteration, not per VU: an arrival-rate
  // executor hands iterations to whichever VU is free, which is mostly the
  // same one when requests are quick, and per-VU accounts would then put a
  // whole scenario's rate on one account's budget. Numbering iterations
  // round the pool spreads the rate evenly whichever VU runs them.
  const [first, size] = POOLS[exec.scenario.name];
  const account = first + (exec.scenario.iterationInTest % size);
  return {
    vu,
    token: data.tokens[account],
    patient: data.patients[(vu - 1) % data.patients.length],
    start: data.base - vu * 3 * 3600 * 1000,
  };
}

function reading(who, offset) {
  return {
    patient_id: who.patient,
    type: 'HEART_RATE',
    value: 60 + ((offset + who.vu) % 40),
    unit: 'bpm',
    recorded_at: new Date(who.start + offset * 10).toISOString(),
    source: 'load-monitor',
  };
}

function nextWindow(who, n) {
  const first = seq;
  seq += n;
  return { first, items: Array.from({ length: n }, (_, i) => reading(who, first + i)) };
}

function expect(res, status, trend, what) {
  const ok = res.status === status;
  if (trend) {
    trend.add(res.timings.duration);
  }
  errors.add(!ok);
  check(res, { [`${what}: ${status}`]: () => ok });
  return ok;
}

// ------------------------------------------------------------- scenarios

export function ingest(data) {
  const who = me(data);
  const w = nextWindow(who, 1);
  const res = http.post(`${BASE}/api/v1/measurements`, JSON.stringify(w.items[0]),
    { headers: headers(who.token), tags: { name: 'POST /measurements' } });
  if (expect(res, 201, t.ingest, 'ingest')) {
    readings.add(1);
  }
}

export function batch(data) {
  const who = me(data);
  const w = nextWindow(who, BATCH_SIZE);
  const res = http.post(`${BASE}/api/v1/measurements/batch`, JSON.stringify({ items: w.items }),
    { headers: headers(who.token), tags: { name: 'POST /measurements/batch' } });
  if (expect(res, 201, t.batch, 'batch')) {
    readings.add(BATCH_SIZE);
  }
}

export function processing(data) {
  const who = me(data);
  const w = nextWindow(who, JOB_READINGS);
  const stored = http.post(`${BASE}/api/v1/measurements/batch`, JSON.stringify({ items: w.items }),
    { headers: headers(who.token), tags: { name: 'POST /measurements/batch (job input)' } });
  if (!expect(stored, 201, null, 'job input batch')) {
    return;
  }
  readings.add(JOB_READINGS);

  // The job covers exactly this window, so every job is the same size.
  const from = w.items[0].recorded_at;
  const to = new Date(who.start + (w.first + JOB_READINGS) * 10).toISOString();
  const job = http.post(`${BASE}/api/v1/processing/jobs`, JSON.stringify({
    patient_id: who.patient, measurement_types: ['HEART_RATE'], windows: ['1m', '5m'], percentiles: [50, 95], from, to,
  }), { headers: headers(who.token), tags: { name: 'POST /processing/jobs' } });
  if (!expect(job, 201, t.jobCreate, 'job created')) {
    if (job.status === 503) {
      jobsRefused.add(1);
    }
    return;
  }
  const body = job.json();
  check(body, { 'job COMPLETED': (b) => b.status === 'COMPLETED' });
  if (body.status === 'COMPLETED') {
    jobs.add(1);
    processingLatency.add(Date.parse(body.completed_at) - Date.parse(body.started_at));
  }

  const results = http.get(`${BASE}/api/v1/patients/${who.patient}/processing-results?limit=50`,
    { headers: headers(who.token), tags: { name: 'GET /patients/{id}/processing-results' } });
  if (expect(results, 200, t.results, 'results')) {
    check(results, { 'results not empty': (r) => r.json('items').length > 0 });
  }
}

export function readers(data) {
  const who = me(data);
  // Read a patient that receives data: those of the processing VUs, which
  // come after the ingest and batch VUs in the numbering.
  const firstProcessingVU = P.ingest.vus + P.batch.vus;
  const target = data.patients[firstProcessingVU + (exec.scenario.iterationInTest % P.processing.vus)];
  const h = headers(who.token);
  expect(http.get(`${BASE}/api/v1/patients/${target}`, { headers: h, tags: { name: 'GET /patients/{id}' } }),
    200, t.readPatient, 'read patient');
  expect(http.get(`${BASE}/api/v1/patients/${target}/measurements?limit=50`, { headers: h, tags: { name: 'GET /patients/{id}/measurements' } }),
    200, t.readMeasurements, 'read measurements');
  expect(http.get(`${BASE}/api/v1/patients/${target}/processing-results?limit=20`, { headers: h, tags: { name: 'GET /patients/{id}/processing-results (reader)' } }),
    200, t.readResults, 'read results');
}

export function ratelimit(data) {
  const who = me(data);
  const res = http.get(`${BASE}/api/v1/patients/${who.patient}`, {
    headers: headers(who.token),
    tags: { name: 'GET /patients/{id} (rate limit)' },
    responseCallback: http.expectedStatuses(200, 429),
  });
  t.ratelimit.add(res.timings.duration);
  rateLimitRequests.add(1);
  if (res.status === 429) {
    rateLimited.add(1);
    check(res, {
      '429 carries Retry-After': (r) => r.headers['Retry-After'] !== undefined,
      '429 says remaining 0': (r) => r.headers['Ratelimit-Remaining'] === '0' || r.headers['RateLimit-Remaining'] === '0',
    });
    errors.add(false);
  } else {
    const ok = res.status === 200;
    check(res, { 'rate-limit probe: 200 or 429': () => ok });
    errors.add(!ok);
  }
}

// --------------------------------------------------------------- summary

function fmt(v) {
  return v === undefined || v === null || Number.isNaN(v) ? '-' : Number(v).toFixed(1);
}

function row(name, m, seconds) {
  if (!m || !m.values || !m.values.count) {
    return `| ${name} | 0 | - | - | - | - | - |`;
  }
  const v = m.values;
  return `| ${name} | ${v.count} | ${fmt(v.count / seconds)} | ${fmt(v['p(50)'])} | ${fmt(v['p(95)'])} | ${fmt(v['p(99)'])} | ${fmt(v.max)} |`;
}

function counter(m) {
  return m && m.values ? m.values.count : 0;
}

export function handleSummary(data) {
  const m = data.metrics;
  const seconds = data.state.testRunDurationMs / 1000;
  const errorRate = m.errors ? m.errors.values.rate : 0;
  const failed = m.http_req_failed ? m.http_req_failed.values.rate : 0;
  const dropped = counter(m.dropped_iterations);
  const total = counter(m.http_reqs);

  const lines = [];
  lines.push(`# Load test report: profile ${PROFILE}, ${STAMP}`);
  lines.push('');
  lines.push('Measurements of one run in the environment below. They describe that');
  lines.push('environment and nothing else; see docs/LOAD_TESTING.md before quoting them.');
  lines.push('');
  lines.push('## Environment');
  lines.push('');
  lines.push('```');
  lines.push(ENV_INFO || '(not recorded)');
  lines.push(`profile ${PROFILE}, run length ${seconds.toFixed(1)} s, accounts ${ACCOUNTS_NEEDED} (one pool per scenario, the rate-limit probe's is one account), VUs up to ${TOTAL_VUS}, batch size ${BATCH_SIZE}, job size ${JOB_READINGS} readings`);
  lines.push('```');
  lines.push('');
  lines.push('## Totals');
  lines.push('');
  lines.push(`| Metric | Value |`);
  lines.push(`|---|---|`);
  lines.push(`| HTTP requests | ${total} (${fmt(total / seconds)} req/s over the whole run) |`);
  lines.push(`| Readings ingested | ${counter(m.readings_ingested)} (${fmt(counter(m.readings_ingested) / seconds)} readings/s) |`);
  lines.push(`| Jobs completed | ${counter(m.jobs_completed)} (${fmt(counter(m.jobs_completed) / seconds)} jobs/s); refused as busy: ${counter(m.jobs_refused)} |`);
  lines.push(`| Error rate (unexpected answers, 429s of the rate-limit probe excluded) | ${(errorRate * 100).toFixed(3)} % |`);
  lines.push(`| http_req_failed (k6's own count, which includes those 429s) | ${(failed * 100).toFixed(3)} % |`);
  lines.push(`| Dropped iterations (arrival rate not sustained by the VUs) | ${dropped} |`);
  lines.push(`| Checks passed | ${m.checks ? (m.checks.values.rate * 100).toFixed(2) : '-'} % |`);
  lines.push('');
  lines.push('## Latency by operation (ms)');
  lines.push('');
  lines.push('| Operation | Requests | req/s | p50 | p95 | p99 | max |');
  lines.push('|---|---|---|---|---|---|---|');
  lines.push(row('POST /measurements (one reading)', m.op_ingest, seconds));
  lines.push(row(`POST /measurements/batch (${BATCH_SIZE} readings)`, m.op_batch, seconds));
  lines.push(row(`POST /processing/jobs (${JOB_READINGS} readings, end to end)`, m.op_job_create, seconds));
  lines.push(row('GET .../processing-results (after a job)', m.op_results, seconds));
  lines.push(row('GET /patients/{id}', m.op_read_patient, seconds));
  lines.push(row('GET /patients/{id}/measurements', m.op_read_measurements, seconds));
  lines.push(row('GET .../processing-results (reader)', m.op_read_results, seconds));
  lines.push(row('GET /patients/{id} (rate-limit probe)', m.op_ratelimit_get, seconds));
  lines.push('');
  lines.push('## Processing latency (ms)');
  lines.push('');
  lines.push('The processor\'s own time on a job, from the job\'s `started_at` to its');
  lines.push('`completed_at`; the end-to-end figure is the job row above.');
  lines.push('');
  lines.push('| Jobs | p50 | p95 | p99 | max |');
  lines.push('|---|---|---|---|---|');
  const pl = m.processing_latency;
  if (pl && pl.values.count) {
    lines.push(`| ${pl.values.count} | ${fmt(pl.values['p(50)'])} | ${fmt(pl.values['p(95)'])} | ${fmt(pl.values['p(99)'])} | ${fmt(pl.values.max)} |`);
  } else {
    lines.push('| 0 | - | - | - | - |');
  }
  lines.push('');
  lines.push('## Rate limiting');
  lines.push('');
  const probes = counter(m.rate_limit_requests);
  const limited = counter(m.rate_limited);
  lines.push(`One account was sent ${P.ratelimit.rate} requests a second for ${P.ratelimit.duration}, against a budget of 300 a minute.`);
  lines.push(`${probes} requests, ${limited} answered 429 (${probes ? ((limited / probes) * 100).toFixed(1) : '-'} %), the rest 200.`);
  if (limited > 0) {
    lines.push('Every 429 was checked for Retry-After and a zero remaining budget (see the checks).');
  }
  lines.push('');
  lines.push('## Thresholds');
  lines.push('');
  for (const [name, metric] of Object.entries(m)) {
    if (metric.thresholds) {
      for (const [expr, th] of Object.entries(metric.thresholds)) {
        lines.push(`- ${name} ${expr}: ${th.ok ? 'ok' : 'FAILED'}`);
      }
    }
  }
  lines.push('');
  const report = lines.join('\n') + '\n';
  // The raw summary carries everything k6 collected except what setup
  // returned: that holds the accounts' access tokens, and a credential
  // does not belong in a file, throwaway environment or not.
  const raw = Object.assign({}, data);
  delete raw.setup_data;
  return {
    stdout: report,
    [`/results/report-${PROFILE}-${STAMP}.md`]: report,
    [`/results/summary-${PROFILE}-${STAMP}.json`]: JSON.stringify(raw, null, 2),
  };
}
