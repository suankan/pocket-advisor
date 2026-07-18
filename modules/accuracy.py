"""Native retrieval-expectation accuracy testing.

Workspace-generic: everything is derived from the selected workspace's bound
configuration and database — no per-workspace knowledge lives here.

Layout (workspace-owned, preserved across ``wipe state``):

    <workspace-state>/search-accuracy-tests/
        expectations/*.yaml     question sets (durable anchors only)
        results/<utc>__<label>.json   one measurement record per run

Expectation entries anchor on durable identities only — Message-IDs
(``expect_any``) and thread stable keys (``expect_thread_key``) — never
integer row ids, which do not survive a re-ingest. Verdicts:

    STRONG       expected Message-ID directly matched in a top-k packet
    THREAD(sum)  expect_thread_key packet selected via the summary channel
    THREAD       only the expected message's thread packet was selected
    MISS         no expected anchor in the top-k packets
    INVALID      anchor not present in this workspace's corpus
    SKIPPED      placeholder question (TODO) not yet authored
"""
import hashlib
import json
import statistics
import time
from dataclasses import dataclass
from datetime import datetime, timezone
from pathlib import Path

import yaml

from modules.config import Config
from modules.custody import write_verified
from modules.pipeline.base import PipelineContext
from modules.retrieval import SearchOptions, SearchResources, run_search

RESULT_SCHEMA_VERSION = 1
_ENTRY_KEYS = frozenset({"id", "question", "expect_any",
                         "expect_thread_key", "flags", "notes", "hint"})
_SCORED = ("STRONG", "THREAD(sum)", "THREAD", "MISS")


class ExpectationError(SystemExit):
    pass


@dataclass(frozen=True, slots=True)
class SuitePaths:
    expectations_dir: Path
    results_dir: Path


def suite_paths(config: Config) -> SuitePaths:
    """Resolve the selected workspace's exact, non-symlinked suite paths."""
    base = config.accuracy_tests_dir
    expected = config.state_dir.resolve(strict=False) / "search-accuracy-tests"
    resolved = base.resolve(strict=False)
    if resolved != expected:
        raise ExpectationError(
            f"accuracy: refusing suite path {resolved}; expected {expected}")
    for component in (config.state_dir, base,
                      base / "expectations", base / "results"):
        if component.is_symlink():
            raise ExpectationError(
                f"accuracy: refusing symlinked suite path: {component}")
    return SuitePaths(base / "expectations", base / "results")


# -- expectations ------------------------------------------------------------

def load_expectations(paths: list[Path]) -> list[dict]:
    """Load and validate one or more expectation files (typo-strict)."""
    entries: list[dict] = []
    seen: set[str] = set()
    for path in paths:
        loaded = yaml.safe_load(path.read_text(encoding="utf-8"))
        if not isinstance(loaded, list):
            raise ExpectationError(
                f"accuracy: {path} must be a YAML list of expectations")
        for entry in loaded:
            if not isinstance(entry, dict):
                raise ExpectationError(
                    f"accuracy: {path} contains a non-mapping entry")
            unknown = set(entry) - _ENTRY_KEYS
            if unknown:
                raise ExpectationError(
                    f"accuracy: {path} entry {entry.get('id')!r} has "
                    f"unknown keys {sorted(unknown)}")
            entry_id = entry.get("id")
            if not entry_id or entry_id in seen:
                raise ExpectationError(
                    f"accuracy: {path} entry id {entry_id!r} is missing "
                    "or duplicated")
            seen.add(entry_id)
            if not entry.get("expect_any") \
                    and not entry.get("expect_thread_key"):
                raise ExpectationError(
                    f"accuracy: entry {entry_id!r} needs expect_any or "
                    "expect_thread_key")
            entries.append(entry)
    if not entries:
        raise ExpectationError("accuracy: expectation set is empty")
    return entries


def expectation_files(paths: SuitePaths,
                      override: Path | None) -> list[Path]:
    if override is not None:
        if not override.is_file():
            raise ExpectationError(
                f"accuracy: no such expectation file: {override}")
        return [override]
    files = sorted(paths.expectations_dir.glob("*.yaml"))
    if not files:
        raise ExpectationError(
            f"accuracy: no expectation sets under {paths.expectations_dir};"
            " author one or scaffold with 'accuracy generate'")
    return files


