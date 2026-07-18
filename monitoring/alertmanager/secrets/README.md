# Alertmanager secrets

Alertmanager reads its Slack webhook and PagerDuty routing key from files here
(referenced via `api_url_file` / `routing_key_file` in `../alertmanager.yml`).

- `slack_webhook_url` — Slack incoming-webhook URL
- `pagerduty_routing_key` — PagerDuty Events API v2 routing key

The committed values are **placeholders** so the config loads. Replace them:

- **dev**: overwrite these files (or bind-mount your own) — real values you drop
  as `*.local` are gitignored.
- **prod (k8s)**: mount a Kubernetes `Secret` at `/etc/alertmanager/secrets`
  (managed by External Secrets Operator), overriding these placeholders.
