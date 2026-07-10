#!/bin/sh
set -e
uv run --package orpheus-workers python -m orpheus_workers.control_plane &
uv run --package orpheus-workers python -m orpheus_workers.grpc_server &
uv run --package orpheus-workers python -m orpheus_workers.worker &
wait
