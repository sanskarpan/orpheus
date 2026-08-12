# Modal GPU services

Orpheus offloads GPU work to Modal. Two apps live here:

- **`orpheus_transcribe.py`** — GPU transcription (faster-whisper large-v3-turbo). See below.
- **`orpheus_llm.py`** — an open instruct model (**Qwen2.5-3B-Instruct**) behind a
  vLLM **OpenAI-compatible** API, so `text.summarize` / `text.translate` /
  `text.detect-language` run for real on GPU with **no external API key**.
- **`orpheus_diarize.py`** — real speaker diarization (SpeechBrain **ECAPA-TDNN**
  embeddings + agglomerative clustering, non-gated) on GPU, replacing the
  round-robin stub. See below.

## Diarization (orpheus_diarize.py)

```bash
modal deploy infra/modal/orpheus_diarize.py   # prints https://<workspace>--orpheus-diarize-diarize.modal.run
```

Wire the worker:

| Env var | Value |
|---|---|
| `ORPHEUS_DIARIZE_BACKEND` | `modal` |
| `ORPHEUS_MODAL_DIARIZE_URL` | the deployed endpoint URL |
| `ORPHEUS_MODAL_DIARIZE_TOKEN` | the `ORPHEUS_MODAL_SHARED_SECRET` value |

Genuine speaker attribution (the same speaker recurs across turns), with
auto speaker-count detection (silhouette) up to `max_speakers`. Verified e2e on a
3-turn two-speaker clip: correctly labelled speaker A / B / A. Falls back to the
stub when unset (or pyannote if `ORPHEUS_DIARIZE_MODEL` + `HF_TOKEN` are set).

## LLM (orpheus_llm.py)

```bash
modal deploy infra/modal/orpheus_llm.py   # prints https://<workspace>--orpheus-llm-serve.modal.run
```

Wire the worker to it via the provider-agnostic LLM layer:

| Env var | Value |
|---|---|
| `ORPHEUS_LLM_PROVIDER` | `openai-compat` |
| `ORPHEUS_LLM_BASE_URL` | `https://<workspace>--orpheus-llm-serve.modal.run/v1` |
| `ORPHEUS_LLM_API_KEY` | the `ORPHEUS_MODAL_SHARED_SECRET` value |
| `ORPHEUS_LLM_MODEL` | `orpheus-llm` |

The same layer also supports `anthropic` (`ANTHROPIC_API_KEY`), `openai`
(`OPENAI_API_KEY`), `gemini` (`GEMINI_API_KEY`), and any other OpenAI-compatible
host (Ollama, OpenRouter, Together…) — set `ORPHEUS_LLM_PROVIDER`/`_BASE_URL`
accordingly. Default (nothing set) is the deterministic stub. Verified e2e:
summarize produced real bullets and translate produced real French from a
transcript, both served by Qwen on an A10.

---

# Modal GPU transcription

GPU transcription service for Orpheus: [faster-whisper](https://github.com/SYSTRAN/faster-whisper)
(CTranslate2) running a modern multilingual model (**large-v3-turbo**, `float16`)
on an [A10 GPU](https://modal.com/pricing), fronted by an authenticated HTTPS
endpoint. It is the GPU counterpart to the worker's local CPU path — the worker
calls it when `ORPHEUS_WORKER_TRANSCRIBE_BACKEND=modal`.

Why: the local default (`tiny.en` on CPU) is slow and English-only. On GPU,
large-v3-turbo is multilingual and far more accurate — e.g. it transcribes the
German "vierteljährliche" correctly where CPU-tiny splits it into "vierte
jährliche" — while a short clip costs a fraction of a cent (A10 ≈ $0.0003/s).

## Deploy

```bash
# one-time: create the shared-secret the worker uses to authenticate
modal secret create orpheus-modal-auth ORPHEUS_MODAL_SHARED_SECRET="$(openssl rand -base64 32)"

# deploy (builds a CUDA+cuDNN image; first build ~30s, first request downloads
# the model to a Volume so later cold starts are fast)
modal deploy infra/modal/orpheus_transcribe.py
```

Deploy prints the endpoint URL, e.g.
`https://<workspace>--orpheus-transcribe-transcribe.modal.run`.

## Wire the worker to it

Set on the worker process (never commit the secret):

| Env var | Value |
|---|---|
| `ORPHEUS_WORKER_TRANSCRIBE_BACKEND` | `modal` (default `local`) |
| `ORPHEUS_MODAL_TRANSCRIBE_URL` | the deployed endpoint URL |
| `ORPHEUS_MODAL_TRANSCRIBE_TOKEN` | the same value as `ORPHEUS_MODAL_SHARED_SECRET` |

With `local` (the default) nothing changes — the worker uses the in-process CPU
model. `modal` offloads every `transcribe` job to the GPU endpoint.

## Request / response

`POST` JSON to the endpoint:

```json
{
  "token": "<shared secret>",
  "audio_b64": "<base64 of the audio bytes>",
  "model": "large-v3-turbo",     // optional; omit/null → GPU default
  "language": null,               // optional; null → auto-detect
  "initial_prompt": null,         // optional
  "vocabulary": ["Orpheus"],     // optional keyterm biasing
  "word_timestamps": false
}
```

Response mirrors the local path plus `gpu_seconds` (wall-clock decode time, for
cost metering):

```json
{ "text": "...", "segments": [...], "language": "de",
  "duration_seconds": 4.85, "model": "large-v3-turbo", "gpu_seconds": 2.79 }
```

## Model weights

Cached on the `orpheus-whisper-cache` Volume (`HF_HOME=/cache`), so only the
first cold start downloads them. To pre-download a different model, call the
endpoint once with `{"model": "<name>"}`.

## Scaling / cost

Configured on the `Transcriber` class in `orpheus_transcribe.py`:
`min_containers=0` (scales to zero when idle → no standing GPU cost),
`scaledown_window=300` (keeps a warm GPU for 5 min after the last request),
`@modal.concurrent(max_inputs=4)` (a few concurrent decodes per GPU). Raise
`min_containers` to keep a GPU permanently warm for latency-sensitive traffic.

## Prewarming (cold-start mitigation)

Scale-to-zero means the first job after idle pays a cold start. Instead of
paying for an always-warm GPU, the API fires a **warmup on a signal that
predicts imminent load** — upload completion — so a container is spinning before
the transcribe job arrives. The transcribe endpoint accepts `{"warmup": true}`
(spins the container + loads the model, returns immediately). Enable it by
pointing the API at the endpoint:

| Env var (on the API) | Value |
|---|---|
| `ORPHEUS_MODAL_WARMUP_URL` | the transcribe endpoint URL |
| `ORPHEUS_MODAL_WARMUP_TOKEN` | the `ORPHEUS_MODAL_SHARED_SECRET` value |

Fire-and-forget (never blocks the upload response); a no-op when unset.
