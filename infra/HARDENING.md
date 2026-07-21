# Production hardening (Phase 5) — status

Validated config artifacts (not yet applied to a live cluster/account):

| Control | Artifact | Validated with |
|---------|----------|----------------|
| gVisor syscall sandbox | `k8s/runtimeclass-gvisor.yaml` + worker Helm `gvisor.enabled` → `runtimeClassName` | kubeconform, `helm template` |
| Community-processor no-egress sandbox | `k8s/networkpolicy-community-sandbox.yaml` | kubeconform |
| WAF (rate / managed rules / geo) | `terraform/modules/waf` | `terraform validate` |
| VPC endpoints (S3/ECR/Secrets/Logs) | `terraform/modules/vpc-endpoints` | `terraform validate` |
| Supply chain (Trivy/SBOM/cosign) | `.github/workflows/{ci,release}.yml` | runs in CI |
| Argon2id API keys, RLS | shipped in the API | integration tests |

## Still infra-bound (design/config only — needs hardware/accounts to run)

These require a live cloud account, GPU hardware, or a running cluster and so
cannot be exercised end-to-end here. They are captured as design in
`docs/design/` and `docs/architecture/PRODUCTION_DESIGN.md`:

- **Multi-region active-passive** + Postgres read replica + cross-region backup
  (single-region Terraform exists; the second region + failover is a rollout).
- **GPU inference** (Ray Serve, dynamic batching, MIG per-tenant isolation) —
  needs GPU nodes; the model registry (checksum-verified) is shipped.
- **Keycloak HA**, **External Secrets Operator**, **OPA/Rego** rollout.
- **WebRTC ingress** for streaming (the WebSocket ASR + session REST API are
  shipped; WebRTC media needs an SFU like LiveKit).
- **SOC 2 Type I** evidence collection + pen test.
- Enforcing gVisor via an **admission controller** (Kyverno/OPA-Gatekeeper) so
  the RuntimeClass is mandatory, not opt-in.
