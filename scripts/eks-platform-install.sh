#!/bin/sh
# Installs the platform components an environment's cluster needs before the
# application can be deployed: the AWS Load Balancer Controller and the
# Cluster Autoscaler. Both are Helm charts, pinned here, with values in
# infrastructure/kubernetes/platform and the cluster-specific settings read
# from the environment's Terraform outputs.
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
echo "  cluster $cluster in $vpc_id"

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

echo
echo "Installed. Checks:"
echo "  kubectl -n kube-system get deploy aws-load-balancer-controller cluster-autoscaler-aws-cluster-autoscaler"
echo "  kubectl get ingressclass alb"
echo "  kubectl -n kube-system logs deploy/cluster-autoscaler-aws-cluster-autoscaler | head"
