"""PII detection + redaction (PRD 08).

A pluggable detector finds PII spans in text; a redactor masks them. The
built-in ``RegexDetector`` covers structured PII (email, phone, SSN,
credit-card with a Luhn check, IP) with no dependencies. ``PresidioDetector``
(optional ``pii`` extra) adds ML NER for PERSON/ADDRESS/etc. Selection is via
``get_detector()``.

Also exposes ``scrub_log_text`` used by the logging layer so PII never lands in
application logs (the platform guarantee).
"""

from __future__ import annotations

import hashlib
import os
import re
from typing import Protocol

# --- detection --------------------------------------------------------------

_EMAIL = re.compile(r"\b[A-Za-z0-9._%+-]+@[A-Za-z0-9.-]+\.[A-Za-z]{2,}\b")
_PHONE = re.compile(r"(?<!\d)(?:\+?\d{1,2}[\s.-]?)?(?:\(?\d{3}\)?[\s.-]?)\d{3}[\s.-]?\d{4}(?!\d)")
_SSN = re.compile(r"\b\d{3}-\d{2}-\d{4}\b")
_CC = re.compile(r"\b(?:\d[ -]?){13,16}\b")
_IP = re.compile(r"\b(?:(?:25[0-5]|2[0-4]\d|[01]?\d?\d)\.){3}(?:25[0-5]|2[0-4]\d|[01]?\d?\d)\b")

DEFAULT_ENTITIES = ["EMAIL", "PHONE", "SSN", "CREDIT_CARD", "IP"]


def _luhn_ok(digits: str) -> bool:
    ds = [int(c) for c in digits if c.isdigit()]
    if not 13 <= len(ds) <= 16:
        return False
    total, parity = 0, len(ds) % 2
    for i, d in enumerate(ds):
        if i % 2 == parity:
            d *= 2
            if d > 9:
                d -= 9
        total += d
    return total % 10 == 0


class Span:
    __slots__ = ("start", "end", "entity", "text")

    def __init__(self, start: int, end: int, entity: str, text: str):
        self.start, self.end, self.entity, self.text = start, end, entity, text


class Detector(Protocol):
    def detect(self, text: str, entities: list[str]) -> list[Span]: ...


class RegexDetector:
    """Dependency-free structured-PII detector."""

    _PATTERNS = {"EMAIL": _EMAIL, "PHONE": _PHONE, "SSN": _SSN, "IP": _IP}

    def detect(self, text: str, entities: list[str]) -> list[Span]:
        want = set(entities or DEFAULT_ENTITIES)
        spans: list[Span] = []
        for entity, pat in self._PATTERNS.items():
            if entity not in want:
                continue
            for m in pat.finditer(text):
                spans.append(Span(m.start(), m.end(), entity, m.group()))
        if "CREDIT_CARD" in want:
            for m in _CC.finditer(text):
                if _luhn_ok(m.group()):
                    spans.append(Span(m.start(), m.end(), "CREDIT_CARD", m.group()))
        # Resolve overlaps: keep the longest span at each position.
        spans.sort(key=lambda s: (s.start, -(s.end - s.start)))
        resolved: list[Span] = []
        last_end = -1
        for s in spans:
            if s.start >= last_end:
                resolved.append(s)
                last_end = s.end
        return resolved


class PresidioDetector:
    """ML NER via Microsoft Presidio (optional ``pii`` extra)."""

    def __init__(self) -> None:
        from presidio_analyzer import AnalyzerEngine  # type: ignore  # noqa: PLC0415

        self._engine = AnalyzerEngine()

    def detect(self, text: str, entities: list[str]) -> list[Span]:
        results = self._engine.analyze(text=text, language="en")
        want = set(entities or [])
        spans: list[Span] = []
        for r in results:
            ent = r.entity_type
            if want and ent not in want:
                continue
            spans.append(Span(r.start, r.end, ent, text[r.start : r.end]))
        return spans


