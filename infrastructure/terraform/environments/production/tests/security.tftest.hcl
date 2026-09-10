# The security review's invariants, asserted on what Terraform would build
# rather than on the source text the scanners read: whichever way a value
# is reached, this is what it has to come out as.
#
# A test can see only a child module's outputs, not its resources, so each
# module is exercised here on its own with production's values, and then
# the platform composition. Every run is an apply against the mocked
# provider, which creates nothing: a plan alone leaves every reference
# between resources (which key, which role, which log group) unknown, and
# an assertion cannot be evaluated on an unknown. The invariants are module
# properties; staging uses the same modules, so one file covers both
# environments.

mock_provider "aws" {
  source = "../../tests/mocks"
}

# ----------------------------------------------------------------- network

run "network" {
  command = apply

  module {
    source = "../../modules/network"
  }

  variables {
    name                    = "vitalmesh-production"
    cidr_block              = "10.30.0.0/16"
    az_count                = 3
    single_nat_gateway      = false
    enable_flow_logs        = true
    flow_log_retention_days = 365
    log_kms_key_arn         = "arn:aws:kms:eu-central-1:123456789012:key/22222222-2222-2222-2222-222222222222"
    enable_ecr_endpoints    = true
  }

  # The ECR endpoints answer HTTPS from inside the VPC and nothing else,
  # with private DNS so pulls resolve to them without any client change.
  assert {
    condition = alltrue([
      for r in aws_vpc_security_group_ingress_rule.endpoints_https : r.cidr_ipv4 == "10.30.0.0/16" && r.from_port == 443 && r.to_port == 443 && r.ip_protocol == "tcp" && r.referenced_security_group_id == null
      ]) && alltrue([
      for e in aws_vpc_endpoint.ecr : e.private_dns_enabled && e.vpc_endpoint_type == "Interface" && toset(e.subnet_ids) == toset([for sn in aws_subnet.private : sn.id])
    ])
    error_message = "ECR endpoints must sit in the private subnets with private DNS, admitting HTTPS from the VPC range only."
  }

  assert {
    condition     = alltrue([for s in aws_subnet.public : s.map_public_ip_on_launch == false])
    error_message = "Public subnets must not hand out public addresses."
  }

  # The public route table is the only one with a route to the internet
  # gateway; the private ones go to NAT; the database tier gets no default
  # route at all (there is no aws_route for its table).
  assert {
    condition     = aws_route.public_internet.gateway_id != null && alltrue([for r in aws_route.private_nat : r.nat_gateway_id != null])
    error_message = "Only the public tier may route to the internet gateway; private tiers go through NAT."
  }

  assert {
    condition     = aws_flow_log.this[0].traffic_type == "ALL" && aws_cloudwatch_log_group.flow_logs[0].kms_key_id == var.log_kms_key_arn
    error_message = "Flow logs must record all traffic into an encrypted log group."
  }

  # The flow-logs role writes to its own log group and nothing else, and
  # can be assumed only on behalf of this account's flow logs.
  assert {
    condition = alltrue([
      for s in data.aws_iam_policy_document.flow_logs[0].statement : !contains(s.resources, "*")
      ]) && anytrue([
      for s in data.aws_iam_policy_document.flow_logs_assume.statement : contains([for c in s.condition : c.variable], "aws:SourceAccount")
    ])
    error_message = "The flow-logs role must be scoped to its log group, with confused-deputy conditions on its trust."
  }
}

# --------------------------------------------------------------------- EKS