def _is_todo(entry: dict) -> bool:
    question = (entry.get("question") or "").strip()
    return not question or question.upper().startswith("TODO")


# -- generate ----------------------------------------------------------------

def generate_scaffold(ctx: PipelineContext, target: Path,
                      force: bool) -> tuple[Path, int]:
    """Write an anchor-verified scaffold for the selected workspace.

    Anchors come from the live DB (multi-email threads with current
    summaries; standalone documents); questions are TODO placeholders the
    human replaces — auto-phrased questions would only measure envelope
    echo, not retrieval quality.
    """
    if target.exists() and not force:
        raise ExpectationError(
            f"accuracy: {target} already exists (pass --force to replace)")
    lines = [
        "# Retrieval expectation scaffold — generated "
        f"{datetime.now(timezone.utc).date()} from workspace "
        f"'{ctx.workspace.id}'.",
        "# Replace each TODO question with a natural question whose answer",
        "# lives in the hinted thread/document, then delete this header.",
        "# Anchors are durable (Message-IDs / thread stable keys) and were",
        "# verified against the live database at generation time.",
        "",
    ]
    count = 0
    threads = ctx.conn.execute(
        """SELECT t.id, t.stable_key, t.representative_subject,
                  t.first_date, t.last_date, t.item_count
             FROM threads t JOIN thread_summaries s ON s.thread_id = t.id
            WHERE s.is_stale = 0 ORDER BY t.item_count DESC""").fetchall()
    for row in threads:
        slug = hashlib.sha256(
            row["stable_key"].encode("utf-8")).hexdigest()[:8]
        subject = " ".join((row["representative_subject"] or "").split())
        lines.extend((
            f"- id: thread-{slug}",
            "  question: \"TODO — thread-level question answered by this"
            " conversation\"",
            f"  expect_thread_key: \"{row['stable_key']}\"",
            "  flags: [thread-level]",
            f"  hint: \"{row['item_count']} items, "
            f"{row['first_date']} .. {row['last_date']}, subject: "
            f"{subject[:80]}\"",
            "",
        ))
        count += 1
    documents = ctx.conn.execute(
        """SELECT message_id, subject FROM items
            WHERE item_kind = 'file' ORDER BY subject""").fetchall()
    for row in documents:
        slug = hashlib.sha256(
            row["message_id"].encode("utf-8")).hexdigest()[:8]
        subject = " ".join((row["subject"] or "").split())
        lines.extend((
            f"- id: document-{slug}",
            "  question: \"TODO — question answered by this document\"",
            "  expect_any:",
            f"    - \"{row['message_id']}\"",
            "  flags: [document]",
            f"  hint: \"{subject[:100]}\"",
            "",
        ))
        count += 1
    if not count:
        raise ExpectationError(
            "accuracy: nothing to scaffold — no summarized threads or "
            "documents in this workspace")
    target.parent.mkdir(parents=True, exist_ok=True)
    target.write_text("\n".join(lines), encoding="utf-8")
    return target, count


# -- run ---------------------------------------------------------------------

def _validate_anchors(conn, entry: dict) -> bool:
    if entry.get("expect_thread_key"):
        row = conn.execute("SELECT 1 FROM threads WHERE stable_key = ?",
                           (entry["expect_thread_key"],)).fetchone()
        return row is not None
    marks = ",".join("?" for _ in entry["expect_any"])
    row = conn.execute(
        f"SELECT 1 FROM items WHERE message_id IN ({marks}) LIMIT 1",
        tuple(entry["expect_any"])).fetchone()
    return row is not None


def _score(entry: dict, packets: list[dict]) -> tuple[str, int | None, str | None]:
    if entry.get("expect_thread_key"):
        for rank, packet in enumerate(packets, 1):
            if packet["thread_key"] == entry["expect_thread_key"]:
                verdict = "THREAD(sum)" if packet["summary_hit"] else "THREAD"
                return verdict, rank, packet["thread_key"]
        return "MISS", None, None
    wanted = set(entry["expect_any"])
    fallback: tuple[str, int | None, str | None] = ("MISS", None, None)
    for rank, packet in enumerate(packets, 1):
        matched = {m["message_id"] for m in packet["matches"]} & wanted
        if matched:
            return "STRONG", rank, sorted(matched)[0]
        members = {m["message_id"] for m in packet["messages"]} & wanted
        if members and fallback[0] == "MISS":
            fallback = ("THREAD", rank, sorted(members)[0])
    return fallback


