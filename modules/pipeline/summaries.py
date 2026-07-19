"""Stage 4a — deterministic thread summaries via the inference server."""
import hashlib
import time
from dataclasses import dataclass
from pathlib import Path

from modules.domain import StageStats
from modules.embedding.dispatch import EmbedDispatcher
from modules.pipeline.base import Stage
from modules.progress import Progress
from modules.review import now_iso
from modules.summarization import (SUMMARY_PROMPT_VERSION,
                                   SUMMARY_ONE_SHOT_TOKENS,
                                   SUMMARY_REDUCE_FAN_IN,
                                   SUMMARY_SEGMENT_TOKENS,
                                   SummaryMode,
                                   SummaryGenerator,
                                   get_summary_generator)


@dataclass(frozen=True, slots=True)
class _MessageSource:
    message_id: str
    date_utc: str
    path: Path


@dataclass(frozen=True, slots=True)
class _ThreadWork:
    thread_id: int
    stable_key: str
    source_digest: str
    messages: tuple[_MessageSource, ...]


@dataclass(frozen=True, slots=True)
class _BrokenThread:
    """Eligible thread whose readable artifacts are missing; its digest
    cannot be verified, so any existing summary is held stale until the
    emails stage restores the artifact."""
    thread_id: int
    stable_key: str
    missing_path: Path


@dataclass(slots=True)
class _GenerationMetrics:
    """Aggregate telemetry for one thread's generation pass."""
    source_tokens: int = 0
    input_tokens: int = 0
    segments: int = 0
    calls: int = 0
    reductions: int = 0
    strategy: str = ""


def _render_message(message: _MessageSource, text: str, position: int,
                    total: int, part: int | None = None,
                    parts: int | None = None) -> str:
    part_label = "" if part is None else f", excerpt {part}/{parts}"
    return (
        f"[message {position} of {total}{part_label}]\n"
        f"Message-ID: {message.message_id}\n"
        f"Date: {message.date_utc}\n"
        "<email>\n"
        f"{text}\n"
        "</email>"
    )


def _render_thread(blocks: list[str]) -> str:
    return "\n\n".join((
        f"COMPLETE THREAD — {len(blocks)} chronological messages",
        *blocks,
    ))


def _render_segment(blocks: list[str], position: int, total: int) -> str:
    return "\n\n".join((
        f"THREAD SEGMENT {position} OF {total} — chronological evidence",
        *blocks,
    ))


def _render_reduction(summaries: list[str], round_number: int,
                      group_number: int) -> str:
    blocks = []
    for position, summary in enumerate(summaries, 1):
        blocks.append(
            f"[segment summary {position} of {len(summaries)}]\n"
            f"<summary>\n{summary}\n</summary>")
    return "\n\n".join((
        f"SUMMARY REDUCTION ROUND {round_number}, GROUP {group_number}",
        *blocks,
    ))


def _split_text_token_aware(text: str, token_budget: int,
                            count_tokens) -> tuple[str, ...]:
    """Deterministically split one oversized message without dropping text.

    Normal segmentation is message-boundary-only.  This is the explicit
    last-resort fallback for a single message that exceeds the structural
    segment budget.  Binary search finds the largest exact character slice
    within the tokenizer budget, then prefers a nearby whitespace boundary;
    concatenating the returned slices always reconstructs the input exactly.
    """
    if token_budget <= 0:
        raise ValueError("summary token budget must be positive")
    if not text:
        return ("",)
    pieces: list[str] = []
    start = 0
    while start < len(text):
        low, high = start + 1, len(text)
        best = start
        while low <= high:
            middle = (low + high) // 2
            if count_tokens(text[start:middle]) <= token_budget:
                best = middle
                low = middle + 1
            else:
                high = middle - 1
        if best == start:
            raise RuntimeError(
                "summary tokenizer cannot fit one source character in the"
                " oversized-message budget")
        if best < len(text):
            floor = start + max(1, int((best - start) * 0.8))
            boundary = max(
                text.rfind("\n\n", floor, best),
                text.rfind("\n", floor, best),
                text.rfind(" ", floor, best),
            )
            if boundary >= floor:
                # Retain the boundary byte/character at the end of this slice;
                # the next slice begins immediately after it.
                best = boundary + 1
        pieces.append(text[start:best])
        start = best
    if "".join(pieces) != text:
        raise AssertionError("oversized summary split lost source text")
    return tuple(pieces)


