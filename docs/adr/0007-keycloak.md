# ADR-0007: Keycloak for Auth in v1

- **Status:** Accepted
- **Date:** 2026-07-09

## Context

We need OAuth2/OIDC, B2B SSO/SAML, API keys, RBAC, and the ability to add
enterprise identity features without vendor lock-in.

## Decision

Self-host **Keycloak** in the EKS cluster from Phase 1. Reasons:
- Free and open source (no per-MAU).
- Full OAuth2/OIDC + SAML + OIDC support.
- Data-residency control (important for EU customers and regulated
  industries).
- Backing store is just Postgres; no external dependency.

Reassess at $1M ARR. If the operational cost of running Keycloak
(upgrades, HA, DB migrations) starts to dominate, evaluate Clerk, Auth0,
or WorkOS.

## Consequences

- Free, data-residency control, full feature set.
- Operational cost is real. We must keep Keycloak upgraded (security
  patches every ~6 weeks) and HA (2 replicas + DB).
- Migration to a managed IdP later is possible but not free.

## Alternatives Considered

- **Auth0** — good for B2C at scale; expensive for B2B SSO.
- **Clerk** — excellent DX, but vendor lock-in. Best for teams < 3
  engineers.
- **WorkOS** — enterprise SSO/SAML/SCIM as a service; pair with our
  own user store.
- **Better Auth** — newer OSS option; watch but not v1.
- **Roll-your-own (Authlib)** — no. Do not roll your own auth.

## References

- `docs/architecture/PRODUCTION_DESIGN.md` §3.6 (Auth), §11.2
  (Authentication)
- Keycloak docs, [https://www.keycloak.org/documentation](https://www.keycloak.org/documentation)
