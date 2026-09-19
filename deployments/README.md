# Deployments

Empty on purpose. The build definitions and the local environment this
directory was planned to hold ended up closer to what they describe, and
this file points at where they actually are rather than at where they were
once going to be.

| What | Where |
|---|---|
| Container build definitions (SPECIFICATIONS.md section 32) | `services/api-gateway/Dockerfile`, `services/processor/Dockerfile` — each beside the service it builds, so a change to a service and a change to how it is packaged are one diff |
| The local environment (section 53) | `docker-compose.yml` at the repository root, where `docker compose` finds it without being told |
| Kubernetes manifests (sections 33–36) | `infrastructure/kubernetes/` |
| The monitoring stack | `infrastructure/kubernetes/monitoring/` |
| AWS infrastructure (sections 57–59) | `infrastructure/terraform/` |

The directory is kept rather than deleted because `docs/IMPLEMENTATION_PLAN.md`
refers to it, and a reader arriving from there should find this note instead
of nothing.