class ThreadSummaryStage(Stage):
    name = "summaries"

    def run(self) -> StageStats:
        """Staleness maintenance always runs — even with generation
        disabled — so retrieval never serves a summary whose sources
        have diverged. Only the generative pass is gated by the knob."""
        stats = StageStats()
        work, broken = self._load_work()
        stats.inc("eligible", len(work) + len(broken))
        current = {
            row["thread_id"]: row
            for row in self.conn.execute(
                "SELECT * FROM thread_summaries").fetchall()
        }
        stale = [job for job in work if not self._is_current(
            current.get(job.thread_id), job)]
        stats.inc("unchanged", len(work) - len(stale))

        keep_ids = {job.thread_id for job in work} | \
            {job.thread_id for job in broken}
        if keep_ids:
            marks = ",".join("?" for _ in keep_ids)
            self.conn.execute(
                f"DELETE FROM thread_summaries WHERE thread_id NOT IN"
                f" ({marks})", tuple(sorted(keep_ids)))
        else:
            self.conn.execute("DELETE FROM thread_summaries")

        existing_stale = [job.thread_id for job in stale
                          if job.thread_id in current] + \
                         [job.thread_id for job in broken
                          if job.thread_id in current]
        if existing_stale:
            marks = ",".join("?" for _ in existing_stale)
            self.conn.execute(
                f"UPDATE thread_summaries SET is_stale = 1"
                f" WHERE thread_id IN ({marks})", existing_stale)
        for job in broken:
            self.review.flag(
                f"thread:{job.stable_key}", self.name, "error",
                f"readable email missing: {job.missing_path};"
                " run './pocket-advisor.py --workspace <id> ingest emails'")
            stats.inc("missing_artifacts")
        self.conn.commit()

        if not self.config.summarize_threads:
            # Deliberate gate: telemetry records not_applicable, not a
            # measured zero.
            self.ctx.telemetry.mark_not_applicable(self.name)
            stats.inc("generation_disabled")
            return stats

        perf = self.ctx.telemetry.summaries
        perf.eligible_threads = len(work) + len(broken)
        perf.pending_threads = len(stale)
        perf.unchanged_threads = len(work) - len(stale)
        if not stale:
            return stats
        perf.new_tiers()

        print(f"summaries: {len(stale)} stale"
              f" {'thread' if len(stale) == 1 else 'threads'} — using"
              f" {self.config.model_thread_summary} via"
              f" {self.config.inference_endpoint}")
        generator = get_summary_generator(self.config)
        # Each finished summary's vector is dispatched at readiness
        # (design decision 5); embed_text=false disables all embedding.
        dispatcher = EmbedDispatcher(self.ctx) \
            if self.config.embed_text else None
        progress = Progress(
            "generate thread summaries",
            total=len(stale))
        for job in stale:
            metrics = _GenerationMetrics()
            progress.start(note=f"thread {job.thread_id} · planning")
            try:
                summary = self._generate(job, generator, progress, metrics)
                publish_started = time.monotonic()
                self.conn.execute(
                    """INSERT INTO thread_summaries
                       (thread_id, summary_text, source_digest,
                        generator_model, prompt_version, is_stale,
                        generated_at)
                       VALUES (?, ?, ?, ?, ?, 0, ?)
                       ON CONFLICT(thread_id) DO UPDATE SET
                         summary_text=excluded.summary_text,
                         source_digest=excluded.source_digest,
                         generator_model=excluded.generator_model,
                         prompt_version=excluded.prompt_version,
                         is_stale=0,
                         generated_at=excluded.generated_at""",
                    (job.thread_id, summary, job.source_digest,
                     self.config.model_thread_summary,
                     SUMMARY_PROMPT_VERSION, now_iso()))
                self.conn.commit()
                perf.timings_seconds.publication += \
                    time.monotonic() - publish_started
                if dispatcher is not None:
                    dispatcher.submit_summary(
                        job.thread_id, summary, at_readiness=True)
                stats.inc("generated")
                perf.completed_threads += 1
            except Exception as exc:
                self.conn.rollback()
                progress.println(
                    f"  summary FAIL thread {job.stable_key}:"
                    f" {type(exc).__name__}: {exc}")
                self.review.flag(
                    f"thread:{job.stable_key}", self.name, "error",
                    f"{type(exc).__name__}: {exc}")
                self.conn.commit()
                stats.inc("failed")
                perf.failed_threads += 1
            if metrics.strategy == "thread":
                perf.one_shot_threads += 1
                stats.inc("one_shot")
            else:
                # A tokenizer/planning failure before strategy selection is
                # conservatively classified in the failure-capable
                # hierarchical path so measured cardinalities stay honest.
                perf.hierarchical_threads += 1
                stats.inc("hierarchical")
            perf.input_messages += len(job.messages)
            perf.input_segments += metrics.segments
            perf.generation_calls += metrics.calls
            perf.total_input_tokens += metrics.input_tokens
            perf.overflow_reductions += metrics.reductions
            tier = perf.tier_for(metrics.source_tokens)
            tier.threads += 1
            tier.generation_calls += metrics.calls
            progress.step(note=f"thread {job.thread_id} complete")
        progress.done()
        if dispatcher is not None:
            dispatcher.drain_into_stats(stats)
            dispatcher.close()
        return stats

    def _load_work(self) -> tuple[list[_ThreadWork], list[_BrokenThread]]:
        root = self.config.project_root
        threads = self.conn.execute(
            """SELECT threads.id, threads.stable_key
                 FROM threads
                WHERE (SELECT COUNT(*) FROM emails
                       WHERE emails.thread_id = threads.id) >= 2
                ORDER BY threads.stable_key""").fetchall()
        work: list[_ThreadWork] = []
        broken: list[_BrokenThread] = []
        for thread in threads:
            rows = self.conn.execute(
                """SELECT message_id, COALESCE(date_utc, '') AS date_utc,
                          body_text_path
                     FROM emails
                    WHERE thread_id = ?
                    ORDER BY date_utc, message_id""",
                (thread["id"],)).fetchall()
            digest = hashlib.sha256()
            digest.update(thread["stable_key"].encode("utf-8"))
            messages = []
            missing: Path | None = None
            for row in rows:
                message_path = root / row["body_text_path"]
                if not message_path.is_file():
                    missing = message_path
                    break
                body = message_path.read_bytes()
                for value in (row["message_id"], row["date_utc"]):
                    digest.update(value.encode("utf-8"))
                    digest.update(b"\0")
                digest.update(body)
                digest.update(b"\0")
                messages.append(_MessageSource(
                    row["message_id"], row["date_utc"], message_path))
            if missing is not None:
                broken.append(_BrokenThread(
                    int(thread["id"]), thread["stable_key"], missing))
                continue
            work.append(_ThreadWork(
                int(thread["id"]), thread["stable_key"],
                digest.hexdigest(), tuple(messages)))
        return work, broken

    def _is_current(self, row, job: _ThreadWork) -> bool:
        return bool(row) and not row["is_stale"] \
            and row["source_digest"] == job.source_digest \
            and row["generator_model"] == self.config.model_thread_summary \
            and row["prompt_version"] == SUMMARY_PROMPT_VERSION

    def _generate(self, job: _ThreadWork, generator: SummaryGenerator,
                  progress: Progress,
                  metrics: _GenerationMetrics) -> str:
        timings = self.ctx.telemetry.summaries.timings_seconds
        render_started = time.monotonic()
        raw_messages = [
            message.path.read_text(encoding="utf-8")
            for message in job.messages
        ]
        blocks = [
            _render_message(message, text, position, len(job.messages))
            for position, (message, text) in enumerate(
                zip(job.messages, raw_messages, strict=True), 1)
        ]
        complete = _render_thread(blocks)
        metrics.source_tokens = generator.count_tokens(complete)
        timings.input_render += time.monotonic() - render_started

        if metrics.source_tokens <= SUMMARY_ONE_SHOT_TOKENS:
            metrics.strategy = "thread"
            metrics.segments = 1
            summary = self._call_generator(
                generator, complete, "thread", progress, metrics,
                f"thread {job.thread_id} · one-shot", timings)
        else:
            metrics.strategy = "hierarchical"
            render_started = time.monotonic()
            segments = self._structural_segments(
                job, raw_messages, generator.count_tokens)
            metrics.segments = len(segments)
            timings.input_render += time.monotonic() - render_started
            summaries = []
            for position, segment in enumerate(segments, 1):
                summaries.append(self._call_generator(
                    generator, segment, "segment", progress, metrics,
                    f"thread {job.thread_id} · segment {position}/"
                    f"{len(segments)}", timings))
            summary = self._reduce(
                job.thread_id, summaries, generator, progress, metrics,
                timings)
        if not summary.strip():
            raise RuntimeError("thread summary is empty")
        return summary.strip()

    @staticmethod
    def _call_generator(generator: SummaryGenerator, evidence: str,
                        mode: SummaryMode, progress: Progress,
                        metrics: _GenerationMetrics, note: str,
                        timings) -> str:
        progress.start(note=note)
        metrics.calls += 1
        model_started = time.monotonic()
        try:
            result = generator.generate(evidence, mode)
        finally:
            timings.model_execution += time.monotonic() - model_started
        # Exact prompt tokens come from the service's usage field when the
        # generator exposes them; the deterministic estimate is the fallback.
        metrics.input_tokens += \
            getattr(generator, "last_prompt_tokens", 0) \
            or generator.count_tokens(evidence)
        if not result.strip():
            raise RuntimeError("thread summarizer returned empty text")
        return result.strip()

    @staticmethod
    def _structural_segments(job: _ThreadWork, raw_messages: list[str],
                             count_tokens) -> list[str]:
        """Pack complete messages into deterministic token-bounded segments.

        Only a single message that cannot fit uses the explicit token-aware
        character-slice fallback.  No source character is discarded.
        """
        message_blocks: list[str] = []
        total_messages = len(job.messages)
        for position, (message, text) in enumerate(
                zip(job.messages, raw_messages, strict=True), 1):
            block = _render_message(
                message, text, position, total_messages)
            conservative = _render_segment([block], 9_999, 9_999)
            if count_tokens(conservative) <= SUMMARY_SEGMENT_TOKENS:
                message_blocks.append(block)
                continue

            pieces = _split_text_token_aware(
                text, SUMMARY_SEGMENT_TOKENS,
                lambda piece: count_tokens(_render_segment([
                    _render_message(
                        message, piece, position, total_messages,
                        9_999, 9_999)
                ], 9_999, 9_999)))
            for part, piece in enumerate(pieces, 1):
                excerpt = _render_message(
                    message, piece, position, total_messages,
                    part, len(pieces))
                if count_tokens(_render_segment(
                        [excerpt], 9_999, 9_999)) > \
                        SUMMARY_SEGMENT_TOKENS:
                    raise RuntimeError(
                        "oversized-message fallback exceeded summary"
                        " segment budget")
                message_blocks.append(excerpt)

        groups: list[list[str]] = []
        current: list[str] = []
        for block in message_blocks:
            candidate = [*current, block]
            rendered = _render_segment(candidate, 9_999, 9_999)
            if current and count_tokens(rendered) > SUMMARY_SEGMENT_TOKENS:
                groups.append(current)
                current = [block]
            else:
                current = candidate
        if current:
            groups.append(current)
        segments = [
            _render_segment(group, position, len(groups))
            for position, group in enumerate(groups, 1)
        ]
        if len(segments) < 2:
            raise RuntimeError(
                "hierarchical summary planning did not produce multiple"
                " structural segments")
        return segments

    def _reduce(self, thread_id: int, summaries: list[str],
                generator: SummaryGenerator, progress: Progress,
                metrics: _GenerationMetrics, timings) -> str:
        round_number = 0
        current = summaries
        while len(current) > 1:
            round_number += 1
            reduced: list[str] = []
            groups = [
                current[start:start + SUMMARY_REDUCE_FAN_IN]
                for start in range(0, len(current), SUMMARY_REDUCE_FAN_IN)
            ]
            for group_number, group in enumerate(groups, 1):
                if len(group) == 1:
                    reduced.append(group[0])
                    continue
                evidence = _render_reduction(
                    group, round_number, group_number)
                if generator.count_tokens(evidence) > \
                        SUMMARY_ONE_SHOT_TOKENS:
                    raise RuntimeError(
                        "summary reduction input exceeds measured quality"
                        " threshold")
                metrics.reductions += 1
                reduced.append(self._call_generator(
                    generator, evidence, "reduce", progress, metrics,
                    f"thread {thread_id} · reduce round {round_number} ·"
                    f" group {group_number}/{len(groups)}", timings))
            if len(reduced) >= len(current):
                raise RuntimeError("summary reduction made no progress")
            current = reduced
        return current[0]
