FROM python:3.12-slim

WORKDIR /app

RUN apt-get update && apt-get install -y --no-install-recommends curl && rm -rf /var/lib/apt/lists/*

RUN pip install --no-cache-dir uv==0.5.10
COPY pyproject.toml uv.lock /app/
COPY apps /app/apps
COPY packages /app/packages
RUN uv sync --all-packages --frozen

EXPOSE 8081 50051

COPY apps/workers/entrypoint.sh /entrypoint.sh
RUN chmod +x /entrypoint.sh
ENTRYPOINT ["/entrypoint.sh"]
