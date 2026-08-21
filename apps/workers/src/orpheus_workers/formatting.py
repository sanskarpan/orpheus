"""Inverse text normalization (ITN) + smart formatting for transcripts (PRD 11 §4.2).

Rewrites spoken-form ASR output into written form — "twenty twenty six" → "2026",
"five dollars" → "$5", "three thirty pm" → "3:30 pm", "twenty percent" → "20%",
"first" → "1st" — plus sentence truecasing. Deterministic rule engine (no model),
so it runs on the worker CPU with zero extra dependencies.

The unit of work is the **word list**: number runs are collapsed into a single
written token whose `start`/`end` span the source words, keeping `words[]`
monotonic for subtitles and streaming. When a segment has no `words[]`, the
segment text is normalized as a string.

Public API:
    format_transcript(transcript: dict, opts: dict) -> dict   # in-place-ish, returns it
    format_text(text: str) -> str                             # string-only convenience
"""

from __future__ import annotations

import re
from typing import Any

# --- number-word vocabulary -------------------------------------------------

_UNITS = {
    "zero": 0,
    "one": 1,
    "two": 2,
    "three": 3,
    "four": 4,
    "five": 5,
    "six": 6,
    "seven": 7,
    "eight": 8,
    "nine": 9,
    "ten": 10,
    "eleven": 11,
    "twelve": 12,
    "thirteen": 13,
    "fourteen": 14,
    "fifteen": 15,
    "sixteen": 16,
    "seventeen": 17,
    "eighteen": 18,
    "nineteen": 19,
}
_TENS = {
    "twenty": 20,
    "thirty": 30,
    "forty": 40,
    "fifty": 50,
    "sixty": 60,
    "seventy": 70,
    "eighty": 80,
    "ninety": 90,
}
_SCALES = {"hundred": 100, "thousand": 1_000, "million": 1_000_000, "billion": 1_000_000_000}
_ORDINAL_WORDS = {
    "first": 1,
    "second": 2,
    "third": 3,
    "fourth": 4,
    "fifth": 5,
    "sixth": 6,
    "seventh": 7,
    "eighth": 8,
    "ninth": 9,
    "tenth": 10,
    "eleventh": 11,
    "twelfth": 12,
    "thirteenth": 13,
    "twentieth": 20,
    "thirtieth": 30,
}
_ORDINAL_SUFFIX = {"twenty": "ieth", "thirty": "ieth"}  # unused placeholder for symmetry
_NUMBER_WORDS = set(_UNITS) | set(_TENS) | set(_SCALES) | {"and", "point", "hundred"}

_MONTHS = {
    "january": 1,
    "february": 2,
    "march": 3,
    "april": 4,
    "may": 5,
    "june": 6,
    "july": 7,
    "august": 8,
    "september": 9,
    "october": 10,
    "november": 11,
    "december": 12,
}


def _clean(tok: str) -> str:
    return tok.lower().strip(" ,.!?;:")


def _words_to_int(tokens: list[str]) -> int | None:
    """Parse a list of number words (no scale ambiguity) into an integer.

    Handles units/teens/tens, "hundred"/"thousand"/... scaling, and "and".
    Returns None if the run isn't a parseable number.
    """
    if not tokens:
        return None
    total = 0
    current = 0
    seen = False
    for raw in tokens:
        w = _clean(raw)
        if w == "and":
            continue
        if w in _UNITS:
            current += _UNITS[w]
            seen = True
        elif w in _TENS:
            current += _TENS[w]
            seen = True
        elif w == "hundred":
            current = (current or 1) * 100
            seen = True
        elif w in _SCALES:
            total += (current or 1) * _SCALES[w]
            current = 0
            seen = True
        else:
            return None
    if not seen:
        return None
    return total + current


