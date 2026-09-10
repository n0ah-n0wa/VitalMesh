# The certificate the load balancer serves (section 104: HTTPS).
#
# Issued by ACM and validated through a DNS record in the hosted zone, so
# there is no private key anywhere: not in state, not in a Secret, not on a
# node. ACM keeps it, renews it, and the load balancer uses it by ARN.
#
# Nothing here needs to be told the ARN. The AWS Load Balancer Controller
# looks up an ACM certificate whose name matches the host in the Ingress,
# which is why ingress_domain_name has to equal the host in the overlay.
#
# What is not here: the record pointing the name at the load balancer. The
# load balancer is created by the controller when the Ingress is applied,
# after this stack, so its address is not known here. Creating that record
# is a step of the first deployment (README), or external-dns's job later.

resource "aws_acm_certificate" "ingress" {
  count = var.ingress_domain_name == null ? 0 : 1

  domain_name       = var.ingress_domain_name
  validation_method = "DNS"
  key_algorithm     = "RSA_2048"

  tags = local.tags

  # A renewed or reissued certificate is created before the old one goes,
  # so the listener is never without one.
  lifecycle {
    create_before_destroy = true
  }
}

# One name, one validation record. one() is deliberate: a certificate with
# alternative names would need a record each, and this stack issues for
# exactly the host the Ingress serves.
resource "aws_route53_record" "acm_validation" {
  count = var.ingress_domain_name == null ? 0 : 1

  zone_id         = var.route53_zone_id
  name            = one(aws_acm_certificate.ingress[0].domain_validation_options).resource_record_name
  type            = one(aws_acm_certificate.ingress[0].domain_validation_options).resource_record_type
  ttl             = 60
  records         = [one(aws_acm_certificate.ingress[0].domain_validation_options).resource_record_value]
  allow_overwrite = true
}

# Waits for ACM to see the record and issue. The output names this
# resource's ARN rather than the certificate's, so nothing downstream can
# reference a certificate that is still pending.
resource "aws_acm_certificate_validation" "ingress" {
  count = var.ingress_domain_name == null ? 0 : 1

  certificate_arn         = aws_acm_certificate.ingress[0].arn
  validation_record_fqdns = [aws_route53_record.acm_validation[0].fqdn]
}
