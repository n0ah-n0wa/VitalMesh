#!/bin/sh
# Installs the platform components an environment's cluster needs before the
# application can be deployed: the AWS Load Balancer Controller, the Cluster
# Autoscaler, and the monitoring stack that reads the application's metrics
# and delivers its alerts.
#
# The first two are Helm charts, pinned here, with values in
# infrastructure/kubernetes/platform. The monitoring stack is this
# repository's own manifests (infrastructure/kubernetes/monitoring), so it
# passes the same kubeconform, kube-linter, Trivy and Checkov gates as
# everything else and is tested on kind by `make k8s-monitoring-test`. All
# cluster-specific settings are read from the environment's Terraform
# outputs.
#
#   sh scripts/eks-platform-install.sh staging
#   sh scripts/eks-platform-install.sh production
#
# Needs: helm and kubectl on PATH, a kubeconfig for the cluster (aws eks
# update-kubeconfig --name <cluster>) as a principal with cluster-admin,
# and the environment's Terraform root initialised (see
# infrastructure/terraform/README.md, Planning). This is a cluster-admin
# task, run once per cluster and again on upgrades; the deploy role has no
# rights outside the application namespace and cannot do it.
set -eu

ENVIRONMENT="${1:?usage: $0 staging|production}"
case "$ENVIRONMENT" in staging | production) ;; *) echo "environment must be staging or production" >&2; exit 2 ;; esac

TERRAFORM_IMAGE="hashicorp/terraform:1.16.2"
LBC_CHART_VERSION="3.5.0"   # eks/aws-load-balancer-controller, controller v3.5.0
CA_CHART_VERSION="9.59.0"   # autoscaler/cluster-autoscaler; image tag pinned in the values file

TF_DIR="infrastructure/terraform"
PLATFORM_DIR="infrastructure/kubernetes/platform"
KUSTOMIZE_IMAGE="registry.k8s.io/kustomize/kustomize:v5.4.3"
REGION="eu-central-1"

MSYS_NO_PATHCONV=1
export MSYS_NO_PATHCONV

host_path() {
    if command -v cygpath >/dev/null 2>&1; then cygpath -m "$1"; else printf '%s' "$1"; fi
}

repo="$(host_path "$(pwd)")"

# Terraform outputs, read from the initialised environment root. The
# credentials the root was initialised with are needed to read state.
tf_output() {
    docker run --rm \
        -v "$repo/$TF_DIR:/tf" -w "/tf/environments/$ENVIRONMENT" \
        -v "$(host_path "$HOME")/.aws:/root/.aws:ro" -e AWS_PROFILE -e AWS_ACCESS_KEY_ID -e AWS_SECRET_ACCESS_KEY -e AWS_SESSION_TOKEN \
        "$TERRAFORM_IMAGE" output -raw "$1"
}

echo "Reading the $ENVIRONMENT outputs"
cluster="$(tf_output cluster_name)"
vpc_id="$(tf_output vpc_id)"
lbc_role="$(tf_output load_balancer_controller_role_arn)"
ca_role="$(tf_output cluster_autoscaler_role_arn)"
am_role="$(tf_output alertmanager_role_arn)"
alarm_topic="$(tf_output alarm_topic_arn)"
echo "  cluster $cluster in $vpc_id"

# Refuse rather than install a monitoring stack that delivers nowhere.
# An Alertmanager with no receiver looks exactly like a working one from
# the outside: alerts arrive, are grouped, and are never sent. That is the
# failure this whole component exists to end, so it is not allowed to be
# the outcome of running this script.
refuse() {
    echo "$1 is empty in the $ENVIRONMENT Terraform outputs." >&2
    echo "Alertmanager would be installed with no way to notify anyone. Apply this" >&2
    echo "environment Terraform first, then run this again." >&2
    exit 2
}
case "$am_role" in "" | null) refuse alertmanager_role_arn ;; esac
case "$alarm_topic" in "" | null) refuse alarm_topic_arn ;; esac

current="$(kubectl config current-context)"
case "$current" in *"$cluster"*) ;; *)
    echo "kubectl's current context ($current) does not name $cluster; run: aws eks update-kubeconfig --name $cluster --region $REGION" >&2
    exit 2 ;;
esac

