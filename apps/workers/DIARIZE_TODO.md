# TODO — `diarize` processor contract (NOT YET IMPLEMENTED)

> **This is a contract/TODO document, not code.** No processor implementation is
> added under `apps/workers/src/` by this doc. It specifies the interface a
> future `diarize` processor MUST satisfy so it slots into the existing
> processor registry (`orpheus_workers.processors`) and the GPU inference plane.
>
> Design context: [`docs/design/10-gpu-inference.md`](../../docs/design/10-gpu-inference.md).
> Reproducibility contract: [ADR-0005](../../docs/adr/0005-model-versioning.md).

---

## Purpose

Answer **"who spoke when"** for an audio artifact using
`pyannote/speaker-diarization-3.1`, returning speaker-labeled time segments.
A separate `align` step (see design #10 §2.4) merges these with a whisper
transcript to produce enriched, speaker-attributed transcript segments.

## Where it fits

- Registered like the existing processors (`extract_metadata`, `probe`,
  `slice`, `transcribe`) in `apps/workers/src/orpheus_workers/processors/`.
- **Does not load the model in-process.** Unlike today's `transcribe.py`, the
  diarize processor calls the **Ray Serve `pyannote-3.1` deployment** and gets
  weights + version from the **model registry** (checksum-verified). See design
  #10 §2.1–2.2.
- Orchestrated by Temporal as the first activity of the `diarize+align` workflow
  (design #9), or standalone as a single job.

---

## Input contract

| Field | Type | Required | Notes |
|---|---|---|---|
| `artifact_key` | `str` | yes | S3 key of source audio (already validated/probed) |
| `model_version_id` | `str` | yes | pinned pyannote version; recorded on the result (ADR-0005) |
| `params.num_speakers` | `int \| null` | no | exact speaker count hint; null = auto |
| `params.min_speakers` | `int \| null` | no | lower bound when auto |
| `params.max_speakers` | `int \| null` | no | upper bound when auto |

Audio is decoded to 16 kHz mono WAV before inference (reuse the existing
`convert-to-wav` helper used by `transcribe.py`). Decoding happens in a **gVisor
sandbox** (untrusted bytes).

`params` MUST validate against the `params_schema` (JSON Schema) stored on the
`model_versions` row for this `model_version_id`.

## Output contract

Return a dict shaped for `Result.result` (JSONB), consistent with
`PRODUCTION_DESIGN §7.2`:

```json
{
  "num_speakers": 2,
  "segments": [
    {"start": 0.00, "end": 4.20, "speaker": "SPEAKER_00"},
    {"start": 4.20, "end": 9.85, "speaker": "SPEAKER_01"}
  ],
  "model_version_id": "pyannote-speaker-diarization-3.1@<sha256>",
  "duration_seconds": 132.4
}
```

- `segments` MUST be sorted by `start`, non-negative, `start < end`, and within
  `[0, duration_seconds]`.
- Speaker labels are stable **within a result** (`SPEAKER_00`, `SPEAKER_01`,
  …); they are NOT stable across artifacts (no cross-file speaker identity).
- Determinism: pinned `model_version_id` + fixed seed ⇒ reproducible segments
  for the same input (ADR-0005). Document any non-determinism (e.g. clustering
  ties) if it cannot be eliminated.

## `align` output (companion step, contract only)

`align(transcript_segments, diarize_segments)` → enriched transcript segments.
Speaker per transcript segment = the diarization speaker with **maximum temporal
overlap**; emit `confidence = overlap_fraction`:

```json
{"start": 0.0, "end": 4.2, "text": "Hello and welcome", "speaker": "SPEAKER_00", "confidence": 0.94}
```

Ties / no-overlap → `speaker: null`, `confidence: 0.0` (do not guess).

---

## Errors

Raise a typed processor error (mirror `TranscribeError` in `transcribe.py`), and
classify each as **retriable** or **terminal** so Temporal's retry policy is
correct:

| Condition | Class |
|---|---|
| GPU OOM / replica evicted | retriable |
| Ray deployment unavailable / timeout | retriable |
| model sha256 mismatch on load | terminal (integrity failure — refuse) |
| corrupt/undecodable audio | terminal |
| params fail `params_schema` validation | terminal |
| pyannote HF token missing/invalid | terminal (config error) |

## Resource envelope

- Runs against the A10G `pyannote-3.1` Ray deployment (design #10 §2.1).
- Not batched (one recording at a time; longer than a whisper chunk).
- Heartbeat to Temporal during inference so cancellation is responsive
  (design #9 §2.2).
- gVisor sandbox, no network egress except the Ray service + S3 VPC endpoint.
- Record `gpu_seconds` for cost attribution (`job_costs`, §7.2).

## Definition of done

- [ ] `diarize` registered in the processor registry; unit + property tests
      (segments sorted, bounded, non-overlapping-per-speaker where expected).
- [ ] Calls the Ray `pyannote-3.1` deployment; weights from the checksum-verified
      registry; `model_version_id` recorded on the result.
- [ ] `params_schema` validation; typed errors classified retriable/terminal.
- [ ] `align` step + `diarize+align` Temporal workflow (designs #9/#10).
- [ ] Quality eval set (DER on a labeled sample) + version-pinning docs.
- [ ] `gpu_seconds` cost recorded; gVisor + no-egress verified.
