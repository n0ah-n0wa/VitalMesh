# The network every environment runs in: three tiers of subnets across two
# or three availability zones.
#
#   public    load balancers and NAT gateways; the only tier with a route
#             from the internet
#   private   EKS nodes and pods; out through NAT, never directly reachable
#   database  RDS and ElastiCache; no route out of the VPC at all

data "aws_availability_zones" "available" {
  state = "available"

  # Opt-in zones such as Local Zones do not offer every service used here.
  filter {
    name   = "opt-in-status"
    values = ["opt-in-not-required"]
  }
}

data "aws_caller_identity" "current" {}
data "aws_partition" "current" {}
data "aws_region" "current" {}

locals {
  azs = slice(data.aws_availability_zones.available.names, 0, var.az_count)

  # How a /16 is carved, using 10.20.0.0/16 as the example:
  #
  #   public    10.20.0.0/24   10.20.1.0/24   10.20.2.0/24
  #   database  10.20.10.0/24  10.20.11.0/24  10.20.12.0/24
  #   private   10.20.32.0/19  10.20.64.0/19  10.20.96.0/19
  #
  # The private subnets are large on purpose. The VPC CNI gives every pod an
  # address from its node's subnet, so pod count rather than node count is
  # what sizes them: a /19 is about 8,190 addresses per zone.
  public_cidrs   = [for i in range(var.az_count) : cidrsubnet(var.cidr_block, 8, i)]
  database_cidrs = [for i in range(var.az_count) : cidrsubnet(var.cidr_block, 8, 10 + i)]
  private_cidrs  = [for i in range(var.az_count) : cidrsubnet(var.cidr_block, 3, 1 + i)]

  nat_count = var.single_nat_gateway ? 1 : var.az_count
}

resource "aws_vpc" "this" {
  cidr_block           = var.cidr_block
  enable_dns_support   = true
  enable_dns_hostnames = true

  tags = merge(var.tags, { Name = var.name })
}

# Anything launched without a security group gets the VPC's default one.
# AWS creates it permissive (all traffic between members, all egress), so a
# forgotten security group would be an open one. Adopting it here with no
# rules at all makes a forgotten security group a closed one instead.
resource "aws_default_security_group" "this" {
  vpc_id = aws_vpc.this.id

  tags = merge(var.tags, { Name = "${var.name}-default-deny" })
}

resource "aws_internet_gateway" "this" {
  vpc_id = aws_vpc.this.id

  tags = merge(var.tags, { Name = var.name })
}

resource "aws_subnet" "public" {
  count = var.az_count

  vpc_id            = aws_vpc.this.id
  cidr_block        = local.public_cidrs[count.index]
  availability_zone = local.azs[count.index]

  # No automatic public addresses. Nothing in this tier needs one: NAT
  # gateways use Elastic IPs and load balancers bring their own. An instance
  # launched here by mistake stays off the internet.
  map_public_ip_on_launch = false

  tags = merge(var.tags, {
    Name = "${var.name}-public-${local.azs[count.index]}"
    Tier = "public"
    # How the AWS Load Balancer Controller finds subnets for an
    # internet-facing load balancer.
    "kubernetes.io/role/elb" = "1"
  })
}

resource "aws_subnet" "private" {
  count = var.az_count

  vpc_id            = aws_vpc.this.id
  cidr_block        = local.private_cidrs[count.index]
  availability_zone = local.azs[count.index]

  tags = merge(var.tags, {
    Name                              = "${var.name}-private-${local.azs[count.index]}"
    Tier                              = "private"
    "kubernetes.io/role/internal-elb" = "1"
  })
}

resource "aws_subnet" "database" {
  count = var.az_count

  vpc_id            = aws_vpc.this.id
  cidr_block        = local.database_cidrs[count.index]
  availability_zone = local.azs[count.index]

  tags = merge(var.tags, {
    Name = "${var.name}-database-${local.azs[count.index]}"
    Tier = "database"
  })
}

