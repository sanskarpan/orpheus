FROM mwader/staticffmpeg:7.1.1 AS ffmpeg

ARG WHISPER_MODEL=tiny.en
ARG WHISPER_DIR=/models
FROM python:3.12-slim AS whisper
RUN pip install --no-cache-dir faster-whisper
RUN mkdir -p /models && \
    python -c "from faster_whisper import WhisperModel; WhisperModel('${WHISPER_MODEL}', download_root='${WHISPER_DIR}')"

FROM python:3.12-slim

WORKDIR /app

ARG ORPHEUS_WORKERS_PROCESS=all

RUN apt-get update && apt-get install -y --no-install-recommends curl ca-certificates && rm -rf /var/lib/apt/lists/*

RUN pip install --no-cache-dir uv==0.5.10
COPY --from=ffmpeg /ffmpeg /usr/local/bin/ffmpeg
COPY --from=ffmpeg /ffprobe /usr/local/bin/ffprobe
COPY --from=whisper /models /models
ENV ORPHEUS_WORKER_WHISPER_DIR=/models
ENV ORPHEUS_WORKER_WHISPER_MODEL=tiny.en
COPY pyproject.toml uv.lock /app/
COPY apps /app/apps
COPY packages /app/packages
RUN uv sync --all-packages --frozen

EXPOSE 8081 50051 8082

COPY apps/workers/entrypoint.sh /entrypoint.sh
RUN chmod +x /entrypoint.sh
ENTRYPOINT ["/entrypoint.sh"]
