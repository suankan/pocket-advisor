"""Self-test for the eval harness (eval.py). Pure scoring math, golden
validation, and compare exit codes — no subprocess call to query.py, no
embedding model, no real corpus (pattern of test_ingest_documents.py).

    venv/bin/python scripts/test_eval.py
"""
import shutil
import sys
import tempfile
from datetime import datetime, timezone
from pathlib import Path

import config

TMP = Path(tempfile.mkdtemp(prefix="pocket_advisor_eval_test_"))
config.PROJECT_ROOT = TMP
config.OUTPUT_DIR = TMP / "output"
config.DB_PATH = config.OUTPUT_DIR / "test.db"
config.VECTORS_DIR = config.OUTPUT_DIR / "vectors"
config.VECTORS_META_JSON = config.VECTORS_DIR / "vectors.meta.json"

import db     # noqa: E402
import eval as evalmod  # noqa: E402  ('eval' shadows the builtin only as a module name)

FAILURES = []


def check(name, cond, detail=""):
    status = "ok" if cond else "FAIL"
    print(f"  [{status}] {name}" + (f" — {detail}" if detail and not cond else ""))
    if not cond:
        FAILURES.append(name)


def make_corpus():
    config.OUTPUT_DIR.mkdir(parents=True, exist_ok=True)
    conn = db.connect()
    db.migrate(conn)
    now = datetime.now(timezone.utc).isoformat()
    conn.execute("INSERT INTO threads (id, representative_subject) VALUES (1, 'x')")
    for i, mid in enumerate(("m1", "m2", "m3"), start=1):
        conn.execute(
            "INSERT INTO emails (message_id, thread_id, ingested_at) VALUES (?, ?, ?)",
            (mid, 1, now))
    conn.commit()
    return conn


def test_score_question():
    print("score_question:")
    entry = {"id": "q1", "expect_any": ["m2"]}
    query_json = {"results": [{"message_id": "m1", "thread_id": 1},
                               {"message_id": "m2", "thread_id": 1},
                               {"message_id": "m3", "thread_id": 1}]}
    s = evalmod.score_question(entry, query_json)
    check("rank found at correct position", s["rank"] == 2)
    check("reciprocal rank", abs(s["rr"] - 0.5) < 1e-9)
    check("hit@1 false", s["hit@1"] is False)
    check("hit@5 true", s["hit@5"] is True)

    entry_miss = {"id": "q2", "expect_any": ["nope"]}
    s2 = evalmod.score_question(entry_miss, query_json)
    check("miss -> rank None", s2["rank"] is None)
    check("miss -> rr 0", s2["rr"] == 0.0)
    check("miss -> hit@15 false", s2["hit@15"] is False)

    entry_thread = {"id": "q3", "expect_thread": 1}
    s3 = evalmod.score_question(entry_thread, query_json)
    check("thread match at rank 1", s3["rank"] == 1)


def test_aggregate():
    print("aggregate:")
    golden_by_id = {"q1": {"flags": ["cross-lingual"]}, "q2": {"flags": []}}
    scored = [
        {"id": "q1", "rr": 1.0, "hit@1": True, "hit@5": True, "hit@15": True},
        {"id": "q2", "rr": 0.0, "hit@1": False, "hit@5": False, "hit@15": False},
    ]
    agg = evalmod.aggregate(scored, golden_by_id)
    check("mean mrr", abs(agg["mrr"] - 0.5) < 1e-9)
    check("mean hit@1", abs(agg["hit@1"] - 0.5) < 1e-9)
    check("by_flag slices cross-lingual", agg["by_flag"]["cross-lingual"]["n"] == 1)
    check("by_flag cross-lingual mrr", abs(agg["by_flag"]["cross-lingual"]["mrr"] - 1.0) < 1e-9)


def test_validate_golden():
    print("validate_golden:")
    conn = make_corpus()
    good = [{"id": "q1", "question": "x?", "expect_any": ["m1"]}]
    try:
        evalmod.validate_golden(conn, good)
        check("valid golden set passes", True)
    except SystemExit:
        check("valid golden set passes", False)

    bad = [{"id": "q1", "question": "x?", "expect_any": ["m1", "does-not-exist"]}]
    try:
        evalmod.validate_golden(conn, bad)
        check("unknown message_id aborts", False)
    except SystemExit as e:
        check("unknown message_id aborts", "does-not-exist" in str(e))

    dup = [{"id": "q1", "question": "a", "expect_any": ["m1"]},
           {"id": "q1", "question": "b", "expect_any": ["m2"]}]
    try:
        evalmod.validate_golden(conn, dup)
        check("duplicate id aborts", False)
    except SystemExit as e:
        check("duplicate id aborts", "duplicate id" in str(e))

    no_target = [{"id": "q1", "question": "x?"}]
    try:
        evalmod.validate_golden(conn, no_target)
        check("missing expect_any/expect_thread aborts", False)
    except SystemExit:
        check("missing expect_any/expect_thread aborts", True)
    conn.close()


def _fake_result(golden_sha, corpus, aggregates, questions):
    return {"fingerprint": {"golden_sha256": golden_sha, "corpus": corpus,
                           "golden_count": len(questions)},
            "aggregates": aggregates, "questions": questions}


def test_compare():
    print("compare:")
    qa = [{"id": "q1", "rank": 1}, {"id": "q2", "rank": None}]
    qb_same = [{"id": "q1", "rank": 1}, {"id": "q2", "rank": None}]
    a = _fake_result("sha1", {"emails": 10}, {"hit@1": 0.5, "hit@5": 0.5, "hit@15": 0.5, "mrr": 0.5}, qa)

    # improvement: q2 now found -> no regression -> exit 0
    b_improved = _fake_result("sha1", {"emails": 10},
                              {"hit@1": 0.5, "hit@5": 1.0, "hit@15": 1.0, "mrr": 0.75},
                              [{"id": "q1", "rank": 1}, {"id": "q2", "rank": 2}])
    _, code = evalmod.compute_comparison(a, b_improved)
    check("improvement exits 0", code == 0)

    # regression: hit@5 drops -> exit 1
    b_regressed = _fake_result("sha1", {"emails": 10},
                               {"hit@1": 0.0, "hit@5": 0.0, "hit@15": 0.0, "mrr": 0.0},
                               qb_same)
    _, code = evalmod.compute_comparison(a, b_regressed)
    check("regression exits 1", code == 1)

    # different golden set -> never gates, even if metrics look worse
    b_diff_golden = _fake_result("sha2", {"emails": 10},
                                 {"hit@1": 0.0, "hit@5": 0.0, "hit@15": 0.0, "mrr": 0.0},
                                 qb_same)
    report, code = evalmod.compute_comparison(a, b_diff_golden)
    check("golden mismatch never gates", code == 0)
    check("golden mismatch warns", "NOT directly comparable" in report)


def main():
    try:
        test_score_question()
        test_aggregate()
        test_validate_golden()
        test_compare()
    finally:
        shutil.rmtree(TMP, ignore_errors=True)

    if FAILURES:
        print(f"\n{len(FAILURES)} FAILURE(S): {FAILURES}")
        return 1
    print("\nAll eval.py self-tests passed.")
    return 0


if __name__ == "__main__":
    sys.exit(main())