def _year_like(tokens: list[str]) -> int | None:
    """ "twenty twenty six" / "nineteen ninety nine" → 2026 / 1999.

    A two-chunk pattern where each chunk is a 1–99 number and the whole reads as
    a 4-digit year.
    """
    parts = [_clean(t) for t in tokens if _clean(t) not in ("and",)]
    if len(parts) not in (2, 3):
        return None
    # split into two halves by tens/units grouping
    vals: list[int] = []
    i = 0
    while i < len(parts):
        w = parts[i]
        if w in _TENS and i + 1 < len(parts) and parts[i + 1] in _UNITS:
            vals.append(_TENS[w] + _UNITS[parts[i + 1]])
            i += 2
        elif w in _TENS:
            vals.append(_TENS[w])
            i += 1
        elif w in _UNITS:
            vals.append(_UNITS[w])
            i += 1
        else:
            return None
    if len(vals) == 2 and 10 <= vals[0] <= 99 and 0 <= vals[1] <= 99:
        return vals[0] * 100 + vals[1]
    return None


_ORDINAL_SUFFIXES = {1: "st", 2: "nd", 3: "rd"}


def _ordinal(n: int) -> str:
    if 10 <= n % 100 <= 20:
        suf = "th"
    else:
        suf = _ORDINAL_SUFFIXES.get(n % 10, "th")
    return f"{n}{suf}"


class _Tok:
    """A working token carrying optional source timing (for words[] remapping)."""

    __slots__ = ("text", "start", "end", "confidence")

    def __init__(self, text: str, start: float | None, end: float | None, confidence: float):
        self.text = text
        self.start = start
        self.end = end
        self.confidence = confidence


_MERIDIEM = {"am", "a.m.", "a.m", "pm", "p.m.", "p.m", "oclock", "o'clock"}


def _time_from_words(words: list[str]) -> str | None:
    """Parse number words as a clock time → "H" or "H:MM" (None if not time-like)."""
    parts = [_clean(t) for t in words if _clean(t) != "and"]
    vals: list[int] = []
    i = 0
    while i < len(parts):
        w = parts[i]
        if w in _TENS and i + 1 < len(parts) and parts[i + 1] in _UNITS:
            vals.append(_TENS[w] + _UNITS[parts[i + 1]])
            i += 2
        elif w in _TENS:
            vals.append(_TENS[w])
            i += 1
        elif w in _UNITS:
            vals.append(_UNITS[w])
            i += 1
        else:
            return None
    if len(vals) == 1 and 0 <= vals[0] <= 23:
        return str(vals[0])
    if len(vals) == 2 and 0 <= vals[0] <= 23 and 0 <= vals[1] <= 59:
        return f"{vals[0]}:{vals[1]:02d}"
    return None


def _run_written_form(run: list[_Tok], following: str | None) -> tuple[str | None, bool]:
    """Convert a number-word run to written form given the next word's context.

    Returns ``(written, consumed_following)`` — ``consumed_following`` is True when
    the trailing currency/percent/meridiem word was folded into the token.
    """
    words = [t.text for t in run]
    nxt = _clean(following) if following else ""
    if nxt in _MERIDIEM:
        tf = _time_from_words(words)
        if tf is not None:
            if nxt.startswith("a"):
                return f"{tf} am", True
            if nxt.startswith("p"):
                return f"{tf} pm", True
            return (tf if ":" in tf else f"{tf}:00"), True  # o'clock
    year = _year_like(words)
    n = year if year is not None else _words_to_int(words)
    if n is None:
        return None, False
    if nxt in ("dollars", "dollar", "bucks"):
        return f"${n}", True
    if nxt in ("cents", "cent"):
        return f"{n}¢", True
    if nxt in ("percent", "percentage"):
        return f"{n}%", True
    return str(n), False


def _apply_itn_tokens(tokens: list[_Tok]) -> list[_Tok]:
    """Collapse number-word runs into single written tokens (span-preserving)."""
    out: list[_Tok] = []
    i = 0
    while i < len(tokens):
        w = _clean(tokens[i].text)
        # ordinal words ("first" → "1st")
        if w in _ORDINAL_WORDS:
            t = tokens[i]
            out.append(_Tok(_ordinal(_ORDINAL_WORDS[w]), t.start, t.end, t.confidence))
            i += 1
            continue
        if w in _NUMBER_WORDS and w not in ("and", "point"):
            j = i
            run: list[_Tok] = []
            while j < len(tokens) and _clean(tokens[j].text) in _NUMBER_WORDS:
                run.append(tokens[j])
                j += 1
            # drop a trailing dangling "and"
            while run and _clean(run[-1].text) == "and":
                run.pop()
                j -= 1
            following = tokens[j].text if j < len(tokens) else None
            written, consumed = _run_written_form(run, following)
            if written is not None:
                span_end = run[-1].end
                if consumed and j < len(tokens):
                    span_end = tokens[j].end
                    j += 1
                out.append(_Tok(written, run[0].start, span_end, run[0].confidence))
            else:
                out.extend(run)  # not a parseable number after all
            i = j
            continue
        out.append(tokens[i])
        i += 1
    return out


