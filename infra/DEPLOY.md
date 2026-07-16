# Deploying Orpheus

This document covers the deployment tooling under `infra/`: the Terraform
that provisions AWS, the Helm charts that package the workloads, and the
ArgoCD manifests that keep the cluster in sync with git.

> Scope: this is the tooling for gap #8 (deploy). It targets a single region
> and the `dev` / `staging` environments. See
> `docs/architecture/PRODUCTION_DESIGN.md` §10 for the wider topology.

## Layout

```
infra/
├── terraform/                  # AWS resources (root module + modules/)
│   ├── providers.tf            # aws, tls, random providers + S3 backend
│   ├── variables.tf            # all inputs with defaults
│   ├── main.tf                 # wires the modules together
│   ├── outputs.tf              # endpoints consumed by app config/secrets
│   ├── terraform.tfvars.example
│   └── modules/
│       ├── vpc/                # VPC, subnets (public/private/db), NAT, IGW
│       ├── eks/                # EKS cluster, IRSA OIDC, managed node group
│       ├── rds/                # Postgres 16 (Secrets Manager master creds)
│       ├── elasticache/        # Redis 7 replication group (TLS, encrypted)
│       └── s3/                 # uploads bucket (versioned, SSE-KMS, CORS)
├── helm/
│   ├── orpheus-api/            # Go HTTP API chart
│   └── orpheus-worker/         # Python worker chart (+ KEDA ScaledObject)
└── argocd/
    ├── project.yaml            # AppProject scoping repos/destinations
    ├── appset.yaml             # ApplicationSet: {chart} x {env} matrix
    └── applications/           # equivalent standalone Application manifests
```

## Image sources

Both charts pull from GHCR:

- API: `ghcr.io/<owner>/orpheus-api` (port 8080, `/health`, `/ready`)
- Worker: `ghcr.io/<owner>/orpheus-workers`
  (`ORPHEUS_WORKERS_PROCESS` selects `all` | `control` | `grpc` | `worker`)

Set `image.owner` and pin `image.tag` (git sha) per environment.

## 1. Provision AWS with Terraform

Terraform has no ordering dependencies you need to manage by hand — the module
graph (VPC → EKS/RDS/ElastiCache/S3) is resolved automatically. You do,
however, need to initialise the S3 backend and pick an environment.

```sh
cd infra/terraform

# One-time: create per-env backend config (S3 bucket + DynamoDB lock table
# must already exist), e.g. envs/dev.s3.tfbackend:
#   bucket         = "orpheus-tfstate"
#   key            = "dev/terraform.tfstate"
#   region         = "us-east-1"
#   dynamodb_table = "orpheus-tflock"

terraform init -backend-config=envs/dev.s3.tfbackend

# Copy and edit inputs.
cp terraform.tfvars.example envs/dev.tfvars

terraform plan  -var-file=envs/dev.tfvars -out=dev.plan
terraform apply dev.plan
```

Apply creates, in dependency order:

1. **VPC** — 3-AZ public/private/database subnets, IGW, NAT gateway(s).
2. **EKS** — control plane, IRSA OIDC provider, `default` managed node group.
3. **RDS** — Postgres 16, `rds.force_ssl=1`, master password in Secrets Manager.
4. **ElastiCache** — Redis 7 replication group, TLS + at-rest encryption.
5. **S3** — uploads bucket, versioned, SSE-KMS, lifecycle + CORS.

Grab the outputs the app config needs:

```sh
terraform output rds_endpoint
terraform output redis_primary_endpoint
terraform output s3_bucket_name
terraform output rds_secret_arn        # DB master creds live here
terraform output eks_cluster_name
```

Point `kubectl` at the new cluster:

```sh
aws eks update-kubeconfig --name "$(terraform output -raw eks_cluster_name)"
```

### Secrets

No secrets are stored in Terraform config or state:

- The **RDS master password** is generated and rotated by AWS
  (`manage_master_user_password = true`) and read from Secrets Manager.