resource "aws_eip" "nat" {
  count = local.nat_count

  domain = "vpc"

  tags = merge(var.tags, { Name = "${var.name}-nat-${local.azs[count.index]}" })

  depends_on = [aws_internet_gateway.this]
}

resource "aws_nat_gateway" "this" {
  count = local.nat_count

  allocation_id = aws_eip.nat[count.index].id
  subnet_id     = aws_subnet.public[count.index].id

  tags = merge(var.tags, { Name = "${var.name}-${local.azs[count.index]}" })

  depends_on = [aws_internet_gateway.this]
}

# ------------------------------------------------------------------ routes

resource "aws_route_table" "public" {
  vpc_id = aws_vpc.this.id

  tags = merge(var.tags, { Name = "${var.name}-public" })
}

resource "aws_route" "public_internet" {
  route_table_id         = aws_route_table.public.id
  destination_cidr_block = "0.0.0.0/0"
  gateway_id             = aws_internet_gateway.this.id
}

resource "aws_route_table_association" "public" {
  count = var.az_count

  subnet_id      = aws_subnet.public[count.index].id
  route_table_id = aws_route_table.public.id
}

# One private route table per zone, each sending egress to the NAT gateway
# in its own zone. With a single NAT gateway they all point at the one.
resource "aws_route_table" "private" {
  count = var.az_count

  vpc_id = aws_vpc.this.id

  tags = merge(var.tags, { Name = "${var.name}-private-${local.azs[count.index]}" })
}

resource "aws_route" "private_nat" {
  count = var.az_count

  route_table_id         = aws_route_table.private[count.index].id
  destination_cidr_block = "0.0.0.0/0"
  nat_gateway_id         = aws_nat_gateway.this[var.single_nat_gateway ? 0 : count.index].id
}

resource "aws_route_table_association" "private" {
  count = var.az_count

  subnet_id      = aws_subnet.private[count.index].id
  route_table_id = aws_route_table.private[count.index].id
}

# The database tier gets a route table with no default route. RDS and
# ElastiCache answer connections; they never need to open one to the
# internet, and a subnet with nowhere to send traffic cannot be used to
# carry data out.
resource "aws_route_table" "database" {
  vpc_id = aws_vpc.this.id

  tags = merge(var.tags, { Name = "${var.name}-database" })
}

resource "aws_route_table_association" "database" {
  count = var.az_count

  subnet_id      = aws_subnet.database[count.index].id
  route_table_id = aws_route_table.database.id
}

# ECR keeps image layers in S3, so without this every image pull from a
# private subnet crosses the NAT gateway and is billed per gigabyte there. A
# gateway endpoint costs nothing and keeps that traffic inside AWS.
resource "aws_vpc_endpoint" "s3" {
  vpc_id            = aws_vpc.this.id
  service_name      = "com.amazonaws.${data.aws_region.current.region}.s3"
  vpc_endpoint_type = "Gateway"
  route_table_ids   = aws_route_table.private[*].id

  tags = merge(var.tags, { Name = "${var.name}-s3" })
}

# --------------------------------------------------------------- flow logs

resource "aws_cloudwatch_log_group" "flow_logs" {
  count = var.enable_flow_logs ? 1 : 0

  name              = "/aws/vpc/${var.name}/flow-logs"
  retention_in_days = var.flow_log_retention_days
  kms_key_id        = var.log_kms_key_arn

  tags = var.tags
}

data "aws_iam_policy_document" "flow_logs_assume" {
  statement {
    actions = ["sts:AssumeRole"]

    principals {
      type        = "Service"
      identifiers = ["vpc-flow-logs.amazonaws.com"]
    }

    # Confused-deputy protection: the service may assume this role only on
    # behalf of flow logs in this account and region.
    condition {
      test     = "StringEquals"
      variable = "aws:SourceAccount"
      values   = [data.aws_caller_identity.current.account_id]
    }

    condition {
      test     = "ArnLike"
      variable = "aws:SourceArn"
      values   = ["arn:${data.aws_partition.current.partition}:ec2:${data.aws_region.current.region}:${data.aws_caller_identity.current.account_id}:vpc-flow-log/*"]
    }
  }
}