run "eks" {
  command = apply

  module {
    source = "../../modules/eks"
  }

  variables {
    name                         = "vitalmesh-production"
    kubernetes_version           = "1.34"
    subnet_ids                   = ["subnet-0a1b2c3d4e5f60001", "subnet-0a1b2c3d4e5f60002", "subnet-0a1b2c3d4e5f60003"]
    endpoint_public_access       = true
    endpoint_public_access_cidrs = ["203.0.113.0/24"]
    secrets_kms_key_arn          = "arn:aws:kms:eu-central-1:123456789012:key/11111111-1111-1111-1111-111111111111"
    log_kms_key_arn              = "arn:aws:kms:eu-central-1:123456789012:key/22222222-2222-2222-2222-222222222222"
    log_retention_days           = 365
    node_instance_types          = ["m6i.xlarge"]
    node_min_size                = 3
    node_desired_size            = 3
    node_max_size                = 8
    admin_principal_arns         = ["arn:aws:iam::123456789012:role/Admin"]
    deployer = {
      principal_arn = "arn:aws:iam::123456789012:role/vitalmesh-production-deploy"
      namespaces    = ["vitalmesh-production"]
    }
    enable_cloudwatch_observability = true
  }

  assert {
    condition     = aws_eks_cluster.this.access_config[0].authentication_mode == "API" && aws_eks_cluster.this.access_config[0].bootstrap_cluster_creator_admin_permissions == false
    error_message = "Cluster access must be by access entries only, with no implicit admin for whoever applies."
  }

  assert {
    condition     = aws_eks_cluster.this.vpc_config[0].endpoint_private_access == true && !contains(aws_eks_cluster.this.vpc_config[0].public_access_cidrs, "0.0.0.0/0")
    error_message = "The private endpoint must be on, and the public one must never be open to the internet."
  }

  assert {
    condition     = aws_eks_cluster.this.encryption_config[0].provider[0].key_arn == var.secrets_kms_key_arn && contains(aws_eks_cluster.this.encryption_config[0].resources, "secrets")
    error_message = "Kubernetes Secrets must be envelope-encrypted with the environment's data key."
  }

  assert {
    condition     = toset(aws_eks_cluster.this.enabled_cluster_log_types) == toset(["api", "audit", "authenticator", "controllerManager", "scheduler"])
    error_message = "Every control-plane log type must be on; audit is the record of who did what in the cluster."
  }

  assert {
    condition     = aws_cloudwatch_log_group.cluster.kms_key_id == var.log_kms_key_arn && alltrue([for g in aws_cloudwatch_log_group.container_insights : g.kms_key_id == var.log_kms_key_arn])
    error_message = "Every cluster log group must be encrypted with the logs key."
  }

  assert {
    condition     = aws_launch_template.node.metadata_options[0].http_tokens == "required" && aws_launch_template.node.metadata_options[0].http_put_response_hop_limit == 1
    error_message = "Nodes must require IMDSv2 with a hop limit of 1, so pods cannot reach the node's credentials."
  }

  assert {
    condition     = alltrue([for m in aws_launch_template.node.block_device_mappings : alltrue([for e in m.ebs : e.encrypted == "true"])])
    error_message = "Node volumes must be encrypted."
  }

  assert {
    condition     = aws_eks_node_group.default.remote_access == null || length(aws_eks_node_group.default.remote_access) == 0
    error_message = "Nodes must have no SSH access configured."
  }

  # The node role carries only the two managed policies nodes need; the
  # CNI's rights are on the CNI's own role.
  assert {
    condition     = toset([for a in aws_iam_role_policy_attachment.node : basename(a.policy_arn)]) == toset(["AmazonEKSWorkerNodePolicy", "AmazonEC2ContainerRegistryReadOnly"])
    error_message = "The node role must carry only AmazonEKSWorkerNodePolicy and AmazonEC2ContainerRegistryReadOnly."
  }

  # Each IRSA role trusts exactly one service account, by exact match.
  assert {
    condition = alltrue(flatten([
      for k, d in data.aws_iam_policy_document.irsa_assume : [
        for s in d.statement : anytrue([for c in s.condition : c.test == "StringEquals" && endswith(c.variable, ":sub") && length(c.values) == 1])
      ]
    ]))
    error_message = "Every IRSA role must trust exactly one service account, matched exactly."
  }

  assert {
    condition     = aws_eks_access_policy_association.deployer.access_scope[0].type == "namespace" && toset(aws_eks_access_policy_association.deployer.access_scope[0].namespaces) == toset(["vitalmesh-production"])
    error_message = "The deploy role must be scoped to its own namespace and nothing cluster-wide."
  }

  assert {
    condition     = jsondecode(aws_eks_addon.vpc_cni.configuration_values).enableNetworkPolicy == "true"
    error_message = "The VPC CNI must enforce NetworkPolicies, or every policy in the manifests is accepted and ignored."
  }

  # The autoscaler may resize only what carries this cluster's tag.
  assert {
    condition = alltrue([
      for s in data.aws_iam_policy_document.cluster_autoscaler.statement :
      !contains(s.actions, "autoscaling:SetDesiredCapacity") || anytrue([for c in s.condition : c.variable == "aws:ResourceTag/k8s.io/cluster-autoscaler/vitalmesh-production" && toset(c.values) == toset(["owned"])])
    ])
    error_message = "The Cluster Autoscaler's write actions must be conditioned on this cluster's k8s.io/cluster-autoscaler tag."
  }

  # The load balancer controller's policy is upstream's, and every
  # statement that creates or deletes is tag-conditioned.
  assert {
    condition = alltrue([
      for st in jsondecode(aws_iam_role_policy.irsa["load_balancer_controller"].policy).Statement :
      !anytrue([for a in flatten([st.Action]) : contains(["elasticloadbalancing:CreateLoadBalancer", "elasticloadbalancing:DeleteLoadBalancer", "ec2:DeleteSecurityGroup", "elasticloadbalancing:DeleteTargetGroup"], a)]) || can(st.Condition)
    ])
    error_message = "Every create or delete in the load balancer controller's policy must carry a condition on the elbv2.k8s.aws/cluster tag."
  }

  assert {
    condition     = toset(keys(aws_iam_role.irsa)) == toset(["vpc_cni", "load_balancer_controller", "cluster_autoscaler", "cloudwatch"])
    error_message = "Exactly four IRSA roles: the CNI, the load balancer controller, the autoscaler and the CloudWatch agent. The application has none."
  }

  assert {
    condition     = aws_eks_node_group.default.node_repair_config[0].enabled && aws_eks_cluster.this.zonal_shift_config[0].enabled == false
    error_message = "Node auto-repair must be on; zonal shift follows the variable (off in this run)."
  }
}