- **S3 access** should use IRSA (attach a role ARN via
  `serviceAccount.annotations`) rather than static keys. Where static keys are
  still required, create the Kubernetes Secret out-of-band (or via External
  Secrets) — the charts only *reference* `orpheus-api-secrets` /
  `orpheus-worker-secrets`, they never create them.

```sh
kubectl create secret generic orpheus-api-secrets -n orpheus-staging \
  --from-literal=DATABASE_URL='postgres://...verify-full' \
  --from-literal=S3_ACCESS_KEY='...' \
  --from-literal=S3_SECRET_KEY='...'
```

## 2. Cluster prerequisites

Install the operators the charts depend on:

```sh
# KEDA — required for the worker ScaledObject (keda.enabled=true).
helm repo add kedacore https://kedacore.github.io/charts
helm install keda kedacore/keda -n keda --create-namespace

# ArgoCD.
kubectl create namespace argocd
kubectl apply -n argocd -f \
  https://raw.githubusercontent.com/argoproj/argo-cd/stable/manifests/install.yaml
```

## 3. Install the Helm charts

### Via Helm directly (imperative — good for smoke-testing)

```sh
# API
helm upgrade --install orpheus-api infra/helm/orpheus-api \
  -n orpheus-dev --create-namespace \
  -f infra/helm/orpheus-api/values-dev.yaml \
  --set image.owner=<owner> --set image.tag=<sha>

# Worker
helm upgrade --install orpheus-worker infra/helm/orpheus-worker \
  -n orpheus-dev --create-namespace \
  -f infra/helm/orpheus-worker/values-dev.yaml \
  --set image.owner=<owner> --set image.tag=<sha>
```

Render/validate without applying:

```sh
helm template orpheus-api infra/helm/orpheus-api -f infra/helm/orpheus-api/values-dev.yaml
helm lint infra/helm/orpheus-api infra/helm/orpheus-worker
```

Key chart behaviours:

- **API**: `Deployment` + `Service` (ClusterIP :80 → :8080) + `HPA`
  (`autoscaling.enabled`) + `ConfigMap` (non-secret `ORPHEUS_*`) +
  `ServiceAccount`. Runs non-root (UID 65532), read-only root FS, all
  capabilities dropped, `RuntimeDefault` seccomp. Liveness `/health`,
  readiness `/ready`.
- **Worker**: `Deployment` + `ConfigMap` + `ServiceAccount`, optional
  `Service`, and a `ScaledObject` (KEDA, NATS JetStream lag) — or a CPU `HPA`
  when `keda.enabled=false`. Writable `/scratch` `emptyDir` for transcode I/O.

### Via ArgoCD (declarative — the intended production path)

```sh
kubectl apply -f infra/argocd/project.yaml
kubectl apply -f infra/argocd/appset.yaml     # generates the 4 Applications

# Watch / force a sync:
argocd app list
argocd app sync orpheus-api-dev
argocd app sync orpheus-worker-staging
```

The `ApplicationSet` matrix produces `orpheus-{api,worker}-{dev,staging}`, each
pointed at its chart with the matching `values-<env>.yaml`. `automated`
sync with `prune` + `selfHeal` keeps the cluster reconciled to `main`. The
standalone manifests in `applications/` are the same four apps expressed
explicitly, for teams that prefer app-of-apps over generators — apply *either*
`appset.yaml` *or* the `applications/` directory, not both.

## 4. Promotion & rollback

- **Promote**: bump `image.tag` in `values-staging.yaml` (dev auto-tracks
  `main`); commit; ArgoCD rolls it out.
- **Rollback**: `argocd app rollback <app> <revision>` or revert the git
  commit — self-heal reconciles the cluster back.

## Teardown

```sh
argocd app delete orpheus-api-dev orpheus-worker-dev ...    # or delete the appset
cd infra/terraform && terraform destroy -var-file=envs/dev.tfvars
```

> RDS has `skip_final_snapshot = false`; a `<name>-pg-final` snapshot is taken
> on destroy. Set `deletion_protection = true` (module var) for prod.
