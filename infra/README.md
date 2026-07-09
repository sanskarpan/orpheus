# `infra/`

Infrastructure as code and deployment manifests.

## Layout

```
infra/
├── terraform/        # AWS resources (VPC, EKS, RDS, etc.) — Phase 5+
├── helm/             # Helm charts for Kubernetes workloads — Phase 4+
└── argocd/           # ArgoCD applicationsets and app-of-apps — Phase 5+
```

## Phase 0

Empty. Local development uses `docker-compose.yml` at the repo root
(Postgres, Redis, MinIO).

## Phase 5+

Terraform manages AWS resources (single region initially, multi-region
later). Helm charts wrap each service. ArgoCD syncs from a GitOps repo.

See `docs/architecture/PRODUCTION_DESIGN.md` §10 (Infrastructure
Design) for the target topology.