def get_detector() -> Detector:
    """Return Presidio when the extra is installed + enabled, else regex."""
    if os.environ.get("ORPHEUS_PII_ENGINE", "").lower() == "presidio":
        try:
            return PresidioDetector()
        except Exception:  # pragma: no cover - optional extra
            pass
    return RegexDetector()


# --- redaction --------------------------------------------------------------


def _mask_for(span: Span, mode: str) -> str:
    if mode == "char":
        return "●" * max(4, len(span.text))
    if mode == "hash":
        return f"<{hashlib.sha256(span.text.encode()).hexdigest()[:8]}>"
    return f"[{span.entity}]"  # type (default)


def redact_text(
    text: str,
    detector: Detector,
    entities: list[str],
    mask: str = "type",
) -> tuple[str, dict[str, int], dict[str, str]]:
    """Return (redacted_text, {entity: count}, {original: masked})."""
    spans = detector.detect(text, entities)
    if not spans:
        return text, {}, {}
    spans.sort(key=lambda s: s.start, reverse=True)  # right-to-left keeps offsets valid
    counts: dict[str, int] = {}
    mapping: dict[str, str] = {}
    out = text
    for s in spans:
        masked = _mask_for(s, mask)
        out = out[: s.start] + masked + out[s.end :]
        counts[s.entity] = counts.get(s.entity, 0) + 1
        mapping[s.text] = masked
    return out, counts, mapping


def redact_transcript(
    transcript: dict,
    entities: list[str] | None = None,
    mask: str = "type",
    detector: Detector | None = None,
) -> tuple[dict, list[dict], dict[str, str]]:
    """Redact text/segments/words in place. Returns (transcript,
    redactions_summary, mapping)."""
    det = detector or get_detector()
    ents = entities or DEFAULT_ENTITIES
    total: dict[str, int] = {}
    mapping: dict[str, str] = {}

    def apply(s: str) -> str:
        red, counts, m = redact_text(s, det, ents, mask)
        for k, v in counts.items():
            total[k] = total.get(k, 0) + v
        mapping.update(m)
        return red

    if "text" in transcript and isinstance(transcript["text"], str):
        transcript["text"] = apply(transcript["text"])
    for seg in transcript.get("segments", []) or []:
        if isinstance(seg.get("text"), str):
            seg["text"] = apply(seg["text"])
        for word in seg.get("words", []) or []:
            if isinstance(word.get("word"), str):
                word["word"] = apply(word["word"])
    summary = [{"entity_type": k, "count": v} for k, v in sorted(total.items())]
    return transcript, summary, mapping


def maybe_redact(transcript: dict, params: dict) -> list[dict]:
    """If params.redact.enabled, redact the transcript in place and return the
    redaction summary; else a no-op returning []. Used by processors so PII is
    masked before translation/summarization/subtitle export sees it."""
    cfg = params.get("redact")
    if not isinstance(cfg, dict) or not cfg.get("enabled"):
        return []
    _, summary, _ = redact_transcript(
        transcript, entities=cfg.get("entities"), mask=cfg.get("mask", "type")
    )
    return summary


# --- logging guarantee ------------------------------------------------------

# Keys whose values are never emitted to logs (transcript/result payloads).
_DENYLIST_KEYS = {"result", "text", "transcript", "segments", "words", "summary", "translation"}


def scrub_log_text(text: str) -> str:
    """Mask any structured PII in a free-text log message."""
    if not text:
        return text
    red, _, _ = redact_text(text, RegexDetector(), DEFAULT_ENTITIES, mask="type")
    return red


def scrub_log_event(event_dict: dict) -> dict:
    """structlog processor: drop denylisted payload keys and scrub the message."""
    for k in list(event_dict.keys()):
        if k in _DENYLIST_KEYS:
            event_dict[k] = "[redacted]"
    if isinstance(event_dict.get("event"), str):
        event_dict["event"] = scrub_log_text(event_dict["event"])
    return event_dict
