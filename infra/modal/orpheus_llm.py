"""Orpheus LLM service on Modal — an open instruct model behind an
OpenAI-compatible API, so the worker's ``openai-compat`` provider can run real
summarize/translate/detect-language on GPU without any external API key.

Serves vLLM's OpenAI server (``/v1/chat/completions`` etc.) for
**Qwen2.5-3B-Instruct** (open, non-gated) on an A10 GPU, gated by the shared
secret in the ``orpheus-modal-auth`` Secret (reused from the transcribe app).

Deploy:  modal deploy infra/modal/orpheus_llm.py

Wire the worker:
  ORPHEUS_LLM_PROVIDER=openai-compat
  ORPHEUS_LLM_BASE_URL=https://<workspace>--orpheus-llm-serve.modal.run/v1
  ORPHEUS_LLM_API_KEY=<ORPHEUS_MODAL_SHARED_SECRET>
  ORPHEUS_LLM_MODEL=orpheus-llm
"""

from __future__ import annotations

import modal

APP_NAME = "orpheus-llm"
MODEL = "Qwen/Qwen2.5-3B-Instruct"
SERVED_NAME = "orpheus-llm"
VLLM_PORT = 8000

image = (
    modal.Image.debian_slim(python_version="3.12")
    .pip_install("vllm==0.6.6.post1", "huggingface_hub[hf_transfer]==0.26.2")
    .env({"HF_HUB_ENABLE_HF_TRANSFER": "1", "HF_HOME": "/cache"})
)

app = modal.App(APP_NAME)
model_cache = modal.Volume.from_name("orpheus-llm-cache", create_if_missing=True)
auth = modal.Secret.from_name("orpheus-modal-auth")


@app.function(
    image=image,
    gpu="a10g",
    volumes={"/cache": model_cache},
    secrets=[auth],
    timeout=1800,
    min_containers=0,  # scale to zero when idle
    scaledown_window=300,
)
@modal.concurrent(max_inputs=8)
@modal.web_server(port=VLLM_PORT, startup_timeout=900)
def serve():
    """Launch vLLM's OpenAI-compatible server; Modal proxies its port.

    The vLLM ``--api-key`` gates every /v1 route with the shared secret, so the
    endpoint isn't open to the world.
    """
    import os
    import subprocess

    secret = os.environ["ORPHEUS_MODAL_SHARED_SECRET"]
    subprocess.Popen(
        [
            "vllm",
            "serve",
            MODEL,
            "--served-model-name",
            SERVED_NAME,
            "--host",
            "0.0.0.0",
            "--port",
            str(VLLM_PORT),
            "--api-key",
            secret,
            "--max-model-len",
            "16384",
            "--gpu-memory-utilization",
            "0.90",
            "--download-dir",
            "/cache",
        ]
    )
