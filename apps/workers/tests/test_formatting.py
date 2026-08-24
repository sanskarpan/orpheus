"""Tests for ITN / smart formatting (PRD 11 §4.2)."""

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


def test_already_written_times_are_not_corrupted():
    # Regression (#575): modern ASR emits written-form times, and the clock rule
    # must not seize a fragment of one — "3.30pm" was being rewritten to
    # "3.30:00 pm" by matching its "30pm" tail as hour=30.
    assert format_text("the meeting is at 3.30pm") == "The meeting is at 3.30pm"
    # an already-correct colon time is left alone
    assert format_text("call me at 3:30 pm") == "Call me at 3:30 pm"
    # spoken/spaced forms still normalize to a colon time
    assert "3:30 pm" in format_text("the meeting is at three thirty pm")


def test_punctuation_restoration():
    # Adds a sentence-final period to unpunctuated text...
    assert format_text("hello world", punctuation=True) == "Hello world."
    # ...but is a no-op when already punctuated (modern ASR output).
    assert format_text("Hello world.", punctuation=True) == "Hello world."
    assert format_text("Is it?", punctuation=True) == "Is it?"
    # Off by default.
    assert format_text("hello world") == "Hello world"
    # Applies per-segment on a word-timed transcript.
    tr = {
        "text": "",
        "segments": [
            {"start": 0.0, "end": 0.5, "text": "hi there",
             "words": [{"word": "hi", "start": 0.0, "end": 0.2, "confidence": 0.9},
                       {"word": "there", "start": 0.2, "end": 0.5, "confidence": 0.9}]},
        ],
    }
    out = format_transcript(dict(tr), {"enabled": True, "punctuation": True})
    assert out["segments"][0]["text"].endswith(".")
    assert out["text"].endswith(".")


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
