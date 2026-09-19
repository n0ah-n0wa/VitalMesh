# Monitoring

What reads the metrics both services have always served, evaluates the alert
rules this repository has always shipped, and answers the trace endpoint the
deployed ConfigMaps have always named.

Until this directory existed, none of that had anything on the other end of
it: `/metrics` was served and never scraped, `alerts.yml` was a file nothing
evaluated, and `OTEL_EXPORTER_OTLP_ENDPOINT` pointed at a hostname that did
not resolve, which the gateway degraded over silently and correctly. That was
the one gap that blocked a production launch: the P1 in
`docs/PRODUCTION_READINESS.md`, now recorded there as closed.

| File | What it is |
|---|---|
| `namespace.yaml` | The `monitoring` namespace, enforcing the restricted Pod Security Standard. |
| `prometheus-rbac.yaml` | Read-only cluster access for endpoint discovery. Nothing else. |
| `prometheus.yaml` | Prometheus, one replica, and its Service. |
| `alertmanager.yaml` | Alertmanager, one replica, and its Service. |
| `collector.yaml` | The OpenTelemetry Collector, two replicas, and its Service. |
| `networkpolicy.yaml` | Default-deny for the namespace, then each opening with its reason. |
| `config/prometheus.yml` | The deployed scrape configuration: Kubernetes discovery, not a static list. |
| `config/rules/` | The recording and alerting rules a deployed Prometheus evaluates. |
| `config/alertmanager.yml` | Routing and inhibition. Notifies nobody; see below. |
| `config/collector.yaml` | Receivers, processors and the traces pipeline. |

## Why it is a separate kustomization

It has a different lifecycle and a different owner from the application. The
application is deployed on every release by CI, through a role scoped to the
`vitalmesh` namespace. This is installed once per cluster by a cluster admin,
beside the load balancer controller and the autoscaler
(`../platform/README.md`). A release must not be able to restart the thing
that would tell you the release went wrong.

It is this repository's own manifests rather than a Helm chart for one
reason that decided it: manifests go through `make k8s-validate` —
kubeconform against two Kubernetes versions, kube-linter with every built-in
check on, Trivy and Checkov — and a chart installed by a script goes through
none of them. The monitoring stack is held to the same standard as
everything else it watches.

## What is deliberately not here

**Delivery.** `config/alertmanager.yml` routes every alert to a receiver
that notifies nobody. An alert arrives, is grouped, is inhibited where a
more serious one covers it, and is visible through the API and the UI — and
goes no further. SNS delivery is added at install time by
`scripts/eks-platform-install.sh`, which substitutes the environment's
`alarm_topic_arn` into `../platform/alertmanager-sns.yml.template` and
annotates the service account with the IRSA role that may publish to it.

The reason is that the topic ARN carries the AWS account id, which does not
belong in a committed manifest, and that a receiver naming an ARN which does
not exist is worse than no receiver: it looks like delivery and is not. The
install script refuses to run when either Terraform output is missing, so a
deployed cluster cannot end up with the base configuration alone.

It also means this base applies unchanged to kind, which is what makes the
whole path testable without an AWS account.

**A trace store.** The collector exports spans to `debug`, as the local one
does. There is no Jaeger, Tempo or X-Ray. What a deployed environment gains
here is the collector's own metrics — accepted, refused and failed span
counts — which are what answer "is tracing working at all" and what
`SpanExportFailing` alerts on. Adding a real backend is one exporter and one
name in the pipeline; no application change.

**Grafana.** The four dashboards in `observability/grafana/dashboards/` are
provisioned into the local stack only. Prometheus's own UI answers the
queries behind them, and a dashboard nobody has opened in a deployed
environment is not worth the deployment surface.

**Long-term storage.** Prometheus keeps 15 days on an `emptyDir`, so history
does not survive the pod. For alerting that costs little: every alert here
evaluates over five-minute windows and the data is rebuilt within one. For
looking back at last week it costs everything, and the answer is remote
write to Amazon Managed Prometheus, which is storage rather than another
replica.

## Single replicas

Prometheus and Alertmanager each run one replica with the `Recreate`
strategy, and that is a decision rather than an oversight. Two Prometheus
instances scraping the same targets duplicate every series and then disagree
about them; two Alertmanagers that are not gossiping both notify. Highly
available versions of each exist — federation, Thanos, an Alertmanager
gossip cluster — and none of them is a replica count. The cost is stated
plainly: while either pod restarts, roughly a minute of samples is missing
and alerts do not evaluate.

The collector runs two, because it is stateless between batches and export
is off the request path, so a restart never fails a request.

## Two copies of the rules

`observability/prometheus/rules/` is what `make up` evaluates; `config/rules/`
is what a deployed Prometheus evaluates. They must agree about what they
measure and are allowed to disagree about how patient they are:

- **recording rules are identical.** They are pure derivations, and an alert
  and a dashboard that share one cannot be allowed to disagree about what
  "the error rate" means.
- **alerting rules have the same names, expressions and severities, and
  longer `for` clauses.** The local file asks for this itself: "a local
  stack is watched for minutes, not weeks. A deployment lengthens them." A
  30-second `ServiceDown` would page on a node drain.

`make k8s-validate` enforces both, and runs `promtool check rules` over each
copy. `scripts/check-alert-rules.py` is the comparison.

## Testing it

```sh
make k8s-monitoring-test          # create, test, destroy
sh scripts/k8s-monitoring-test.sh --keep   # leave the cluster up
```

It applies the rendered manifests to a kind cluster running Calico — so the
NetworkPolicies are enforced rather than accepted and ignored — and then
walks the path that matters: both services discovered and scraped through
the Kubernetes API, recording rules producing series, all eleven alerting
rules loaded with no evaluation errors, spans reaching the collector, and
then a real failure. It points the gateway's Service at a port nothing is
listening on — so the pod stays Ready and its endpoint stays in discovery
while the scrape is refused — waits out `ServiceDown`'s two-minute `for`
clause, confirms the alert fires in Prometheus and arrives at Alertmanager,
restores the Service and confirms the alert clears.

Two other injections were tried first and are written down in the script,
because each taught something. Scaling the Deployment to zero does not work:
the endpoint goes with the pod, the target leaves discovery, and `up == 0`
matches nothing when `up` is absent — which is a real gap in the rule, not in
the test, and is recorded as finding 12 in `docs/FINAL_AUDIT.md`. Deleting
the NetworkPolicy that admits the scrape does not work either: Calico is
stateful and Prometheus holds its scrape connection open, so scraping carried
on through a policy that no longer allowed it.

What it cannot prove is SNS delivery, which needs an account. After
installing into a real environment, publish a test message to the alarm
topic and confirm a subscriber receives it; `scripts/eks-platform-install.sh`
prints that instruction on the way out.

## Reaching it

There is no Ingress. Both UIs are reached by port-forward, as a cluster
admin:

```sh
kubectl -n monitoring port-forward svc/prometheus 9090:9090
kubectl -n monitoring port-forward svc/alertmanager 9093:9093
```

Then `http://localhost:9090/targets` for what is being scraped,
`/alerts` for what is firing, and `http://localhost:9093/#/status` for how
Alertmanager is configured and where it would send.
