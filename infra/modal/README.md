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