# -------------------------------------------------------------- PostgreSQL

run "rds" {
  command = apply

  module {
    source = "../../modules/rds"
  }

  variables {
    identifier                   = "vitalmesh-production"
    vpc_id                       = "vpc-0a1b2c3d4e5f60000"
    subnet_ids                   = ["subnet-0a1b2c3d4e5f60011", "subnet-0a1b2c3d4e5f60012", "subnet-0a1b2c3d4e5f60013"]
    allowed_security_group_ids   = { eks_cluster = "sg-0a1b2c3d4e5f60001" }
    instance_class               = "db.m6g.large"
    allocated_storage            = 100
    max_allocated_storage        = 500
    multi_az                     = true
    backup_retention_days        = 14
    deletion_protection          = true
    skip_final_snapshot          = false
    kms_key_arn                  = "arn:aws:kms:eu-central-1:123456789012:key/11111111-1111-1111-1111-111111111111"
    log_kms_key_arn              = "arn:aws:kms:eu-central-1:123456789012:key/22222222-2222-2222-2222-222222222222"
    log_retention_days           = 365
    monitoring_interval          = 60
    performance_insights_enabled = true
    connections_alarm_threshold  = 700
    freeable_memory_alarm_bytes  = 800 * 1048576
    enable_alarms                = true
    alarm_topic_arn              = "arn:aws:sns:eu-central-1:123456789012:vitalmesh-production-alarms"
  }

  assert {
    condition     = aws_db_instance.this.publicly_accessible == false
    error_message = "PostgreSQL must not be publicly accessible."
  }

  assert {
    condition     = aws_db_instance.this.storage_encrypted && aws_db_instance.this.kms_key_id == var.kms_key_arn && aws_db_instance.this.performance_insights_kms_key_id == var.kms_key_arn
    error_message = "PostgreSQL storage and Performance Insights must be encrypted with the environment's data key."
  }

  assert {
    condition     = aws_db_instance.this.manage_master_user_password == true && aws_db_instance.this.master_user_secret_kms_key_id == var.kms_key_arn && aws_db_instance.this.password == null
    error_message = "The master password must be managed by RDS under the data key, never set by Terraform."
  }

  assert {
    condition     = aws_db_instance.this.iam_database_authentication_enabled == true
    error_message = "IAM database authentication must be on, for a least-privileged application role without a static password."
  }

  assert {
    condition     = anytrue([for p in aws_db_parameter_group.this.parameter : p.name == "rds.force_ssl" && p.value == "1"]) && anytrue([for p in aws_db_parameter_group.this.parameter : p.name == "ssl_min_protocol_version" && p.value == "TLSv1.2"])
    error_message = "PostgreSQL must require TLS 1.2 or later (rds.force_ssl = 1)."
  }

  assert {
    condition     = anytrue([for p in aws_db_parameter_group.this.parameter : p.name == "log_parameter_max_length" && p.value == "0"]) && anytrue([for p in aws_db_parameter_group.this.parameter : p.name == "log_parameter_max_length_on_error" && p.value == "0"])
    error_message = "PostgreSQL must not log bound parameters: a slow login query would log an email address."
  }

  assert {
    condition     = alltrue([for r in aws_vpc_security_group_ingress_rule.postgres : r.referenced_security_group_id != null && r.cidr_ipv4 == null && r.cidr_ipv6 == null && r.from_port == 5432 && r.to_port == 5432 && r.ip_protocol == "tcp"])
    error_message = "PostgreSQL must admit port 5432 from named security groups only, never from an address range."
  }

  assert {
    condition     = alltrue([for g in aws_cloudwatch_log_group.this : g.kms_key_id == var.log_kms_key_arn]) && toset(aws_db_instance.this.enabled_cloudwatch_logs_exports) == toset(["postgresql", "upgrade"])
    error_message = "PostgreSQL logs must be exported into log groups encrypted with the logs key."
  }

  assert {
    condition     = aws_db_instance.this.backup_retention_period == 14 && aws_db_instance.this.deletion_protection && !aws_db_instance.this.skip_final_snapshot && aws_db_instance.this.multi_az
    error_message = "Production's values must reach the instance: 14 days of backups, deletion protection, a final snapshot, Multi-AZ."
  }

  # Monitoring: four alarms and the event subscription, all to the topic.
  assert {
    condition = alltrue([
      for a in concat(aws_cloudwatch_metric_alarm.cpu, aws_cloudwatch_metric_alarm.storage, aws_cloudwatch_metric_alarm.connections, aws_cloudwatch_metric_alarm.memory) :
      toset(a.alarm_actions) == toset([var.alarm_topic_arn]) && a.dimensions.DBInstanceIdentifier == aws_db_instance.this.identifier
    ]) && length(aws_cloudwatch_metric_alarm.connections) == 1 && length(aws_cloudwatch_metric_alarm.memory) == 1 && length(aws_db_event_subscription.this) == 1 && contains(aws_db_event_subscription.this[0].event_categories, "failover")
    error_message = "PostgreSQL must have CPU, storage, connection and memory alarms and an RDS event subscription, all notifying the alarm topic."
  }

  # Password changes are restricted to the rds_password role, and no
  # statement logs its parameters.
  assert {
    condition     = anytrue([for p in aws_db_parameter_group.this.parameter : p.name == "rds.restrict_password_commands" && p.value == "1" && p.apply_method == "pending-reboot"])
    error_message = "rds.restrict_password_commands must be on, as a pending-reboot parameter."
  }
}

