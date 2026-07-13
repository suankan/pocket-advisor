"""Retrieval eval harness (docs/specs/eval-harness.md, Phase 1a;
warm path: docs/specs/warm-eval.md).

Measures query.py's retrieval quality against a golden question set and
records every run with a full fingerprint (git commit, index identity,
corpus counts, retrieval config, golden-set hash) so any two runs are
honestly comparable. No accuracy-affecting change (pre-filter, reranker,
transliteration, ...) is called an improvement without a `compare` run.

    eval.py run --golden eval/golden/<name>.yaml [--label L] [--top-k N]
                [--mode warm|cold]
    eval.py compare <result_a.json> <result_b.json>
    eval.py list [--golden eval/golden/<name>.yaml]

Default --mode warm loads embed/rerank models once per run (not a
generative chat context). --mode cold spawns query.py per question
(old CLI-faithful behavior).

`eval/` is workspace data (golden sets + results contain case facts) —
entirely gitignored; see config.py for why no default path is baked in
here.
"""
import argparse
import hashlib
import json
import re
import subprocess
import sys
import time
from datetime import datetime, timezone
from pathlib import Path

import yaml

import config
import db

SCRIPT_DIR = Path(__file__).resolve().parent
TOP_KS = (1, 5, 15)
VALID_MODES = ("warm", "cold")


# ---- golden set ------------------------------------------------------

def load_golden(path):
    data = yaml.safe_load(Path(path).read_text()) or []
    if not isinstance(data, list):
        raise SystemExit(f"eval: golden set {path} must be a YAML list")
    return data


def validate_golden(conn, golden_list):
    """Abort (listing every offending id) rather than silently scoring
    against a golden set that no longer matches the corpus."""
    errors = []
    seen_ids = set()
    for e in golden_list:
        eid = e.get("id", "<missing id>")
        if "id" not in e or "question" not in e:
            errors.append(f"{eid}: missing required 'id' or 'question'")
            continue
        if eid in seen_ids:
            errors.append(f"{eid}: duplicate id")
        seen_ids.add(eid)
        if not e.get("expect_any") and e.get("expect_thread") is None:
            errors.append(f"{eid}: needs expect_any and/or expect_thread")
        for mid in e.get("expect_any") or []:
            if not conn.execute(
                    "SELECT 1 FROM items WHERE message_id=?", (mid,)).fetchone():
                errors.append(f"{eid}: unknown message_id {mid!r}")
        if e.get("expect_thread") is not None:
            if not conn.execute(
                    "SELECT 1 FROM threads WHERE id=?", (e["expect_thread"],)).fetchone():
                errors.append(f"{eid}: unknown thread_id {e['expect_thread']!r}")
    if errors:
        raise SystemExit("eval: golden set validation failed:\n" +
                         "\n".join(f"  - {x}" for x in errors))


# ---- scoring (pure — no I/O, unit-testable) ---------------------------

def score_question(entry, query_json):
    """rank = 1-based position of the first result satisfying expect_any
    or expect_thread; None if never satisfied."""
    results = query_json.get("results", [])
    expect_any = set(entry.get("expect_any") or [])
    expect_thread = entry.get("expect_thread")
    rank = None
    for i, r in enumerate(results, start=1):
        if r.get("message_id") in expect_any or (
                expect_thread is not None and r.get("thread_id") == expect_thread):
            rank = i
            break
    rr = 1.0 / rank if rank else 0.0
    out = {"id": entry["id"], "rank": rank, "rr": rr,
           "returned_ids": [r.get("message_id") for r in results]}
    for k in TOP_KS:
        out[f"hit@{k}"] = bool(rank is not None and rank <= k)
    return out


def aggregate(scored, golden_by_id):
    def agg_over(qs):
        n = len(qs)
        if n == 0:
            return {"n": 0, "mrr": 0.0, **{f"hit@{k}": 0.0 for k in TOP_KS}}
        out = {"n": n, "mrr": sum(q["rr"] for q in qs) / n}
        for k in TOP_KS:
            out[f"hit@{k}"] = sum(q[f"hit@{k}"] for q in qs) / n
        return out

    result = agg_over(scored)
    flags = sorted({f for e in golden_by_id.values() for f in (e.get("flags") or [])})
    result["by_flag"] = {
        flag: agg_over([q for q in scored
                        if flag in (golden_by_id[q["id"]].get("flags") or [])])
        for flag in flags
    }
    return result


# ---- I/O: query invocation (warm | cold), fingerprinting ---------------

def run_query_cold(question, top_k, include_privileged=False):
    """Subprocess query.py — CLI-faithful cold start every question."""
    cmd = [sys.executable, str(SCRIPT_DIR / "query.py"), question,
           "--json", "--top-k", str(top_k)]
    if include_privileged:
        cmd.append("--include-privileged")
    proc = subprocess.run(cmd, capture_output=True, text=True)
    if proc.returncode != 0:
        raise SystemExit(f"eval: query.py failed for {question!r}:\n{proc.stderr}")
    return json.loads(proc.stdout)


