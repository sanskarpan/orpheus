"""Shared HTTP client for Modal GPU endpoints.

Modal's ``@modal.fastapi_endpoint`` answers fast calls synchronously, but when a
call outlives the sync window (e.g. a GPU cold start + first-run model download)
it returns a **303** to a polling URL and keeps 303-ing until the result is ready.
A plain ``httpx.post`` errors on that redirect, and even ``follow_redirects=True``
gives up at the default 20-redirect cap. ``modal_post_json`` follows the poll
chain with a large redirect budget so the call simply waits for the result.
"""

from __future__ import annotations

from typing import Any

# Generous cap: Modal poll-redirects are server-side long-polls, so a large budget
# means "wait for the result" while the function's own timeout bounds the total.
_MAX_REDIRECTS = 1000


def modal_post_json(url: str, payload: dict, timeout: float = 600.0) -> Any:
    """POST JSON to a Modal endpoint and return the (raise_for_status'd) response,
    transparently following the cold-start 303 polling chain."""
    import httpx

    with httpx.Client(
        follow_redirects=True, max_redirects=_MAX_REDIRECTS, timeout=timeout
    ) as client:
        resp = client.post(url, json=payload)
    resp.raise_for_status()
    return resp