def run_expectations(ctx: PipelineContext, entries: list[dict],
                     source_files: list[Path], *, top_k: int,
                     label: str) -> dict:
    started_at = datetime.now(timezone.utc).isoformat()
    resources = SearchResources.load(ctx)
    fingerprint = resources.fingerprint

    questions = []
    elapsed_all: list[float] = []
    for entry in entries:
        if _is_todo(entry):
            questions.append({"id": entry["id"], "verdict": "SKIPPED",
                              "rank": None, "matched": None,
                              "flags": entry.get("flags", []),
                              "elapsed_seconds": None})
            continue
        if not _validate_anchors(ctx.conn, entry):
            questions.append({"id": entry["id"], "verdict": "INVALID",
                              "rank": None, "matched": None,
                              "flags": entry.get("flags", []),
                              "elapsed_seconds": None})
            continue
        t0 = time.monotonic()
        result = run_search(ctx, entry["question"],
                            SearchOptions(top_k=top_k), resources=resources)
        elapsed = round(time.monotonic() - t0, 3)
        elapsed_all.append(elapsed)
        verdict, rank, matched = _score(entry, result["results"])
        questions.append({"id": entry["id"], "verdict": verdict,
                          "rank": rank, "matched": matched,
                          "flags": entry.get("flags", []),
                          "elapsed_seconds": elapsed})

    counts = {name: 0 for name in
              ("strong", "thread_only", "miss", "invalid", "skipped")}
    for question in questions:
        verdict = question["verdict"]
        if verdict in ("STRONG", "THREAD(sum)"):
            counts["strong"] += 1
        elif verdict == "THREAD":
            counts["thread_only"] += 1
        elif verdict == "MISS":
            counts["miss"] += 1
        elif verdict == "INVALID":
            counts["invalid"] += 1
        else:
            counts["skipped"] += 1
    scored = counts["strong"] + counts["thread_only"] + counts["miss"]
    corpus = {
        "items": ctx.conn.execute(
            "SELECT count(*) FROM items").fetchone()[0],
        "chunks": ctx.conn.execute(
            "SELECT count(*) FROM chunks").fetchone()[0],
        "summaries_current": ctx.conn.execute(
            "SELECT count(*) FROM thread_summaries WHERE is_stale=0"
        ).fetchone()[0],
    }
    digest = hashlib.sha256()
    for path in source_files:
        digest.update(path.read_bytes())
    return {
        "schema_version": RESULT_SCHEMA_VERSION,
        "workspace_id": ctx.workspace.id,
        "label": label,
        "started_at": started_at,
        "ended_at": datetime.now(timezone.utc).isoformat(),
        "expectations": {
            "files": [path.name for path in source_files],
            "sha256": digest.hexdigest(),
            "count": len(entries),
        },
        "environment": {
            "embed": fingerprint,
            "rerank_enabled": bool(ctx.config.rerank_enabled),
            "rerank_model": ctx.config.mlx_model_rerank
            if ctx.config.rerank_enabled else None,
            "top_k": top_k,
            "corpus": corpus,
        },
        "questions": questions,
        "aggregates": {
            **counts,
            "total": len(questions),
            "scored": scored,
            "strong_rate": round(counts["strong"] / scored, 4)
            if scored else None,
            "thread_or_better_rate": round(
                (counts["strong"] + counts["thread_only"]) / scored, 4)
            if scored else None,
            "mean_elapsed_seconds": round(
                statistics.mean(elapsed_all), 3) if elapsed_all else None,
            "median_elapsed_seconds": round(
                statistics.median(elapsed_all), 3) if elapsed_all else None,
            "total_elapsed_seconds": round(sum(elapsed_all), 3),
        },
    }


def persist_result(result: dict, paths: SuitePaths) -> Path:
    stamp = datetime.fromisoformat(result["ended_at"]).astimezone(
        timezone.utc).strftime("%Y%m%dT%H%M%S%fZ")
    label = "".join(ch if ch.isalnum() or ch in "-_" else "-"
                    for ch in result["label"]) or "run"
    target = paths.results_dir / f"{stamp}__{label}.json"
    target.parent.mkdir(parents=True, exist_ok=True)
    payload = (json.dumps(result, indent=2, sort_keys=True,
                          ensure_ascii=False) + "\n").encode("utf-8")
    write_verified(target, payload)
    return target