class WarmQuerySession:
    """Thin wrapper around query.WarmResources for eval warm mode
    (docs/specs/warm-eval.md). Same residency idea as query_daemon."""

    def __init__(self):
        import query as querymod
        self._warm = querymod.WarmResources(
            log=lambda m: print(f"eval: {m}", flush=True))

    def search(self, question, top_k, include_privileged=False):
        return self._warm.search(
            question, top_k=top_k, include_privileged=include_privileged)

    def close(self):
        self._warm.close()


def _git(args):
    try:
        out = subprocess.run(["git", *args], cwd=config.PROJECT_ROOT,
                             capture_output=True, text=True, check=True)
        return out.stdout.strip()
    except Exception:
        return None


def build_fingerprint(conn, golden_path, golden_list, top_k, query_mode):
    index_meta = json.loads(config.VECTORS_META_JSON.read_text()) \
        if config.VECTORS_META_JSON.exists() else {}
    corpus = {
        "emails": conn.execute("SELECT COUNT(*) FROM items").fetchone()[0],
        "chunks": conn.execute("SELECT COUNT(*) FROM chunks").fetchone()[0],
        "embedded": conn.execute(
            "SELECT COUNT(*) FROM chunks WHERE embedded_at IS NOT NULL").fetchone()[0],
    }
    golden_bytes = Path(golden_path).read_bytes()
    return {
        "git_commit": _git(["rev-parse", "HEAD"]),
        "git_dirty": bool(_git(["status", "--porcelain"])),
        "index": index_meta,
        "corpus": corpus,
        "retrieval_config": {
            "FTS_CANDIDATES": config.FTS_CANDIDATES,
            "VEC_CANDIDATES": config.VEC_CANDIDATES,
            "RRF_K": config.RRF_K,
            "DEFAULT_TOP_K": config.DEFAULT_TOP_K,
            "run_top_k": top_k,
            "RERANK_ENABLED": config.RERANK_ENABLED,
            "RERANK_BACKEND": config.RERANK_BACKEND,
            "RERANK_MODEL": config.RERANK_MODEL_FILE
                if config.RERANK_BACKEND == "llama_cpp"
                else config.MLX_JINA_RERANK_MODEL_REPO,
            "query_mode": query_mode,
        },
        "golden_path": str(golden_path),
        "golden_sha256": hashlib.sha256(golden_bytes).hexdigest(),
        "golden_count": len(golden_list),
    }


# ---- compare (pure core + I/O wrapper) ---------------------------------

def compute_comparison(a, b):
    """Returns (report_str, exit_code). exit_code is 0 unless an
    aggregate regressed AND the two runs used the same golden set."""
    lines = []
    comparable = a["fingerprint"]["golden_sha256"] == b["fingerprint"]["golden_sha256"]
    if not comparable:
        lines.append("WARNING: golden sets differ (sha256 mismatch) — "
                     "results are NOT directly comparable; not gating on regression.")
    if a["fingerprint"]["corpus"] != b["fingerprint"]["corpus"]:
        lines.append(f"NOTE: corpus counts differ: A={a['fingerprint']['corpus']} "
                     f"B={b['fingerprint']['corpus']}")

    noise = 1.0 / max(a["fingerprint"].get("golden_count", 1), 1)
    regressed = False
    lines.append("Aggregates (A -> B):")
    for metric in (*[f"hit@{k}" for k in TOP_KS], "mrr"):
        av, bv = a["aggregates"][metric], b["aggregates"][metric]
        delta = bv - av
        if delta < 0 and comparable:
            regressed = True
        marker = "REGRESSED" if delta < 0 else ("improved" if delta > 0 else "unchanged")
        noise_note = " (within noise)" if abs(delta) < noise else ""
        lines.append(f"  {metric}: {av:.3f} -> {bv:.3f}  ({delta:+.3f}) {marker}{noise_note}")

    qa = {q["id"]: q for q in a["questions"]}
    qb = {q["id"]: q for q in b["questions"]}
    rows = []
    for qid in sorted(set(qa) & set(qb)):
        ra, rb = qa[qid]["rank"], qb[qid]["rank"]
        if ra is not None and rb is not None:
            delta = rb - ra
        elif ra is not None and rb is None:
            delta = 100_000            # found -> lost: worst possible
        elif ra is None and rb is not None:
            delta = -100_000           # lost -> found: best possible
        else:
            delta = 0
        rows.append((delta, qid, ra, rb))
    rows.sort(key=lambda r: -r[0])

    lines.append("Per-question rank changes (worst regressions first):")
    for delta, qid, ra, rb in rows:
        tag = "REGRESSED" if delta > 0 else ("improved" if delta < 0 else "")
        lines.append(f"  {qid}: {ra if ra is not None else 'miss'} -> "
                     f"{rb if rb is not None else 'miss'}  {tag}")

    return "\n".join(lines), (1 if (regressed and comparable) else 0)


# ---- CLI ---------------------------------------------------------------