_TIME_RE = re.compile(r"\b(\d{1,2})[:\s]?(\d{2})?\s*(a\.?m\.?|p\.?m\.?|o'?clock)\b", re.IGNORECASE)


def _format_times(text: str) -> str:
    """ "3 30 pm" / "three thirty pm" (post-ITN → "3 30 pm") → "3:30 pm"."""

    def repl(m: re.Match) -> str:
        hh, mm, mer = m.group(1), m.group(2) or "00", m.group(3).lower().replace(".", "")
        if mer.startswith("o"):
            return f"{int(hh)}:00"
        return f"{int(hh)}:{mm} {mer}"

    return _TIME_RE.sub(repl, text)


_SENT_SPLIT = re.compile(r"([.!?]\s+|^)([a-z])")


def _truecase(text: str) -> str:
    """Capitalize sentence starts and the pronoun "I"."""
    text = _SENT_SPLIT.sub(lambda m: m.group(1) + m.group(2).upper(), text)
    text = re.sub(r"\bi\b", "I", text)
    return text


def _tokens_from_words(words: list[dict[str, Any]]) -> list[_Tok]:
    return [
        _Tok(
            str(w.get("word", "")),
            float(w.get("start", 0.0)),
            float(w.get("end", 0.0)),
            float(w.get("confidence", 0.0) or 0.0),
        )
        for w in words
    ]


def _words_from_tokens(tokens: list[_Tok]) -> list[dict[str, Any]]:
    out = []
    for t in tokens:
        d: dict[str, Any] = {"word": t.text}
        if t.start is not None:
            d["start"] = t.start
        if t.end is not None:
            d["end"] = t.end
        d["confidence"] = t.confidence
        out.append(d)
    return out


def format_text(
    text: str, itn: bool = True, punctuation: bool = False, truecase: bool = True
) -> str:
    """Normalize a plain string (used when a segment has no word timings)."""
    if itn:
        toks = [_Tok(w, None, None, 0.0) for w in text.split()]
        toks = _apply_itn_tokens(toks)
        text = " ".join(t.text for t in toks)
        text = _format_times(text)
    if truecase:
        text = _truecase(text)
    return text


def format_transcript(transcript: dict, opts: dict) -> dict:
    """Apply ITN/formatting to a transcript in place, honoring ``opts``.

    ``opts``: ``{enabled, itn, punctuation, truecase}`` (itn/truecase default True
    once enabled). Rewrites ``segments[].text`` (+ ``words[]`` span-preserving) and
    the top-level ``text``. No-op when ``enabled`` is falsy.
    """
    if not opts or not opts.get("enabled"):
        return transcript
    itn = opts.get("itn", True)
    do_truecase = opts.get("truecase", True)
    punctuation = opts.get("punctuation", False)

    segments = transcript.get("segments") or []
    for seg in segments:
        words = seg.get("words")
        if itn and words:
            toks = _apply_itn_tokens(_tokens_from_words(words))
            seg["words"] = _words_from_tokens(toks)
            new_text = " ".join(t.text for t in toks)
            new_text = _format_times(new_text)
            seg["text"] = _truecase(new_text) if do_truecase else new_text
        else:
            seg["text"] = format_text(
                seg.get("text", ""), itn=itn, punctuation=punctuation, truecase=do_truecase
            )
    if segments:
        transcript["text"] = " ".join(s.get("text", "") for s in segments).strip()
    elif transcript.get("text"):
        transcript["text"] = format_text(
            transcript["text"], itn=itn, punctuation=punctuation, truecase=do_truecase
        )
    return transcript
