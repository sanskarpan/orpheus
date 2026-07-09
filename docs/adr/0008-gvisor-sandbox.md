# ADR-0008: gVisor Sandbox for Untrusted Audio Processing

- **Status:** Accepted
- **Date:** 2026-07-09

## Context

Workers process user-uploaded audio with ffmpeg, librosa, and ML models.
ffmpeg has a long history of memory-safety CVEs. A malicious file could
escape the worker. The exploit class is real and well-known.

## Decision

All processing workers run in **gVisor** (`runtimeClassName: gvisor`) from
Phase 2 onward. We add:

- Default-deny egress NetworkPolicy.
- Resource limits (CPU, memory, ephemeral storage, FDs, PIDs).
- Read-only root filesystem; writable scratch on emptyDir with size cap.
- Custom seccomp profile (denies ptrace, mount, etc.).
- No privileged containers, no host paths, no host network.
- Per-tenant VRAM cap on shared GPU pools.

In Phase 7 (third-party marketplace), community publishers run in an
even stricter sandbox (no network egress, ephemeral container, no FS
persistence between jobs, dropped capabilities).

## Consequences

- Defends against kernel-escape-class exploits from untrusted audio.
- ~5–15% performance overhead vs runc. Acceptable.
- Reversibility: high (it's a pod spec change).

## Alternatives Considered

- **Firecracker** — best isolation, but harder to integrate with K8s
  (no Device Plugin, no GPU support).
- **Kata Containers** — closer to K8s-native than Firecracker, but heavier
  and slower than gVisor.
- **nsjail** — fine-grained but high operational complexity.
- **Plain runc** — no defense; rejected.

## References

- `docs/architecture/PRODUCTION_DESIGN.md` §11.4 (Worker isolation)
- gVisor, [https://gvisor.dev](https://gvisor.dev)
