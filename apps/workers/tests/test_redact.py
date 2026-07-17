"""Tests for PII redaction (PRD 08) with the regex detector."""

from __future__ import annotations

from orpheus_workers.redact import (
    RegexDetector,
    maybe_redact,
    redact_text,
    redact_transcript,
    scrub_log_event,
    scrub_log_text,
)

DET = RegexDetector()


def test_detect_structured_pii():
    text = "email me at jane.doe@example.com or call (415) 555-1234, ssn 123-45-6789"
    red, counts, mapping = redact_text(text, DET, ["EMAIL", "PHONE", "SSN"], mask="type")
    assert "[EMAIL]" in red and "[PHONE]" in red and "[SSN]" in red
    assert "jane.doe@example.com" not in red
    assert counts == {"EMAIL": 1, "PHONE": 1, "SSN": 1}
    assert mapping["jane.doe@example.com"] == "[EMAIL]"


def test_credit_card_luhn():
    # 4111111111111111 passes Luhn; 4111111111111112 fails.
    valid = "card 4111 1111 1111 1111 here"
    invalid = "num 4111 1111 1111 1112 nope"
    r1, c1, _ = redact_text(valid, DET, ["CREDIT_CARD"], mask="type")
    r2, c2, _ = redact_text(invalid, DET, ["CREDIT_CARD"], mask="type")
    assert c1 == {"CREDIT_CARD": 1} and "[CREDIT_CARD]" in r1
    assert c2 == {} and "4111 1111 1111 1112" in r2


def test_mask_modes():
    text = "reach me@x.com"
    assert "●" in redact_text(text, DET, ["EMAIL"], mask="char")[0]
    hashed = redact_text(text, DET, ["EMAIL"], mask="hash")[0]
    assert hashed.startswith("reach <") and hashed.endswith(">")


def test_redact_transcript_segments_words():
    transcript = {
        "text": "call 415-555-1234",
        "segments": [
            {"start": 0, "end": 1, "text": "call 415-555-1234", "words": [{"word": "415-555-1234"}]}
        ],
    }
    red, summary, mapping = redact_transcript(transcript, entities=["PHONE"], mask="type")
    assert "[PHONE]" in red["text"]
    assert red["segments"][0]["text"].endswith("[PHONE]")
    assert red["segments"][0]["words"][0]["word"] == "[PHONE]"
    assert {"entity_type": "PHONE", "count": 3} in summary


def test_maybe_redact_noop_without_flag():
    t = {"text": "email a@b.com"}
    assert maybe_redact(t, {}) == []
    assert t["text"] == "email a@b.com"  # untouched
    assert maybe_redact(t, {"redact": {"enabled": True, "entities": ["EMAIL"]}})
    assert "a@b.com" not in t["text"]


def test_scrub_log_guarantees():
    # Free-text message with PII is masked.
    assert "a@b.com" not in scrub_log_text("user a@b.com failed")
    # Denylisted payload keys are dropped; message scrubbed.
    ev = scrub_log_event({"event": "job done a@b.com", "result": {"text": "secret"}, "job_id": "1"})
    assert ev["result"] == "[redacted]"
    assert "a@b.com" not in ev["event"]
    assert ev["job_id"] == "1"  # non-PII fields preserved