resource "aws_iam_role" "flow_logs" {
  count = var.enable_flow_logs ? 1 : 0

  name               = "${var.name}-flow-logs"
  description        = "Delivers VPC flow logs for ${var.name} to its own log group and nowhere else"
  assume_role_policy = data.aws_iam_policy_document.flow_logs_assume.json

  tags = var.tags
}

# Scoped to the one log group, where the common example grants the logs
# actions on every resource in the account.
data "aws_iam_policy_document" "flow_logs" {
  count = var.enable_flow_logs ? 1 : 0

  statement {
    actions = [
      "logs:CreateLogStream",
      "logs:DescribeLogStreams",
      "logs:PutLogEvents",
    ]
    resources = [
      aws_cloudwatch_log_group.flow_logs[0].arn,
      "${aws_cloudwatch_log_group.flow_logs[0].arn}:*",
    ]
  }
}

resource "aws_iam_role_policy" "flow_logs" {
  count = var.enable_flow_logs ? 1 : 0

  name   = "write-flow-logs"
  role   = aws_iam_role.flow_logs[0].id
  policy = data.aws_iam_policy_document.flow_logs[0].json
}

resource "aws_flow_log" "this" {
  count = var.enable_flow_logs ? 1 : 0

  vpc_id                   = aws_vpc.this.id
  traffic_type             = "ALL"
  log_destination_type     = "cloud-watch-logs"
  log_destination          = aws_cloudwatch_log_group.flow_logs[0].arn
  iam_role_arn             = aws_iam_role.flow_logs[0].arn
  max_aggregation_interval = 60

  tags = merge(var.tags, { Name = var.name })
}

# ---------------------------------------------------------- ECR endpoints

# Pulling an image talks to two ECR APIs (ecr.api for the manifest and the
# token, ecr.dkr for the layers) and then to S3 for the bytes. The S3
# gateway endpoint above already keeps the bytes off the NAT gateway; these
# two keep the API calls off it as well, so a pull never leaves AWS and a
# private subnet with no NAT at all could still pull. Interface endpoints
# are billed per hour per zone, which is why they are optional.
#
# The security group admits HTTPS from inside the VPC and nothing else, and
# has no egress: an endpoint answers, it never calls out.
resource "aws_security_group" "endpoints" {
  #checkov:skip=CKV2_AWS_5:Attached to both aws_vpc_endpoint.ecr instances below; checkov's graph does not follow the for_each reference.

  count = var.enable_ecr_endpoints ? 1 : 0

  name        = "${var.name}-vpc-endpoints"
  description = "Interface endpoints for ${var.name}: HTTPS from inside the VPC only"
  vpc_id      = aws_vpc.this.id

  tags = merge(var.tags, { Name = "${var.name}-vpc-endpoints" })
}

resource "aws_vpc_security_group_ingress_rule" "endpoints_https" {
  count = var.enable_ecr_endpoints ? 1 : 0

  security_group_id = aws_security_group.endpoints[0].id
  cidr_ipv4         = aws_vpc.this.cidr_block
  ip_protocol       = "tcp"
  from_port         = 443
  to_port           = 443
  description       = "HTTPS from inside the VPC"

  tags = var.tags
}

resource "aws_vpc_endpoint" "ecr" {
  for_each = var.enable_ecr_endpoints ? toset(["api", "dkr"]) : toset([])

  vpc_id              = aws_vpc.this.id
  service_name        = "com.amazonaws.${data.aws_region.current.region}.ecr.${each.key}"
  vpc_endpoint_type   = "Interface"
  subnet_ids          = aws_subnet.private[*].id
  security_group_ids  = [aws_security_group.endpoints[0].id]
  private_dns_enabled = true

  tags = merge(var.tags, { Name = "${var.name}-ecr-${each.key}" })
}
