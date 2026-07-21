"""Native retrieval-expectation accuracy testing.

Workspace-generic: everything is derived from the selected workspace's bound
configuration and database — no per-workspace knowledge lives here.

Layout (workspace-owned, preserved across ``wipe state``):

    <workspace-state>/search-accuracy-tests/
        expectations/*.yaml     question sets (durable anchors only)
        results/<utc>__<label>.json   one measurement record per run

Expectation entries anchor on durable identities only — Message-IDs
(``expect_any``), thread stable keys (``expect_thread_key``), and document
SHA-256 values — never integer row ids. Verdicts:

    STRONG       expected Message-ID/document SHA directly matched
    THREAD(sum)  expect_thread_key packet selected via the summary channel
    THREAD       only the expected message's thread packet was selected
    MISS         no expected anchor in the top-k packets
    INVALID      anchor not present in this workspace's corpus
    SKIPPED      empty or TODO placeholder (legacy hand scaffolds only)

``accuracy generate`` synthesizes scorable questions from authored email
bodies and PDF text via a local MLX model (never subjects/filenames/summaries).
"""
import hashlib
import json
import statistics
import time
from dataclasses import dataclass
from datetime import datetime, timezone
from pathlib import Path

import yaml

from modules.integrity import write_verified
from modules.emailbody import body_text as message_body_text
from modules.pipeline.base import PipelineContext
from modules.progress import Progress
from modules.question_generation import (QUESTION_MAX_INPUT_TOKENS,
                                         QUESTION_PROMPT_VERSION,
                                         QuestionGenerator, accept_question,
                                         get_question_generator)
from modules.retrieval import SearchOptions, SearchResources, run_search

RESULT_SCHEMA_VERSION = 1
_ENTRY_KEYS = frozenset({"id", "question", "expect_any",
                         "expect_thread_key", "flags", "notes", "hint",
                         "origin"})
GENERATED_EXPECTATIONS_NAME = "generated.yaml"

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
            " run 'accuracy generate' or add a hand-authored *.yaml")
    return files


def _is_todo(entry: dict) -> bool:
    question = (entry.get("question") or "").strip()
    return not question or question.upper().startswith("TODO")


# -- generate ----------------------------------------------------------------

@dataclass(frozen=True, slots=True)
class GenerateStats:
    generated: int
    skipped_empty: int
    rejected: int
    considered: int
    model: str
    prompt_version: int


@dataclass(frozen=True, slots=True)
class _Candidate:
    kind: str  # "thread" | "document"
    entry_id: str
    body: str
    anchors: dict
    flags: list[str]
    hint: str


def _thread_candidates(ctx: PipelineContext) -> tuple[list[_Candidate], int]:
    """Multi-email threads with a loadable authored body as body."""
    root = ctx.config.project_root
    rows = ctx.conn.execute(
        """SELECT id, stable_key, first_date, last_date, item_count
             FROM threads
            WHERE item_count >= 2
            ORDER BY item_count DESC, stable_key ASC""").fetchall()
    candidates: list[_Candidate] = []
    skipped_empty = 0
    for row in rows:
        members = ctx.conn.execute(
            """SELECT message_id, body_text_path
                 FROM emails
                WHERE thread_id = ? AND body_text_path IS NOT NULL
                ORDER BY message_id ASC""",
            (row["id"],)).fetchall()
        best_body = ""
        best_mid: str | None = None
        for member in members:
            path = root / member["body_text_path"]
            if not path.is_file():
                continue
            try:
                body = message_body_text(
                    path.read_bytes(), source=path).strip()
            except (OSError, ValueError, UnicodeDecodeError):
                continue
            if len(body) > len(best_body) or (
                    len(body) == len(best_body) and body
                    and (best_mid is None
                         or (member["message_id"] or "") < best_mid)):
                best_body = body
                best_mid = member["message_id"]
        if not best_body:
            skipped_empty += 1
            continue
        slug = hashlib.sha256(
            row["stable_key"].encode("utf-8")).hexdigest()[:8]
        anchors: dict = {"expect_thread_key": row["stable_key"]}
        if best_mid:
            anchors["expect_any"] = [best_mid]
        candidates.append(_Candidate(
            kind="thread",
            entry_id=f"thread-{slug}",
            body=best_body,
            anchors=anchors,
            flags=["thread-level", "generated"],
            hint=(f"{row['item_count']} emails, "
                  f"{row['first_date']} .. {row['last_date']}"),
        ))
    return candidates, skipped_empty