def load_result(path: Path) -> dict:
    try:
        data = json.loads(path.read_text(encoding="utf-8"))
        if data.get("schema_version") != RESULT_SCHEMA_VERSION:
            raise ValueError(
                f"unsupported schema_version {data.get('schema_version')!r}")
        return data
    except (json.JSONDecodeError, ValueError, KeyError) as exc:
        raise ExpectationError(
            f"accuracy: {path} is not a readable result record: "
            f"{exc}") from exc


def result_files(paths: SuitePaths) -> list[Path]:
    if not paths.results_dir.is_dir():
        return []
    return sorted(paths.results_dir.glob("*.json"))


# -- rendering ---------------------------------------------------------------

def format_run(result: dict, record_path: Path | None) -> str:
    lines = [""]
    for question in result["questions"]:
        rank = f"rank {question['rank']}" if question["rank"] else "-"
        flags = ",".join(question["flags"]) or "-"
        timing = f"{question['elapsed_seconds']:.1f}s" \
            if question["elapsed_seconds"] is not None else "-"
        lines.append(f"  {question['id']:20} {question['verdict']:12} "
                     f"{rank:9} [{flags}] {timing}")
    aggregates = result["aggregates"]
    summary = (f"accuracy: {aggregates['scored']} scored — "
               f"{aggregates['strong']} strong, "
               f"{aggregates['thread_only']} thread-only, "
               f"{aggregates['miss']} miss")
    extras = []
    if aggregates["skipped"]:
        extras.append(f"{aggregates['skipped']} TODO skipped")
    if aggregates["invalid"]:
        extras.append(f"{aggregates['invalid']} INVALID anchors")
    if extras:
        summary += f" ({'; '.join(extras)})"
    if aggregates["thread_or_better_rate"] is not None:
        summary += (f" — {100 * aggregates['thread_or_better_rate']:.0f}% "
                    "thread-or-better")
    lines.extend(("", summary))
    if record_path is not None:
        lines.append(f"Result record: {record_path}")
    return "\n".join(lines)


def format_compare(results: list[dict], names: list[str]) -> str:
    """Newest-last table of aggregates plus per-question changes."""
    lines = ["", "Runs (oldest first):"]
    for name, result in zip(names, results):
        aggregates = result["aggregates"]
        rate = aggregates["thread_or_better_rate"]
        lines.append(
            f"  {name:32} scored={aggregates['scored']} "
            f"strong={aggregates['strong']} "
            f"thread={aggregates['thread_only']} miss={aggregates['miss']} "
            + (f"thread-or-better={100 * rate:.0f}%" if rate is not None
               else "thread-or-better=n/a")
            + (f" label={result['label']}" if result.get("label") else ""))
    shas = {result["expectations"]["sha256"] for result in results}
    if len(shas) > 1:
        lines.append("  ⚠ expectation sets differ between runs — "
                     "per-question rows compare by id only")

    by_run = [
        {q["id"]: q for q in result["questions"]} for result in results]
    all_ids = sorted({qid for run in by_run for qid in run})
    changed = []
    for qid in all_ids:
        cells = []
        for run in by_run:
            question = run.get(qid)
            cells.append("absent" if question is None else
                         question["verdict"] +
                         (f"@{question['rank']}" if question["rank"]
                          else ""))
        if len(set(cells)) > 1:
            changed.append((qid, cells))
    if changed:
        lines.extend(("", "Changed questions (oldest → newest):"))
        for qid, cells in changed:
            lines.append(f"  {qid:20} " + "  →  ".join(cells))
    else:
        lines.extend(("", "No per-question changes across the compared runs."))
    return "\n".join(lines)


def format_list(paths: SuitePaths) -> str:
    files = result_files(paths)
    if not files:
        return f"accuracy: no results under {paths.results_dir}"
    lines = []
    for path in files:
        result = load_result(path)
        aggregates = result["aggregates"]
        rate = aggregates["thread_or_better_rate"]
        lines.append(
            f"  {path.name:44} scored={aggregates['scored']} "
            f"strong={aggregates['strong']} miss={aggregates['miss']} "
            + (f"thread-or-better={100 * rate:.0f}%" if rate is not None
               else "thread-or-better=n/a"))
    return "\n".join(lines)