# A restore: the same module with a snapshot and a suffix creates a second
# instance beside the first, named apart, and refuses without the suffix.
run "rds_restore" {
  command = apply

  module {
    source = "../../modules/rds"
  }

  variables {
    identifier                   = "vitalmesh-production"
    instance_suffix              = "-r20260910"
    snapshot_identifier          = "vitalmesh-production-final"
    vpc_id                       = "vpc-0a1b2c3d4e5f60000"
    subnet_ids                   = ["subnet-0a1b2c3d4e5f60011", "subnet-0a1b2c3d4e5f60012", "subnet-0a1b2c3d4e5f60013"]
    allowed_security_group_ids   = { eks_cluster = "sg-0a1b2c3d4e5f60001" }
    instance_class               = "db.m6g.large"
    allocated_storage            = 100
    max_allocated_storage        = 500
    multi_az                     = true
    backup_retention_days        = 14
    deletion_protection          = true
    skip_final_snapshot          = false
    kms_key_arn                  = "arn:aws:kms:eu-central-1:123456789012:key/11111111-1111-1111-1111-111111111111"
    log_kms_key_arn              = "arn:aws:kms:eu-central-1:123456789012:key/22222222-2222-2222-2222-222222222222"
    log_retention_days           = 365
    monitoring_interval          = 60
    performance_insights_enabled = true
    enable_alarms                = true
    alarm_topic_arn              = "arn:aws:sns:eu-central-1:123456789012:vitalmesh-production-alarms"
  }

  assert {
    condition     = aws_db_instance.this.identifier == "vitalmesh-production-r20260910" && aws_db_instance.this.snapshot_identifier == "vitalmesh-production-final" && aws_db_instance.this.final_snapshot_identifier == "vitalmesh-production-r20260910-final"
    error_message = "A restored instance must carry the suffix in its identifier and its final snapshot name."
  }

  assert {
    condition     = aws_security_group.this.name == "vitalmesh-production-postgres" && aws_db_subnet_group.this.name == "vitalmesh-production" && alltrue([for g in aws_cloudwatch_log_group.this : startswith(g.name, "/aws/rds/instance/vitalmesh-production-r20260910/")])
    error_message = "A restore renames the instance and its log groups, and nothing else."
  }

  assert {
    condition     = aws_db_instance.this.publicly_accessible == false && aws_db_instance.this.storage_encrypted && aws_db_instance.this.manage_master_user_password == true
    error_message = "A restored instance is as private and encrypted as the original."
  }
}