helm repo add eks https://aws.github.io/eks-charts >/dev/null
helm repo add autoscaler https://kubernetes.github.io/autoscaler >/dev/null
helm repo update >/dev/null

echo "AWS Load Balancer Controller $LBC_CHART_VERSION"
helm upgrade --install aws-load-balancer-controller eks/aws-load-balancer-controller \
    --namespace kube-system --version "$LBC_CHART_VERSION" \
    --values "$PLATFORM_DIR/aws-load-balancer-controller.values.yaml" \
    --set "clusterName=$cluster" \
    --set "region=$REGION" \
    --set "vpcId=$vpc_id" \
    --set "serviceAccount.annotations.eks\.amazonaws\.com/role-arn=$lbc_role" \
    --wait --timeout 5m

echo "Cluster Autoscaler $CA_CHART_VERSION"
helm upgrade --install cluster-autoscaler autoscaler/cluster-autoscaler \
    --namespace kube-system --version "$CA_CHART_VERSION" \
    --values "$PLATFORM_DIR/cluster-autoscaler.values.yaml" \
    --set "autoDiscovery.clusterName=$cluster" \
    --set "awsRegion=$REGION" \
    --set "rbac.serviceAccount.annotations.eks\.amazonaws\.com/role-arn=$ca_role" \
    --wait --timeout 5m

echo "Monitoring stack (metrics collection and alert delivery)"

# Rendered with the same kustomize version the validation gates use, so
# what is applied here is what those gates checked.
rendered="$(mktemp)"
trap 'rm -f "$rendered" "${am_config:-}"' EXIT INT TERM
docker run --rm -v "$repo/infrastructure/kubernetes:/work" -w /work \
    "$KUSTOMIZE_IMAGE" build monitoring > "$rendered"
kubectl apply --server-side --force-conflicts -f "$(host_path "$rendered")"

# The role that may publish to the alarm topic. Annotated here rather than
# committed, because the ARN carries the account id.
kubectl -n monitoring annotate serviceaccount alertmanager \
    "eks.amazonaws.com/role-arn=$am_role" --overwrite

# Replace the base configuration, which notifies nobody, with one that
# routes to the environment's alarm topic: the same topic the RDS and
# ElastiCache alarms use, so everything that can wake someone arrives in
# one place.
am_config="$(mktemp)"
sed -e "s#@TOPIC_ARN@#$alarm_topic#g" \
    -e "s#@REGION@#$REGION#g" \
    -e "s#@ENVIRONMENT@#$ENVIRONMENT#g" \
    "$PLATFORM_DIR/alertmanager-sns.yml.template" > "$am_config"

# Every placeholder must be gone. A leftover @NAME@ would be published as
# a literal topic ARN and fail at the moment it mattered.
if grep -q "@[A-Z_]*@" "$am_config"; then
    echo "the Alertmanager template still has unsubstituted placeholders:" >&2
    grep -o "@[A-Z_]*@" "$am_config" | sort -u >&2
    exit 1
fi

kubectl -n monitoring create configmap alertmanager-config \
    --from-file=alertmanager.yml="$(host_path "$am_config")" \
    --dry-run=client -o yaml | kubectl -n monitoring apply -f -

# The ConfigMap is mounted, so the running process is still on the old
# one until it restarts.
kubectl -n monitoring rollout restart deployment/alertmanager
kubectl -n monitoring rollout status deployment/alertmanager --timeout=5m
kubectl -n monitoring rollout status deployment/prometheus --timeout=5m
kubectl -n monitoring rollout status deployment/otel-collector --timeout=5m

echo
echo "Installed. Checks:"
echo "  kubectl -n kube-system get deploy aws-load-balancer-controller cluster-autoscaler-aws-cluster-autoscaler"
echo "  kubectl get ingressclass alb"
echo "  kubectl -n kube-system logs deploy/cluster-autoscaler-aws-cluster-autoscaler | head"
echo "  kubectl -n monitoring get deploy prometheus alertmanager otel-collector"
echo "  kubectl -n monitoring port-forward svc/prometheus 9090:9090   # then /targets and /alerts"
echo "  kubectl -n monitoring port-forward svc/alertmanager 9093:9093 # then /#/status"
echo
echo "Alert delivery is configured and has not been proven end to end here:"
echo "publish a test message to $alarm_topic and confirm a subscriber receives it."
