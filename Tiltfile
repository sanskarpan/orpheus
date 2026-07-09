# Tiltfile for Orpheus local K8s dev.
#
# Phase 0: stub. Phase 4+ will use this for full K8s dev (EKS / k3d / kind).
#
# Requirements:
#   - tilt (https://tilt.dev)
#   - k3d + registry OR kind OR a remote cluster
#   - kubectl
#
# To enable: install tilt and k3d, then `tilt up` from the repo root.
# Until then, use `docker compose up -d` for the minimum stack.

# Print a friendly message if Tilt is invoked but the heavy dependencies aren't installed.
load('ext://helm_remote', 'helm_remote', _disable=True)
load('ext://namespace', 'namespace_create', _disable=True)