def _document_candidates(ctx: PipelineContext) -> tuple[list[_Candidate], int]:
    """PDFs with a readable extracted-text product as body."""
    root = ctx.config.project_root
    rows = ctx.conn.execute(
        """SELECT sha256, extracted_text_path
             FROM documents
            WHERE media_kind = 'pdf'
              AND is_skipped = 0
              AND extracted_text_path IS NOT NULL
            ORDER BY sha256 ASC""").fetchall()
    candidates: list[_Candidate] = []
    skipped_empty = 0
    for row in rows:
        path = root / row["extracted_text_path"]
        if not path.is_file():
            skipped_empty += 1
            continue
        try:
            text = path.read_text(encoding="utf-8", errors="replace").strip()
        except OSError:
            skipped_empty += 1
            continue
        if not text:
            skipped_empty += 1
            continue
        slug = row["sha256"][:8]
        candidates.append(_Candidate(
            kind="document",
            entry_id=f"document-{slug}",
            body=text,
            anchors={"expect_any": [row["sha256"]]},
            flags=["document", "generated"],
            hint="pdf",
        ))
    return candidates, skipped_empty


def generate_expectations(
        ctx: PipelineContext, target: Path, force: bool, *,
        limit: int | None = None,
        generator: QuestionGenerator | None = None,
) -> tuple[Path, GenerateStats]:
    """Synthesize a complete expectation set from body/PDF body."""
    if target.exists() and not force:
        raise ExpectationError(
            f"accuracy: {target} already exists (pass --force to replace)")
    if limit is not None and limit <= 0:
        raise ExpectationError("accuracy generate: --limit must be positive")

    threads, thread_empty = _thread_candidates(ctx)
    documents, document_empty = _document_candidates(ctx)
    candidates = [*threads, *documents]
    skipped_empty = thread_empty + document_empty
    if not candidates:
        raise ExpectationError(
            "accuracy: nothing to generate — no multi-email threads with "
            "authored bodies or PDFs with extracted text in this workspace")

    considered = len(candidates)
    if limit is not None:
        candidates = candidates[:limit]

    if generator is None:
        print("accuracy: loading question model…", flush=True)
        generator = get_question_generator(ctx.config)

    entries: list[dict] = []
    rejected = 0
    progress = Progress("generate questions", total=len(candidates))
    try:
        for candidate in candidates:
            progress.start(note=candidate.entry_id)
            body = generator.truncate(
                candidate.body, QUESTION_MAX_INPUT_TOKENS)
            if not body.strip():
                rejected += 1
                progress.step(note=candidate.entry_id)
                continue
            try:
                raw = generator.generate(body)
            except Exception as exc:  # noqa: BLE001 — isolate one failure
                progress.println(
                    f"accuracy: {candidate.entry_id}: generation failed: {exc}")
                rejected += 1
                progress.step(note=candidate.entry_id)
                continue
            question = accept_question(raw)
            if question is None:
                rejected += 1
                progress.step(note=candidate.entry_id)
                continue
            entry = {
                "id": candidate.entry_id,
                "question": question,
                **candidate.anchors,
                "flags": list(candidate.flags),
                "hint": candidate.hint,
                "notes": (f"generated v{QUESTION_PROMPT_VERSION}"),
                "origin": "generated",
            }
            entries.append(entry)
            progress.step(note=candidate.entry_id)
    finally:
        progress.done()

    if not entries:
        raise ExpectationError(
            "accuracy: generation produced no usable questions "
            f"(considered={considered}, skipped_empty={skipped_empty}, "
            f"rejected={rejected})")

    header = (
        f"# Retrieval expectations — generated "
        f"{datetime.now(timezone.utc).date()} from workspace "
        f"'{ctx.workspace.id}'.\n"
        f"# question_prompt_version: {QUESTION_PROMPT_VERSION}\n"
        "# Questions were synthesized from authored email bodies and PDF "
        "text only\n"
        "# (no subjects, filenames, or thread summaries).\n"
        "\n"
    )
    body = yaml.safe_dump(
        entries, allow_unicode=True, default_flow_style=False,
        sort_keys=False)
    target.parent.mkdir(parents=True, exist_ok=True)
    if force and target.exists():
        target.unlink()
    target.write_text(header + body, encoding="utf-8")
    stats = GenerateStats(
        generated=len(entries),
        skipped_empty=skipped_empty,
        rejected=rejected,
        considered=considered,
        model=ctx.config.summarisation_endpoint,
        prompt_version=QUESTION_PROMPT_VERSION,
    )
    return target, stats


