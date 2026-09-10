variable "name" {
  description = "Cluster name, and the prefix for its IAM roles."
  type        = string

  validation {
    condition     = can(regex("^[a-z][a-z0-9-]{2,40}$", var.name))
    error_message = "name must be 3 to 41 lowercase letters, digits or hyphens, starting with a letter."
  }
}

variable "kubernetes_version" {
  description = "Kubernetes minor version, written like 1.34. The application manifests need 1.30 or later."
  type        = string

  validation {
    condition     = can(regex("^1\\.3[0-9]$", var.kubernetes_version))
    error_message = "kubernetes_version must be a minor version from 1.30 on, written like 1.34. The manifests rely on the preStop sleep handler, which is GA from 1.30."
  }
}

variable "subnet_ids" {
  description = "Private subnets for the control plane's network interfaces and for the nodes."
  type        = list(string)

  validation {
    condition     = length(var.subnet_ids) >= 2
    error_message = "EKS requires subnets in at least two availability zones."
  }
}

variable "endpoint_public_access" {
  description = "Whether the Kubernetes API is reachable from outside the VPC at all. The private endpoint is always on."
  type        = bool
  default     = true
}

variable "endpoint_public_access_cidrs" {
  description = "Addresses allowed to reach the public API endpoint. Required when it is enabled."
  type        = list(string)

  validation {
    condition     = alltrue([for c in var.endpoint_public_access_cidrs : can(cidrnetmask(c))])
    error_message = "Every entry must be a valid IPv4 CIDR."
  }

  validation {
    condition     = !contains(var.endpoint_public_access_cidrs, "0.0.0.0/0")
    error_message = "Opening the Kubernetes API to the whole internet is refused here. IAM still authenticates every request, but an endpoint nobody can reach cannot be probed. If it is really wanted, remove this validation deliberately, in review."
  }

  validation {
    condition     = !var.endpoint_public_access || length(var.endpoint_public_access_cidrs) > 0
    error_message = "A public endpoint needs at least one allowed CIDR; AWS would otherwise default to 0.0.0.0/0."
  }
}

variable "secrets_kms_key_arn" {
  description = "KMS key used for envelope encryption of Kubernetes Secrets in etcd."
  type        = string
}

variable "log_kms_key_arn" {
  description = "KMS key that encrypts the cluster's log groups."
  type        = string
}

variable "log_retention_days" {
  description = "Days to keep control-plane and Container Insights logs."
  type        = number

  validation {
    condition     = contains([1, 3, 5, 7, 14, 30, 60, 90, 120, 150, 180, 365, 400, 545, 731, 1096, 1827, 2192, 2557, 2922, 3288, 3653], var.log_retention_days)
    error_message = "log_retention_days must be a retention period CloudWatch Logs supports."
  }
}

variable "node_instance_types" {
  description = "EC2 instance types for the managed node group. x86_64 only: the service images are built for amd64."
  type        = list(string)

  validation {
    condition     = length(var.node_instance_types) > 0
    error_message = "At least one instance type is required."
  }
}

variable "node_ami_type" {
  description = "EKS-optimised AMI family for the nodes. Must match the architecture of node_instance_types and of the images."
  type        = string
  default     = "AL2023_x86_64_STANDARD"
}

variable "node_min_size" {
  description = "Fewest nodes the group may shrink to."
  type        = number
}

variable "node_desired_size" {
  description = "Nodes to run. Nothing in this stack autoscales nodes yet, so this is the count."
  type        = number
}

variable "node_max_size" {
  description = "Most nodes the group may grow to."
  type        = number

  validation {
    condition     = var.node_min_size >= 1 && var.node_min_size <= var.node_desired_size && var.node_desired_size <= var.node_max_size
    error_message = "Node counts must satisfy 1 <= node_min_size <= node_desired_size <= node_max_size."
  }
}

variable "node_disk_size_gib" {
  description = "Root volume size for each node, in GiB. Holds container images and logs."
  type        = number
  default     = 50

  validation {
    condition     = var.node_disk_size_gib >= 20 && var.node_disk_size_gib <= 500
    error_message = "node_disk_size_gib must be between 20 and 500."
  }
}

variable "admin_principal_arns" {
  description = "IAM principals given cluster-admin. Nobody else is, including whoever runs terraform apply."
  type        = list(string)
  default     = []

  validation {
    condition     = alltrue([for a in var.admin_principal_arns : can(regex("^arn:aws[a-z-]*:iam::[0-9]{12}:(role|user)/", a))])
    error_message = "Each admin principal must be an IAM role or user ARN."
  }
}

variable "deployer" {
  description = "The principal that deploys the application, and the namespaces it may manage. It gets admin rights inside those namespaces and nothing cluster-wide."
  type = object({
    principal_arn = string
    namespaces    = list(string)
  })
}

variable "enable_cloudwatch_observability" {
  description = "Install the Amazon CloudWatch Observability add-on (Container Insights metrics and container logs). Charged per metric and per GB ingested."
  type        = bool
  default     = false
}

variable "tags" {
  description = "Tags for every resource, added to the provider's default tags."
  type        = map(string)
  default     = {}
}

# ------------------------------------------------------- platform components

variable "enable_load_balancer_controller" {
  description = "Create the IRSA role for the AWS Load Balancer Controller, which turns the application's Ingress into an Application Load Balancer. The controller itself is installed from infrastructure/kubernetes/platform."
  type        = bool
  default     = true
}

variable "enable_cluster_autoscaler" {
  description = "Create the IRSA role for the Cluster Autoscaler, which resizes the node group between node_min_size and node_max_size. The autoscaler itself is installed from infrastructure/kubernetes/platform."
  type        = bool
  default     = true
}

variable "node_auto_repair" {
  description = "Let EKS replace a node it detects as unhealthy (node monitoring agent plus managed repair). Free, and one fewer thing for a person to notice."
  type        = bool
  default     = true
}

variable "enable_zonal_shift" {
  description = "Register the cluster with ARC zonal shift, so that traffic can be moved away from an impaired availability zone with one API call. Free; needs the cluster to span at least two zones to mean anything."
  type        = bool
  default     = false
}

variable "deletion_protection" {
  description = "Refuse to delete the cluster until this is turned off first, in its own reviewed change."
  type        = bool
  default     = false
}
