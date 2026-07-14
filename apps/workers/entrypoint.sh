#!/bin/sh
set -e
PROCESS="${ORPHEUS_WORKERS_PROCESS:-all}"
case "$PROCESS" in
    all)
        uv run --package orpheus-workers python -m orpheus_workers.control_plane &
        uv run --package orpheus-workers python -m orpheus_workers.grpc_server &
        uv run --package orpheus-workers python -m orpheus_workers.worker &
        wait
        ;;
    control)
        exec uv run --package orpheus-workers python -m orpheus_workers_control
        ;;
    grpc)
        exec uv run --package orpheus-workers python -m orpheus_workers_grpc
        ;;
    worker)
        exec uv run --package orpheus-workers python -m orpheus_workers_worker
        ;;
    *)
        echo "Unknown ORPHEUS_WORKERS_PROCESS: $PROCESS" >&2
        exit 1
        ;;
esac
