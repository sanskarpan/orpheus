# Orpheus — Deployment & Status Handoff

_Last updated: 2026-08-22. Branch of record: `main` @ the merge that introduced this file._

This document is the single source of truth for **what is deployed, where it runs, how to
bring it back up, and what is still ephemeral**. It contains **no secret values** — only the
names of the stores that hold them.

---

## 1. Current status at a glance

| Stage | State | Evidence |
|-------|-------|----------|
| 1. Modal GPU services | Deployed + auth-gated | 8 services live; 401 on bad shared secret |
| 2. Backend E2E on live Modal | Passing | Full Go `internal/e2e` suite green against the real Python worker + Modal (~56s) |
| 3. Frontend on Vercel | Deployed | Production deployment public; account store on Cloudflare D1 |
| 4. Deployed-UI E2E | Passing | signup → upload(R2) → transcribe(Modal) → COMPLETED, driven through the live Vercel URL |

**Everything user-facing is on allowed services only: Modal + Cloudflare (R2/D1/Tunnel) + Vercel.**

---

## 2. Architecture (as deployed)

```mermaid
flowchart LR
  Browser -->|HTTPS| Vercel[Vercel: Next.js BFF<br/>orpheus-web]
  Vercel -->|accounts CRUD| D1[(Cloudflare D1<br/>orpheus-accounts)]
  Vercel -->|/v1 API calls| Tunnel[Cloudflare Tunnel]
  Tunnel --> API[Go API :8090<br/>local machine]
  API --> PG[(Postgres :5432<br/>local)]
  API -->|presign / objects| R2[(Cloudflare R2<br/>orpheus-uploads)]
  API -->|JetStream| NATS[NATS :4222<br/>local]
  NATS --> Worker[Python worker<br/>local]
  Worker -->|objects| R2
  Worker -->|inference HTTP| Modal[Modal GPU services]
```

- **Frontend (permanent):** Vercel project `orpheus-web`. Deployment protection is **off** so it's public.
- **Account store (permanent):** Cloudflare **D1** database `orpheus-accounts` (table `accounts`).
  Passwords are `scrypt`; the org owner-key is AES-256-GCM encrypted at rest with a key derived
  from `SESSION_SECRET`.
- **Object storage (permanent):** Cloudflare **R2** bucket `orpheus-uploads`.
- **Inference (permanent):** **Modal** workspace `sanskarpandey2004`, 8 services (below).
- **Backend compute (EPHEMERAL):** Go API + Python worker + Postgres + NATS run on the **local
  dev machine**, exposed to Vercel via a **Cloudflare _quick_ tunnel**. The public URL changes on
  restart and only lives while the machine + tunnel + stack are up. Making this permanent is the
  main open item — see §7.

---

## 3. Modal services

Workspace `sanskarpandey2004`. Deploy with `modal deploy infra/modal/<file>.py`. Endpoints follow
`https://sanskarpandey2004--orpheus-<app>-<fn>.modal.run`.

| App | File | Endpoint fn | Purpose |
|-----|------|-------------|---------|
| orpheus-transcribe | `orpheus_transcribe.py` | `transcribe` | faster-whisper ASR |
| orpheus-align | `orpheus_align.py` | `align` | torchaudio MMS_FA forced alignment |
| orpheus-diarize | `orpheus_diarize.py` | `diarize`, `embed` | ECAPA diarization + speaker embedding |
| orpheus-embed | `orpheus_embed.py` | `embed` | all-MiniLM sentence embeddings |
| orpheus-senses | `orpheus_senses.py` | `analyze` | SenseVoice emotion/events |
| orpheus-enhance | `orpheus_enhance.py` | `enhance` | SpeechBrain MetricGAN+ enhancement |
| orpheus-tts | `orpheus_tts.py` | `synth` | Kokoro TTS |
| orpheus-llm | `orpheus_llm.py` | `serve` (`/v1`) | vLLM OpenAI-compatible LLM |

**Auth:** every endpoint checks `payload.token == ORPHEUS_MODAL_SHARED_SECRET`, injected from the
Modal secret **`orpheus-modal-auth`**. If the secret is rotated, containers must cold-start (or the
app must be redeployed) to pick up the new value.

