# ADR-0009: GitOps with ArgoCD

- **Status:** Accepted
- **Date:** 2026-07-09

## Context

We have multiple environments (dev, staging, prod), per-region clusters,
and ephemeral preview environments. We need a single source of truth for
"what should be running" and an audit trail for every change.

## Decision

**Git is the source of truth.** CI builds images, signs them (cosign
keyless), generates SBOMs (Syft), and updates the GitOps repo with new
image tags. **ArgoCD** syncs the GitOps repo to the cluster. The Go API
image and the Python worker image are built and signed separately in
CI; both are pushed to ECR and tracked in the GitOps repo with versioned
image tags.

Progressive delivery via **Argo Rollouts**: canary 5% → 25% → 50% → 100%
with Prometheus-driven analysis (error rate, p99 latency, SLO burn rate).

## Consequences

- Audit trail for every deploy (Git history).
- Rollback = `git revert`.
- Preview environments are cheap (per-PR ephemeral cluster).
- Slower feedback loop than push-based deploys; mitigated by GitHub
  Actions for dev/staging.

## Alternatives Considered

- **Push-based deploys (kubectl apply from CI)** — no audit trail, no
  drift detection.
- **FluxCD** — equivalent to ArgoCD. ArgoCD has better UI and is the
  CNCF-graduated default.
- **Spinnaker** — too heavy for our team size.

## References

- `docs/architecture/PRODUCTION_DESIGN.md` §15 (Deployment), §16
  (CI/CD)
- Argo Project, [https://argoproj.github.io/cd](https://argoproj.github.io/cd)