def cmd_run(args):
    if args.mode not in VALID_MODES:
        raise SystemExit(f"eval: --mode must be one of {VALID_MODES}, "
                         f"got {args.mode!r}")
    golden_path = Path(args.golden)
    if not golden_path.exists():
        raise SystemExit(f"eval: golden set not found: {golden_path}")
    golden_list = load_golden(golden_path)

    conn = db.connect()
    db.migrate(conn)
    validate_golden(conn, golden_list)
    golden_by_id = {e["id"]: e for e in golden_list}

    warm = None
    if args.mode == "warm":
        warm = WarmQuerySession()

    started = datetime.now(timezone.utc)
    t0 = time.time()
    scored = []
    try:
        for i, entry in enumerate(golden_list, 1):
            q0 = time.time()
            priv = entry.get("include_privileged", False)
            if warm is not None:
                data = warm.search(entry["question"], args.top_k, priv)
            else:
                data = run_query_cold(entry["question"], args.top_k, priv)
            s = score_question(entry, data)
            s["seconds"] = round(time.time() - q0, 3)
            scored.append(s)
            print(f"  [{i}/{len(golden_list)}] {entry['id']}  "
                  f"{s['seconds']:.1f}s  rank={s['rank']}", flush=True)
    finally:
        if warm is not None:
            warm.close()
    duration = time.time() - t0

    aggregates = aggregate(scored, golden_by_id)
    fingerprint = build_fingerprint(conn, golden_path, golden_list,
                                    args.top_k, args.mode)
    conn.close()

    result = {"label": args.label, "started_utc": started.isoformat(),
              "duration_s": round(duration, 2), "fingerprint": fingerprint,
              "aggregates": aggregates, "questions": scored}

    config.EVAL_RESULTS_DIR.mkdir(parents=True, exist_ok=True)
    ts = started.strftime("%Y%m%dT%H%M%SZ")
    slug = re.sub(r"[^a-zA-Z0-9_-]+", "-", args.label) or "run"
    out_path = config.EVAL_RESULTS_DIR / f"{ts}__{slug}.json"
    out_path.write_text(json.dumps(result, indent=2, ensure_ascii=False))

    print(f"eval: {len(golden_list)} questions ({args.mode}) -> {out_path}")
    print(f"  duration_s={duration:.1f}  "
          f"hit@1={aggregates['hit@1']:.2f} hit@5={aggregates['hit@5']:.2f} "
          f"hit@15={aggregates['hit@15']:.2f} mrr={aggregates['mrr']:.3f}")
    for flag, agg in aggregates["by_flag"].items():
        print(f"  [{flag}] n={agg['n']} hit@5={agg['hit@5']:.2f} mrr={agg['mrr']:.3f}")


def cmd_compare(args):
    a = json.loads(Path(args.result_a).read_text())
    b = json.loads(Path(args.result_b).read_text())
    report, code = compute_comparison(a, b)
    print(report)
    sys.exit(code)


def cmd_list(args):
    files = sorted(config.EVAL_RESULTS_DIR.glob("*.json"))
    if args.golden:
        want = hashlib.sha256(Path(args.golden).read_bytes()).hexdigest()
        files = [f for f in files
                if json.loads(f.read_text())["fingerprint"]["golden_sha256"] == want]
    print(f"{'started_utc':22} {'label':16} {'commit':9} {'n':>3} {'hit@5':>6} {'mrr':>6}")
    for f in files:
        d = json.loads(f.read_text())
        fp = d["fingerprint"]
        commit = (fp.get("git_commit") or "?")[:8]
        print(f"{d['started_utc']:22} {d['label']:16} {commit:9} "
              f"{fp['golden_count']:>3} {d['aggregates']['hit@5']:>6.2f} "
              f"{d['aggregates']['mrr']:>6.3f}")


def main():
    ap = argparse.ArgumentParser(description=__doc__,
                                 formatter_class=argparse.RawDescriptionHelpFormatter)
    sub = ap.add_subparsers(dest="cmd", required=True)

    p_run = sub.add_parser("run", help="run the golden set through query and score it")
    p_run.add_argument("--golden", required=True, help="path to golden set YAML")
    p_run.add_argument("--label", default="run")
    p_run.add_argument("--top-k", type=int, default=config.DEFAULT_TOP_K)
    p_run.add_argument(
        "--mode", choices=VALID_MODES, default="warm",
        help="warm (default): load embed/rerank once in-process. "
             "cold: subprocess query.py per question (CLI cold-start cost). "
             "See docs/specs/warm-eval.md.")
    p_run.set_defaults(func=cmd_run)

    p_cmp = sub.add_parser("compare", help="compare two result JSON files")
    p_cmp.add_argument("result_a")
    p_cmp.add_argument("result_b")
    p_cmp.set_defaults(func=cmd_compare)

    p_list = sub.add_parser("list", help="list past runs")
    p_list.add_argument("--golden", help="filter to runs of this golden set")
    p_list.set_defaults(func=cmd_list)

    args = ap.parse_args()
    args.func(args)


if __name__ == "__main__":
    sys.exit(main())
