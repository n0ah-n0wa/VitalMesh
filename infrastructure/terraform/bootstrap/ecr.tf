# Image repositories, one per service, shared by both environments.
#
# Shared on purpose. An image is built once, on main, and the same digest is
# deployed to staging and then promoted to production. Separate registries
# per environment would mean rebuilding for production, and then production
# would run something staging never tested.
#
# Tags are immutable, so a tag always means the image it was first pushed
# with (section 32: immutable identifiers). Every push is scanned, and
# images are encrypted with a key of their own.

data "aws_iam_policy_document" "ecr_key" {
  #checkov:skip=CKV_AWS_109:A key policy. Its resource wildcard means this key only, and this statement is AWS's default policy delegating the key to IAM.
  #checkov:skip=CKV_AWS_111:As CKV_AWS_109.
  #checkov:skip=CKV_AWS_356:As CKV_AWS_109.

  statement {
    sid       = "AccountAdministersThroughIAM"
    actions   = ["kms:*"]
    resources = ["*"]

    principals {
      type        = "AWS"
      identifiers = [local.account_root]
    }
  }
}

resource "aws_kms_key" "ecr" {
  description             = "VitalMesh container images"
  enable_key_rotation     = true
  deletion_window_in_days = 30
  policy                  = data.aws_iam_policy_document.ecr_key.json
}

resource "aws_kms_alias" "ecr" {
  name          = "alias/vitalmesh-ecr"
  target_key_id = aws_kms_key.ecr.key_id
}

resource "aws_ecr_repository" "this" {
  for_each = var.ecr_repositories

  name                 = each.value
  image_tag_mutability = "IMMUTABLE"

  # A repository holding images cannot be deleted by Terraform. Emptying it
  # first is a deliberate step, taken knowing what is still deployed.
  force_delete = false

  image_scanning_configuration {
    scan_on_push = true
  }

  encryption_configuration {
    encryption_type = "KMS"
    kms_key         = aws_kms_key.ecr.arn
  }
}

resource "aws_ecr_lifecycle_policy" "this" {
  for_each = aws_ecr_repository.this

  repository = each.value.name

  policy = jsonencode({
    rules = [
      {
        rulePriority = 1
        description  = "Untagged images are build leftovers; expire them after a week"
        selection = {
          tagStatus   = "untagged"
          countType   = "sinceImagePushed"
          countUnit   = "days"
          countNumber = 7
        }
        action = { type = "expire" }
      },
      {
        rulePriority = 2
        description  = "Keep the newest ${var.ecr_keep_images} images"
        selection = {
          tagStatus   = "any"
          countType   = "imageCountMoreThan"
          countNumber = var.ecr_keep_images
        }
        action = { type = "expire" }
      },
    ]
  })
}