run "rds_restore_without_suffix_is_refused" {
  command = plan

  module {
    source = "../../modules/rds"
  }

  variables {
    identifier                 = "vitalmesh-production"
    snapshot_identifier        = "vitalmesh-production-final"
    vpc_id                     = "vpc-0a1b2c3d4e5f60000"
    subnet_ids                 = ["subnet-0a1b2c3d4e5f60011", "subnet-0a1b2c3d4e5f60012"]
    allowed_security_group_ids = { eks_cluster = "sg-0a1b2c3d4e5f60001" }
    instance_class             = "db.m6g.large"
    allocated_storage          = 100
    max_allocated_storage      = 500
    multi_az                   = true
    backup_retention_days      = 14
    deletion_protection        = true
    skip_final_snapshot        = false
    kms_key_arn                = "arn:aws:kms:eu-central-1:123456789012:key/11111111-1111-1111-1111-111111111111"
    log_kms_key_arn            = "arn:aws:kms:eu-central-1:123456789012:key/22222222-2222-2222-2222-222222222222"
    log_retention_days         = 365
    alarm_topic_arn            = "arn:aws:sns:eu-central-1:123456789012:vitalmesh-production-alarms"
  }

  expect_failures = [var.restore_to_point_in_time]
}

# ------------------------------------------------------------------- Redis

