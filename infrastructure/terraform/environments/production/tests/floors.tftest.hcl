# The production floors in modules/platform, each broken on its own against
# production's values. Every run must be refused by exactly the variable it
# breaks, so a floor that stops refusing fails this file.
#
# These are tested here because terraform validate cannot test them: it
# never evaluates the validation rules on a module's inputs, and on its own
# passes production with a single-AZ database.

mock_provider "aws" {
  source = "../../tests/mocks"
}

# Production's values, as environments/production/main.tf sets them.
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

run "single_az_database" {
  command = plan

  module {
    source = "../../modules/platform"
  }

  variables {
    db_multi_az = false
  }

  expect_failures = [var.db_multi_az]
}

run "short_backup_window" {
  command = plan

  module {
    source = "../../modules/platform"
  }

  variables {
    db_backup_retention_days = 6
  }

  expect_failures = [var.db_backup_retention_days]
}

run "no_final_snapshot" {
  command = plan

  module {
    source = "../../modules/platform"
  }

  variables {
    db_skip_final_snapshot = true
  }

  expect_failures = [var.db_skip_final_snapshot]
}

run "no_redis_replica" {
  command = plan

  module {
    source = "../../modules/platform"
  }

  variables {
    redis_num_cache_clusters = 1
  }

  expect_failures = [var.redis_num_cache_clusters]
}

run "no_flow_logs" {
  command = plan

  module {
    source = "../../modules/platform"
  }

  variables {
    enable_flow_logs = false
  }

  expect_failures = [var.enable_flow_logs]
}

run "short_log_retention" {
  command = plan

  module {
    source = "../../modules/platform"
  }

  variables {
    log_retention_days = 90
  }

  expect_failures = [var.log_retention_days]
}

run "no_https" {
  command = plan

  module {
    source = "../../modules/platform"
  }

  variables {
    ingress_domain_name = null
    route53_zone_id     = null
  }

  expect_failures = [var.ingress_domain_name]
}
