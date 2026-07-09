# Keycloak — local auth

Phase 1+ runs Keycloak in dev mode and auto-imports the `orpheus` realm from
[`realm-orpheus.json`](./realm-orpheus.json) on first boot.

## Start

```bash
docker compose up -d keycloak
```

Keycloak takes ~20–40 s to come up the first time (it imports the realm and
runs Flyway migrations against Postgres). The healthcheck polls
`/health/ready` on the in-container port 8080.

Wait for the realm to be ready:

```bash
docker compose ps keycloak
# STATE should be "healthy"

# or just hit the well-known endpoint
curl -fsS http://localhost:8088/realms/orpheus/.well-known/openid-configuration
```

## Default credentials

| User / role              | Value          |
| ------------------------ | -------------- |
| Keycloak admin console   | `admin` / `admin` |
| Realm admin (`admin@orpheus.dev`)  | `admin`  |
| Standard user (`user@orpheus.dev`)  | `user`   |
| Service account (`service@orpheus.dev`) | `service` |

> These are dev-only. **Never** use them in production.

## Admin console

Open <http://localhost:8088> and sign in with `admin` / `admin`.

The `orpheus` realm is pre-selected. From here you can manage clients, users,
roles, and identity providers.

## Get a test token

For the `service@orpheus.dev` user via the password grant:

```bash
bash keycloak/test-token.sh
```

Or by hand:

```bash
curl -s -X POST http://localhost:8088/realms/orpheus/protocol/openid-connect/token \
  -H "Content-Type: application/x-www-form-urlencoded" \
  -d "grant_type=password" \
  -d "client_id=orpheus-cli" \
  -d "username=service@orpheus.dev" \
  -d "password=service" | jq -r '.access_token'
```

The same call works for `admin@orpheus.dev` / `admin` and
`user@orpheus.dev` / `user`.

## Clients

| Client ID       | Type           | Used by                                  |
| --------------- | -------------- | ---------------------------------------- |
| `orpheus-api`   | Confidential   | The Go API (validates tokens, server-to-server with `serviceAccountsEnabled`) |
| `orpheus-cli`   | Public         | Local CLI tools (OIDC auth-code + PKCE)  |

The confidential client's secret in dev is `orpheus-api-dev-secret` (see
`realm-orpheus.json`).

## Realm roles

- `admin` — full access
- `user` — standard end-user
- `service` — machine-to-machine accounts

Roles are surfaced on JWTs as `realm_access.roles`.

## Re-importing the realm manually

`--import-realm` only imports on first boot, when the realm is absent from
the DB. To re-import (e.g. after editing `realm-orpheus.json`):

**Option 1 — admin console:** Realm selector → `orpheus` → **Import** in the
sidebar → upload the file. Or use **Realm settings → Action → Partial import**
if you only want to update parts.

**Option 2 — CLI inside the container:**

```bash
docker compose exec keycloak \
  /opt/keycloak/bin/kc.sh import \
    --file /opt/keycloak/data/import/realm-orpheus.json \
    --override true
```

`--override true` lets the import replace existing objects with the same
name (clients, roles, users).

**Option 3 — nuke the Keycloak DB:**

```bash
make infra-reset
# or
docker compose down -v && docker compose up -d keycloak
```

This wipes both `keycloak_data` and the `keycloak` Postgres database, and
the next `up` will re-import from `realm-orpheus.json`.

## Useful endpoints

| URL                                                                            | Purpose                                |
| ------------------------------------------------------------------------------ | -------------------------------------- |
| `http://localhost:8088/`                                                       | Welcome / realm selector               |
| `http://localhost:8088/admin/master/console/`                                  | Admin console                          |
| `http://localhost:8088/realms/orpheus/.well-known/openid-configuration`        | OIDC discovery                         |
| `http://localhost:8088/realms/orpheus/protocol/openid-connect/token`           | Token endpoint                         |
| `http://localhost:8088/realms/orpheus/protocol/openid-connect/userinfo`        | UserInfo endpoint                      |
| `http://localhost:8088/realms/orpheus/protocol/openid-connect/certs`           | JWKS (for token verification)          |