# -- run ---------------------------------------------------------------------

def _validate_anchors(conn, entry: dict) -> bool:
    """An expect_any anchor may be an email Message-ID or a document
    sha256 (scaffold() produces both, depending on what it anchored) —
    existence in either table validates it. message_id is explicitly
    non-unique now (collisions retained, not merged), so this is an
    existence check, never a "the one row" lookup; the ORDER BY id LIMIT 1
    is deterministic tie-breaking, not a uniqueness assumption."""
    if entry.get("expect_thread_key"):
        row = conn.execute(
            "SELECT 1 FROM threads WHERE stable_key = ? ORDER BY id LIMIT 1",
            (entry["expect_thread_key"],)).fetchone()
        return row is not None
    marks = ",".join("?" for _ in entry["expect_any"])
    values = tuple(entry["expect_any"])
    email_row = conn.execute(
        f"SELECT 1 FROM emails WHERE message_id IN ({marks})"
        " ORDER BY id LIMIT 1", values).fetchone()
    if email_row is not None:
        return True
    document_row = conn.execute(
        f"SELECT 1 FROM documents WHERE sha256 IN ({marks})"
        " ORDER BY id LIMIT 1", values).fetchone()
    return document_row is not None


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
        if packet.get("kind") == "document":
            # Document packets anchor on sha256, not message_id — parallel
            # to the email/thread match check below.
            if packet.get("sha256") in wanted:
                return "STRONG", rank, packet["sha256"]
            continue
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
    progress = Progress("accuracy run", total=len(entries))
    try:
        for entry in entries:
            is_scored = not _is_todo(entry) and _validate_anchors(ctx.conn, entry)
            if _is_todo(entry):
                questions.append({"id": entry["id"], "verdict": "SKIPPED",
                                  "rank": None, "matched": None,
                                  "flags": entry.get("flags", []),
                                  "elapsed_seconds": None})
                progress.println(_fmt_question_line({
                    "id": entry["id"], "verdict": "SKIPPED", "rank": None,
                    "matched": None, "flags": entry.get("flags", []),
                    "elapsed_seconds": None}))
                progress.step(note=entry["id"])
                continue
            if not _validate_anchors(ctx.conn, entry):
                questions.append({"id": entry["id"], "verdict": "INVALID",
                                  "rank": None, "matched": None,
                                  "flags": entry.get("flags", []),
                                  "elapsed_seconds": None})
                progress.println(_fmt_question_line({
                    "id": entry["id"], "verdict": "INVALID", "rank": None,
                    "matched": None, "flags": entry.get("flags", []),
                    "elapsed_seconds": None}))
                progress.step(note=entry["id"])
                continue
            t0 = time.monotonic()
            result = run_search(ctx, entry["question"],
                                SearchOptions(top_k=top_k), resources=resources)
            elapsed = round(time.monotonic() - t0, 3)
            elapsed_all.append(elapsed)
            verdict, rank, matched = _score(entry, result["results"])
            question = {"id": entry["id"], "verdict": verdict,
                        "rank": rank, "matched": matched,
                        "flags": entry.get("flags", []),
                        "elapsed_seconds": elapsed}
            questions.append(question)
            progress.println(_fmt_question_line(question))
            progress.step(note=entry["id"])
    finally:
        progress.done()

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
        "emails": ctx.conn.execute(
            "SELECT count(*) FROM emails").fetchone()[0],
        "documents": ctx.conn.execute(
            "SELECT count(*) FROM documents").fetchone()[0],
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
            "rerank_model": ctx.config.reranker_endpoint
            if ctx.config.rerank_enabled else None,
            "top_k": top_k,
            "corpus": corpus,
            **({
                "question_generator": {
                    "endpoint": ctx.config.summarisation_endpoint,
                    "prompt_version": QUESTION_PROMPT_VERSION,
                },
            } if any(entry.get("origin") == "generated"
                     for entry in entries) else {}),
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

def _fmt_question_line(question: dict) -> str:
    """One live result line, matching the format_run layout."""
    rank = f"rank {question['rank']}" if question["rank"] else "-"
    flags = ",".join(question.get("flags", [])) or "-"
    timing = f"{question['elapsed_seconds']:.1f}s" \
        if question.get("elapsed_seconds") is not None else "-"
    return (f"  {question['id']:20} {question['verdict']:12} "
            f"{rank:9} [{flags}] {timing}")


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
