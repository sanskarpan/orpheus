"""Tests for the transcript embedding index (PRD 06 #359/#360)."""

from __future__ import annotations

from orpheus_workers.embeddings import StubTextEmbedder, cosine, get_text_embedder, rank_by_similarity
from orpheus_workers.processors.transcript_index import ask_proc, index_proc, search_proc

SEG = [
    {"start": 0.0, "end": 3.0, "text": "the mitochondria is the powerhouse of the cell"},
    {"start": 3.0, "end": 6.0, "text": "we will ship the new pricing on friday"},
    {"start": 6.0, "end": 9.0, "text": ""},  # empty → not indexed
]


def test_stub_embedder_deterministic_unit_norm():
    e = StubTextEmbedder()
    a = e.embed(["hello world"])[0]
    b = e.embed(["hello world"])[0]
    assert a == b
    assert abs(sum(x * x for x in a) - 1.0) < 1e-6
    assert cosine(a, b) > 0.999  # identical text ≈ 1.0


def test_default_embedder_is_stub(monkeypatch):
    monkeypatch.delenv("ORPHEUS_MODAL_EMBED_TEXT_URL", raising=False)
    assert isinstance(get_text_embedder(), StubTextEmbedder)


def test_rank_by_similarity_orders_and_guards_dim():
    q = [1.0, 0.0]
    rows = [
        {"text": "a", "embedding": [1.0, 0.0]},
        {"text": "b", "embedding": [0.0, 1.0]},
        {"text": "c", "embedding": [1.0, 0.0, 0.0]},  # wrong dim → skipped
    ]
    hits = rank_by_similarity(q, rows, top_k=2)
    assert [h["text"] for h in hits] == ["a", "b"]
    assert hits[0]["score"] == 1.0


class _IndexDB:
    def __init__(self):
        self.inserted: list = []
        self.deleted = 0

    def fetchrow(self, sql, *a):
        if "id, org_id, artifact_id, params" in sql:
            return {"id": "idxjob", "org_id": "o", "artifact_id": "art-1",
                    "params": {"source_job_id": "srcjob"}}
        if "SELECT result FROM jobs" in sql:
            return {"result": {"segments": SEG, "text": "..."}}
        raise AssertionError(sql)

    def execute(self, sql, *a):
        if "DELETE FROM transcript_embeddings" in sql:
            self.deleted += 1

    def executemany(self, sql, rows):
        assert "INSERT INTO transcript_embeddings" in sql
        self.inserted = rows


async def test_index_proc_embeds_and_inserts(tmp_path, monkeypatch):
    monkeypatch.delenv("ORPHEUS_MODAL_EMBED_TEXT_URL", raising=False)
    db = _IndexDB()
    ctx = {"db": db, "s3": None, "bucket": "b", "work_dir": str(tmp_path)}
    res = await index_proc(ctx, "idxjob")
    assert res["indexed"] == 2  # empty segment skipped
    assert db.deleted == 1  # idempotent delete-then-insert
    # each row: (org, job, artifact, seg_idx, start, end, text, emb, dim, mv)
    org, jid, art, idx0, start0, _end, text0, emb0, dim0, _mv = db.inserted[0]
    assert org == "o" and jid == "srcjob" and art == "art-1"
    assert idx0 == 0 and dim0 == StubTextEmbedder.dim == len(emb0)
    assert text0.startswith("the mitochondria")


class _SearchDB:
    """Returns pre-embedded candidate rows so search/ask can rank them."""

    def __init__(self, query):
        e = StubTextEmbedder()
        self.query = query
        self.rows = [
            {"job_id": "j1", "artifact_id": "a1", "segment_index": 0,
             "start_seconds": 3.0, "end_seconds": 6.0,
             "text": SEG[1]["text"], "embedding": e.embed([SEG[1]["text"]])[0]},
            {"job_id": "j2", "artifact_id": "a2", "segment_index": 0,
             "start_seconds": 0.0, "end_seconds": 3.0,
             "text": SEG[0]["text"], "embedding": e.embed([SEG[0]["text"]])[0]},
        ]

    def fetchrow(self, sql, *a):
        if "id, org_id, params" in sql:
            return {"id": "q", "org_id": "o", "params": {"query": self.query, "top_k": 1}}
        raise AssertionError(sql)

    def fetchall(self, sql, *a):
        assert "FROM transcript_embeddings" in sql
        return self.rows


async def test_search_proc_returns_most_similar(tmp_path, monkeypatch):
    monkeypatch.delenv("ORPHEUS_MODAL_EMBED_TEXT_URL", raising=False)
    # query is exactly the pricing segment → it should rank #1
    db = _SearchDB(query="we will ship the new pricing on friday")
    ctx = {"db": db, "s3": None, "bucket": "b", "work_dir": str(tmp_path)}
    res = await search_proc(ctx, "q")
    assert res["results"]
    assert res["results"][0]["job_id"] == "j1"  # the pricing segment
    assert res["results"][0]["start"] == 3.0
    assert res["results"][0]["score"] > 0.99


async def test_ask_proc_answers_with_citations(tmp_path, monkeypatch):
    monkeypatch.delenv("ORPHEUS_MODAL_EMBED_TEXT_URL", raising=False)

    class FakeLLM:
        model_version_id = "fake"

        def complete(self, system, user, max_tokens=512):
            assert "pricing" in user  # the retrieved context reached the LLM
            return "They ship pricing on Friday [0]."

    monkeypatch.setattr("orpheus_workers.processors.transcript_index.get_llm", lambda: FakeLLM())
    db = _SearchDB(query="when do we ship pricing")
    ctx = {"db": db, "s3": None, "bucket": "b", "work_dir": str(tmp_path)}
    res = await ask_proc(ctx, "q")
    assert "Friday" in res["answer"]
    assert res["citations"] and res["citations"][0]["job_id"] in ("j1", "j2")
