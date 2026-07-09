#!/usr/bin/env bash
# Get a test JWT for the `service@orpheus.dev` user via the OIDC password grant.
#
# Usage:
#   ./keycloak/test-token.sh           # prints just the access token
#   ./keycloak/test-token.sh --full    # prints the whole token response as JSON
#
# Requires: curl, jq, and a running Keycloak (see keycloak/README.md).

set -euo pipefail

KEYCLOAK_URL="${KEYCLOAK_URL:-http://localhost:8088}"
REALM="${REALM:-orpheus}"
CLIENT_ID="${CLIENT_ID:-orpheus-cli}"
USERNAME="${USERNAME:-service@orpheus.dev}"
PASSWORD="${PASSWORD:-service}"

TOKEN_ENDPOINT="${KEYCLOAK_URL}/realms/${REALM}/protocol/openid-connect/token"

if ! command -v jq >/dev/null 2>&1; then
  echo "error: jq is required (brew install jq)" >&2
  exit 1
fi

if ! command -v curl >/dev/null 2>&1; then
  echo "error: curl is required" >&2
  exit 1
fi

response=$(curl -fsS -X POST "${TOKEN_ENDPOINT}" \
  -H "Content-Type: application/x-www-form-urlencoded" \
  --data-urlencode "grant_type=password" \
  --data-urlencode "client_id=${CLIENT_ID}" \
  --data-urlencode "username=${USERNAME}" \
  --data-urlencode "password=${PASSWORD}")

if [ "${1:-}" = "--full" ] || [ "${1:-}" = "-f" ]; then
  printf '%s\n' "${response}" | jq .
  exit 0
fi

token=$(printf '%s' "${response}" | jq -er '.access_token // empty')
if [ -z "${token}" ]; then
  echo "error: no access_token in response:" >&2
  printf '%s\n' "${response}" | jq . >&2
  exit 1
fi

printf '%s\n' "${token}"