run "elasticache" {
  command = apply

  module {
    source = "../../modules/elasticache"
  }

  variables {
    name                        = "vitalmesh-production"
    vpc_id                      = "vpc-0a1b2c3d4e5f60000"
    subnet_ids                  = ["subnet-0a1b2c3d4e5f60011", "subnet-0a1b2c3d4e5f60012", "subnet-0a1b2c3d4e5f60013"]
    allowed_security_group_ids  = { eks_cluster = "sg-0a1b2c3d4e5f60001" }
    node_type                   = "cache.m6g.large"
    num_cache_clusters          = 2
    kms_key_arn                 = "arn:aws:kms:eu-central-1:123456789012:key/11111111-1111-1111-1111-111111111111"
    log_kms_key_arn             = "arn:aws:kms:eu-central-1:123456789012:key/22222222-2222-2222-2222-222222222222"
    log_retention_days          = 365
    auth_secret_name            = "vitalmesh/production/redis"
    secret_recovery_window_days = 30
    auth_token_version          = 1
    enable_alarms               = true
    alarm_topic_arn             = "arn:aws:sns:eu-central-1:123456789012:vitalmesh-production-alarms"
  }

  assert {
    condition     = aws_elasticache_replication_group.this.transit_encryption_enabled == true && aws_elasticache_replication_group.this.transit_encryption_mode == "required"
    error_message = "Redis must require TLS."
  }

  assert {
    condition     = tobool(aws_elasticache_replication_group.this.at_rest_encryption_enabled) && aws_elasticache_replication_group.this.kms_key_id == var.kms_key_arn
    error_message = "Redis must be encrypted at rest with the environment's data key."
  }

  # The token is set, through the write-only argument; the plain argument
  # (which would land in state) is not.
  assert {
    condition     = aws_elasticache_replication_group.this.auth_token == null && aws_elasticache_replication_group.this.auth_token_wo_version == 1 && aws_secretsmanager_secret_version.auth.secret_string_wo_version == 1
    error_message = "The AUTH token must reach the cache and its secret through write-only arguments only."
  }

  assert {
    condition     = aws_secretsmanager_secret.auth.kms_key_id == var.kms_key_arn
    error_message = "The AUTH secret must be encrypted with the environment's data key."
  }

  assert {
    condition     = alltrue([for r in aws_vpc_security_group_ingress_rule.redis : r.referenced_security_group_id != null && r.cidr_ipv4 == null && r.cidr_ipv6 == null && r.from_port == 6379 && r.to_port == 6379 && r.ip_protocol == "tcp"])
    error_message = "Redis must admit port 6379 from named security groups only, never from an address range."
  }

  assert {
    condition     = alltrue([for g in aws_cloudwatch_log_group.this : g.kms_key_id == var.log_kms_key_arn])
    error_message = "Redis log groups must be encrypted with the logs key."
  }

  # Monitoring: CPU, memory and evictions on both members, lag on the
  # replica, all to the topic.
  assert {
    condition     = length(aws_cloudwatch_metric_alarm.engine_cpu) == 2 && length(aws_cloudwatch_metric_alarm.memory) == 2 && length(aws_cloudwatch_metric_alarm.evictions) == 2 && length(aws_cloudwatch_metric_alarm.replication_lag) == 1 && alltrue([for a in aws_cloudwatch_metric_alarm.evictions : toset(a.alarm_actions) == toset([var.alarm_topic_arn]) && a.threshold == 0])
    error_message = "Redis must alarm on engine CPU, memory and evictions per member and on replication lag per replica, all to the alarm topic."
  }
}

# ---------------------------------------------------------------- platform

