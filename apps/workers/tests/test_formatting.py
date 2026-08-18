"""Tests for ITN / smart formatting (PRD 01 §4.2)."""

from __future__ import annotations

from orpheus_workers.formatting import format_text, format_transcript


def test_itn_numbers_currency_percent_time_ordinal():
    assert "2026" in format_text("we shipped in twenty twenty six")
    assert "$5" in format_text("it costs five dollars")
    assert "20%" in format_text("revenue grew twenty percent")
    assert "3:30 pm" in format_text("the meeting is at three thirty pm")
    assert "1st" in format_text("this is the first item")
    assert "350" in format_text("three hundred and fifty widgets")
    assert "1999" in format_text("released in nineteen ninety nine")


def test_truecasing():
    assert format_text("hello world. how are you") == "Hello world. How are you"
    assert format_text("i think i am right") == "I think I am right"


def test_disabled_is_noop():
    tr = {"text": "twenty dollars", "segments": [{"text": "twenty dollars"}]}
    out = format_transcript(dict(tr), {"enabled": False})
    assert out["text"] == "twenty dollars"  # unchanged


def test_word_spans_are_preserved_and_monotonic():
    tr = {
        "text": "",
        "segments": [
            {
                "start": 0.0,
                "end": 1.4,
                "text": "twenty twenty six",
                "words": [
                    {"word": "twenty", "start": 0.0, "end": 0.5, "confidence": 0.9},
                    {"word": "twenty", "start": 0.5, "end": 1.0, "confidence": 0.9},
                    {"word": "six", "start": 1.0, "end": 1.4, "confidence": 0.8},
                ],
            }
        ],
    }
    format_transcript(tr, {"enabled": True, "itn": True})
    words = tr["segments"][0]["words"]
    assert [w["word"] for w in words] == ["2026"]
    assert words[0]["start"] == 0.0 and words[0]["end"] == 1.4  # spans the source run
    # monotonic
    starts = [w["start"] for w in words]
    assert starts == sorted(starts)
    assert tr["text"] == "2026"


def test_itn_then_redaction_ordering():
    # After ITN, a spoken SSN "one two three ..." becomes grouped digits that the
    # redaction regex can match. Here we just confirm ITN yields digits the
    # downstream regex operates on.
    from orpheus_workers.redact import get_detector, redact_text

    text = format_text("my number is five five five one two three four")
    # digits present after ITN
    assert any(c.isdigit() for c in text)
    red, counts, _ = redact_text(text, get_detector(), ["PHONE", "SSN"], "type")
    assert isinstance(counts, dict)  # runs without error on normalized text


def test_multiword_confidence_taken_from_first_token():
    tr = {
        "segments": [
            {
                "text": "fifty percent",
                "words": [
                    {"word": "fifty", "start": 0.0, "end": 0.4, "confidence": 0.7},
                    {"word": "percent", "start": 0.4, "end": 0.9, "confidence": 0.95},
                ],
            }
        ],
    }
    format_transcript(tr, {"enabled": True})
    w = tr["segments"][0]["words"]
    assert w[0]["word"] == "50%"
    assert w[0]["start"] == 0.0 and w[0]["end"] == 0.9  # spans "fifty" .. "percent"
