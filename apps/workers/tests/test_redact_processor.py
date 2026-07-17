"""text.redact processor test (PRD 08) — redacts + writes a pii_mapping artifact."""

from __future__ import annotations

from orpheus_workers.processors.redact import redact_proc


class FakeDB:
    def __init__(self, transcript):
        self.transcript = transcript
        self.inserted = []

    def fetchrow(self, sql, *args):
        if "org_id, artifact_id, params" in sql:
            return {
                "org_id": "org-1",
                "artifact_id": None,
                "params": {"source_job_id": "j0", "keep_mapping": True},
            }
        if "SELECT result FROM jobs" in sql:
            return {"result": self.transcript}
        if "INSERT INTO artifacts" in sql:
            self.inserted.append(args)
            return {"id": "mapping-artifact-1"}
        raise AssertionError(f"unexpected sql: {sql}")


class FakeS3:
    def __init__(self):
        self.uploaded = {}

    def upload_file(self, bucket, key, src, content_type=None):
        from pathlib import Path

        self.uploaded[key] = Path(src).read_bytes()
        return len(self.uploaded[key])


async def test_redact_processor_masks_and_writes_mapping(tmp_path):
    transcript = {
        "text": "call jane at jane@x.com or 415-555-1234",
        "segments": [{"start": 0, "end": 1, "text": "jane@x.com"}],
        "language": "en",
    }
    db = FakeDB(transcript)
    s3 = FakeS3()
    ctx = {"db": db, "s3": s3, "bucket": "b", "work_dir": str(tmp_path)}

    res = await redact_proc(ctx, "j1")
    assert "jane@x.com" not in res["text"]
    assert "[EMAIL]" in res["text"] and "[PHONE]" in res["text"]
    assert res["segments"][0]["text"] == "[EMAIL]"
    ents = {r["entity_type"] for r in res["redactions"]}
    assert "EMAIL" in ents and "PHONE" in ents
    # keep_mapping → a pii_mapping artifact was inserted + uploaded.
    assert res["mapping_artifact_id"] == "mapping-artifact-1"
    assert len(db.inserted) == 1
    assert any("pii-mappings/org-1/j1.json" in k for k in s3.uploaded)