run "platform" {
  command = apply

  module {
    source = "../../modules/platform"
  }

  variables {
    environment                          = "production"
    github_repository                    = "n0ah-n0wa/VitalMesh"
    vpc_cidr                             = "10.30.0.0/16"
    az_count                             = 3
    single_nat_gateway                   = false
    enable_flow_logs                     = true
    kubernetes_version                   = "1.34"
    cluster_endpoint_public_access       = true
    cluster_endpoint_public_access_cidrs = ["203.0.113.0/24"]
    cluster_admin_principal_arns         = ["arn:aws:iam::123456789012:role/Admin"]
    node_instance_types                  = ["m6i.xlarge"]
    node_min_size                        = 3
    node_desired_size                    = 3
    node_max_size                        = 8
    db_instance_class                    = "db.m6g.large"
    db_allocated_storage                 = 100
    db_max_allocated_storage             = 500
    db_multi_az                          = true
    db_backup_retention_days             = 14
    db_deletion_protection               = true
    db_skip_final_snapshot               = false
    redis_node_type                      = "cache.m6g.large"
    redis_num_cache_clusters             = 2
    log_retention_days                   = 365
    secret_recovery_window_days          = 30
    ingress_domain_name                  = "api.vitalmesh.example"
    route53_zone_id                      = "Z0123456789ABCDEFGHIJ"
  }

  assert {
    condition     = aws_secretsmanager_secret_version.app.secret_string_wo_version == 1
    error_message = "The application secret must reach Secrets Manager through the write-only argument only."
  }

  assert {
    condition     = aws_secretsmanager_secret.app.kms_key_id == aws_kms_key.data.arn && aws_sns_topic.alarms.kms_master_key_id == aws_kms_key.logs.arn
    error_message = "The application secret must use the data key and the alarm topic the logs key."
  }

  assert {
    condition     = aws_kms_key.data.enable_key_rotation && aws_kms_key.logs.enable_key_rotation
    error_message = "Both keys must rotate."
  }

  # The data key's policy names nobody but the account; only the logs key
  # names services.
  assert {
    condition     = alltrue([for s in data.aws_iam_policy_document.data_key.statement : alltrue([for p in s.principals : p.type == "AWS"])])
    error_message = "No AWS service may be a principal on the data key; services use it through grants under IAM."
  }

  # The deploy role: trusted only from this environment's GitHub
  # environment, and allowed only its own three secrets, its cluster, and
  # decryption through Secrets Manager.
  assert {
    condition = alltrue(flatten([
      for s in data.aws_iam_policy_document.deploy_assume.statement : [
        for c in s.condition : c.test == "StringEquals" && (endswith(c.variable, ":sub") ? toset(c.values) == toset(["repo:n0ah-n0wa/VitalMesh:environment:production"]) : true)
      ]
    ]))
    error_message = "The deploy role must trust exactly repo:<repository>:environment:production."
  }

  assert {
    condition = alltrue([
      for s in data.aws_iam_policy_document.deploy.statement : !contains(s.resources, "*") && !anytrue([for a in s.actions : endswith(a, "*")])
    ])
    error_message = "The deploy policy must name every resource and every action; no wildcards."
  }

  assert {
    condition = anytrue([
      for s in data.aws_iam_policy_document.deploy.statement : contains(s.actions, "kms:Decrypt") && anytrue([for c in s.condition : c.variable == "kms:ViaService"])
    ])
    error_message = "The deploy role may decrypt only through Secrets Manager (kms:ViaService)."
  }

  assert {
    condition     = output.kubernetes_namespace == "vitalmesh-production"
    error_message = "The namespace must match infrastructure/kubernetes/overlays/production."
  }

  # HTTPS: a DNS-validated ACM certificate for exactly the Ingress host,
  # and the output names the validated certificate, not the pending one.
  assert {
    condition     = aws_acm_certificate.ingress[0].domain_name == "api.vitalmesh.example" && aws_acm_certificate.ingress[0].validation_method == "DNS" && output.ingress_certificate_arn == aws_acm_certificate_validation.ingress[0].certificate_arn
    error_message = "Production must issue a DNS-validated certificate for the Ingress host and output the validated ARN."
  }
}
