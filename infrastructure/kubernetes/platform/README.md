# Platform components

What a deployed cluster runs beside the application, and the application
manifests assume is there. Nothing here is applied to the kind cluster;
`overlays/local` has no Ingress and no autoscaling to serve.

| Component | Chart | Does | IAM role (Terraform output) |
|---|---|---|---|
| AWS Load Balancer Controller v3.5.0 | `eks/aws-load-balancer-controller` 3.5.0 | Turns the `Ingress` in `components/deployed` into an Application Load Balancer with pods as targets, finds the ACM certificate for its host, and injects readiness gates in labelled namespaces | `load_balancer_controller_role_arn` |
| Cluster Autoscaler 1.34.5 | `autoscaler/cluster-autoscaler` 9.59.0 | Resizes the managed node group between the Terraform minimum and maximum as pods go Pending or nodes sit empty | `cluster_autoscaler_role_arn` |

Both are installed, and upgraded, by one script, as a cluster admin, after
the environment's Terraform has been applied and before the application is
deployed:

```sh
aws eks update-kubeconfig --name vitalmesh-staging --region eu-central-1
sh scripts/eks-platform-install.sh staging
```

The script pins both chart versions and reads the cluster name, VPC and
role ARNs from the Terraform outputs; the values files here hold
everything else. The deploy role cannot run it: it has no rights outside
the application namespace, which is the point of it.

## Why these, and not the alternatives

**An Application Load Balancer, through the controller, rather than an
in-cluster ingress controller behind a Network Load Balancer.** The load
balancer is then AWS's to run, patch and scale; TLS terminates there with
a certificate ACM issued and renews, so no certificate or key ever enters
the cluster; and WAF, Shield and access logs attach to it natively. An
ingress-nginx would be one more Deployment to size, upgrade and watch for
CVEs, in front of a two-service application. This is OQ-18's recommended
default, implemented; the decision record is still to be written.

**Pods as targets (`target-type: ip`) rather than nodes.** A request goes
load balancer to pod, with no NodePort and no extra hop through kube-proxy.
The cost is that the load balancer connects from the public subnets, so
each overlay's NetworkPolicy admits those ranges explicitly
(`overlays/<env>/networkpolicy-ingress.yaml`, matching the Terraform output
`public_subnet_cidrs`), and that a new pod must be registered before it is
useful, which the readiness gate handles.

**Cluster Autoscaler rather than Karpenter.** One managed node group of one
instance type is what the Terraform builds, and the autoscaler resizes it
with no further moving parts: its role is scoped to the tag EKS puts on the
group. Karpenter would choose instance types and zones and consolidate more
aggressively, at the cost of an SQS queue, EventBridge rules, its own node
role and CRDs. It is the upgrade path once the node group is the
bottleneck, not before.

**Not EKS Auto Mode.** Auto Mode bundles both of the above, plus storage,
into the control plane, at a premium on every instance hour and without
the launch template the Terraform relies on for IMDSv2, the hop limit and
the root volume. The managed pieces used instead cover the same ground and
can all be read in `infrastructure/terraform/modules/eks`.

**No storage driver.** Nothing in the application has a persistent volume:
state lives in RDS and ElastiCache. The EBS CSI add-on is a one-line
addition to `addons.tf` when something needs one.

## What is not installed yet

- **external-dns.** The record pointing the Ingress host at the load
  balancer is created by hand after the first deployment (see the
  Terraform README) until it is.
- **A metrics or logging stack in the cluster.** Container Insights ships
  container logs and metrics to CloudWatch where it is enabled (production);
  Prometheus, Grafana and a collector are the observability phase's.