**Fix shipped this round:** `orpheus_diarize.py` crashed (`silhouette_score` ValueError) on short /
single-speaker clips when `max_speakers >= n_windows`. Fixed by capping the k-search below
`n_samples`; redeployed and verified with real audio. (Issue #559 / PR #562.)

---

## 4. Where secrets live (values NOT in this doc or in git)

| Secret | Lives in |
|--------|----------|
| Modal shared secret | Modal secret `orpheus-modal-auth`; local backend env |
| R2 access key / secret, Cloudflare API token, D1 id | local `scratchpad/r2.env` (chmod 600) + Vercel env vars |
| Vercel token | provided by operator; not stored in repo |
| Platform-admin API key (`ORPHEUS_ADMIN_KEY`) | Postgres (`api_keys`) + Vercel env + `apps/web/.env.local` |
| `SESSION_SECRET` | `apps/web/.env.local` + Vercel env (must match so D1 org-keys decrypt) |

`apps/web/.env.local`, `.vercel/`, and local env files are gitignored. **No tokens are committed.**

---

## 5. Bring the backend up locally (what the tunnel points at)

Prereqs on the machine: Postgres on `:5432` (db `orpheus`/`orpheus`, migrated to current version via
`apps/api/cmd/migrate`), `nats-server` (brew), and Modal auth. Required env (see §4 for where the
values come from):

```
# API
ORPHEUS_PORT=8090 ORPHEUS_ENV=dev
ORPHEUS_DATABASE_URL=postgres://orpheus:orpheus@localhost:5432/orpheus?sslmode=disable
ORPHEUS_NATS_URL=nats://localhost:4222
ORPHEUS_S3_ENDPOINT=<R2 endpoint> ORPHEUS_S3_ACCESS_KEY=<r2> ORPHEUS_S3_SECRET_KEY=<r2> ORPHEUS_S3_BUCKET=orpheus-uploads
# Worker (Modal backends, R2 storage)
ORPHEUS_WORKER_{NATS_URL,DATABASE_URL,S3_ENDPOINT,S3_ACCESS_KEY,S3_SECRET_KEY,S3_BUCKET}=...
ORPHEUS_WORKER_TRANSCRIBE_BACKEND=modal  ORPHEUS_ALIGN_BACKEND=modal  ORPHEUS_DIARIZE_BACKEND=modal
ORPHEUS_ENHANCE_BACKEND=modal  ORPHEUS_SENSE_BACKEND=modal
ORPHEUS_MODAL_<SVC>_URL=...  ORPHEUS_MODAL_<SVC>_TOKEN=<shared secret>  ORPHEUS_MODAL_SHARED_SECRET=<shared secret>
AWS_DEFAULT_REGION=us-east-1   # R2 SigV4 via boto3
```

Then: build `apps/api` (`go build ./cmd/api`), run it; run the worker with
`uv run --package orpheus-workers python -m orpheus_workers.worker` (it `sync_catalog`s all 31
processors into Postgres on start). Expose the API:
`cloudflared tunnel --url http://localhost:8090` → public URL.

An admin key is minted with `go run ./apps/api/cmd/bootstrap-admin "<dsn>"` (or `make bootstrap-admin`).

---

## 6. Redeploy the frontend (Vercel)

From `apps/web` (project already linked as `orpheus-web`):

1. Ensure Vercel **production env vars** are set (`vercel env ls production`): `ORPHEUS_API_URL`
   (the current backend public URL), `ORPHEUS_ADMIN_KEY`, `SESSION_SECRET`,
   `ORPHEUS_PLATFORM_ADMIN_EMAILS`, `CLOUDFLARE_ACCOUNT_ID`, `CLOUDFLARE_API_TOKEN`,
   `D1_DATABASE_ID`, `NEXT_PUBLIC_ORPHEUS_WS_URL`.
2. `vercel deploy --prod --yes --token <token>`.
3. If the backend URL changed (new tunnel), update `ORPHEUS_API_URL` (+ WS) and redeploy.

The account store is **Cloudflare D1** (`apps/web/lib/accounts.ts`), so signup/login work on
serverless. (Issue #560 / PR #563.)

---

## 7. Open item: make the backend permanent

Today the backend is local + an ephemeral tunnel. Recommended free, permanent options (researched
2026):

- **Oracle Cloud Always-Free VM** — runs the existing `docker-compose` stack unchanged, no
  idle-sleep, lifetime-free (note: ARM shape reduced to ~2 OCPU/12 GB in mid-2026, "always free"
  has an asterisk). Best "zero code change" path; gives a stable IP/URL for Vercel.
- **Koyeb (compute, no sleep) + Neon (Postgres) + Synadia Cloud or self-hosted NATS** — fully
  managed, stable `*.koyeb.app` URL (no tunnel needed). Free RAM is tight but the worker is just an
  HTTP client at runtime when `BACKEND=modal`.
- **Modal for the Go API** (`@modal.web_server(8090)`) + Neon + Synadia — keeps compute in the
  existing ecosystem; NATS is the awkward piece.

Avoid Google Cloud Run for the worker (request-driven; a long-running NATS consumer needs a paid
min-instance) and Render free for the worker (idle-sleep drops jobs).

When a host is chosen: deploy the stack there, repoint Vercel `ORPHEUS_API_URL` at its stable URL,
drop the tunnel, and re-run the deployed-UI E2E.

---

## 8. Git / issue trail

- This round: issues **#559** (diarize crash), **#560** (serverless account store), **#561**
  (deploy) → PRs **#562**, **#563**, **#564** (final → `main`); all issues auto-closed on merge.
- Prior round: PRs **#539–#556**, issues **#489–#538**.

---

## 9. Known caveats

- **Ephemeral backend URL** (see §7) — the deployed site is live only while the local stack + tunnel
  run.
- **Modal shared secret rotation** requires cold containers / redeploy to take effect across all
  services.
- **Two Postgres on `:5432`** on the dev machine: a host-native instance shadows the Docker
  container. The host instance holds the real migrated schema; connect to `localhost:5432`.
- **Docker daemon on the dev machine has been flaky** — the local stack was run with host-native
  `nats-server` and (previously) `minio` instead of the Docker containers.
