# Runbook: Synthetic canary failing

Alerts: `CanaryDown` (page), `CanaryAbsent` (ticket).

The canary (`cmd/canary`) probes the API's `/health` and `/ready` from outside
the cluster and exports `orpheus_canary_up`. It catches client-facing outages
that self-scraped metrics miss (broken ingress/LB, DNS, TLS, cert expiry).

## CanaryDown — `orpheus_canary_up == 0`

The prober reached the network but the endpoint did not return 2xx (or was
unreachable). This is a **client-facing outage** until proven otherwise.

1. Confirm scope: is it one endpoint (`{{ $labels.endpoint }}`) or both?
   - `/ready` only → a dependency (DB/NATS/Redis) is unhealthy; check the
     `ApiDown`, `OutboxStalled`, `postgres-connection-exhaustion` signals.
   - both `/health` and `/ready` → the API process or the path to it is down.
2. Reproduce from your workstation: `curl -sS -m5 -o /dev/null -w '%{http_code}\n' https://<api-host>/health`.
   - Connection refused / timeout → ingress/LB/DNS. Check the ingress
     controller, LB target health, and recent cert/DNS changes.
   - 5xx → the API itself. Check `kubectl get pods`, recent deploys, and
     `api-5xx-spike.md`.
3. Mitigate: roll back the last deploy if it correlates; scale the API up if
   pods are crash-looping; fail over ingress if the LB is the culprit.
4. Verify recovery: `orpheus_canary_up` returns to 1 and the alert clears.

## CanaryAbsent — no `orpheus_canary_up` samples

The canary itself stopped reporting (we are blind to the outside view).

1. `kubectl get deploy orpheus-canary` — is it running? Check logs for
   `canary.started` and probe errors.
2. Confirm Prometheus is scraping the canary target (`orpheus-canary:9102`).
3. Restore the Deployment / fix the scrape config. This does not by itself mean
   the API is down — but do a manual `curl` of `/health` while blind.
