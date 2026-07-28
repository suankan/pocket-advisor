"""Thread-summary generation core, callable from worker threads.

Progress-free and telemetry-free execution path for one thread's summary.
The stage's ``EmailThreadsSummaryDispatcher`` calls ``_generate_thread_summary``
from a worker thread; the worker owns fresh ``_GenerationMetrics`` and
``SummariesTimings`` instances (never the shared telemetry object) and returns
them on the outcome so the main thread can merge and settle.

All helpers are module-level so a worker has a clean call surface with no
reference to ``Progress`` or the database.
"""
import time
from dataclasses import dataclass
from pathlib import Path

from v2.modules.summarization import (SUMMARY_ONE_SHOT_TOKENS,
                                   SUMMARY_REDUCE_FAN_IN,
                                   SUMMARY_SEGMENT_TOKENS, SummaryGenerator,
                                   SummaryMode)


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
        f"THREAD SEGMENT {position} OF {total} — chronological content",
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


def _call_generator(generator: SummaryGenerator, body: str,
                    mode: SummaryMode, metrics: _GenerationMetrics,
                    timings) -> str:
    metrics.calls += 1
    model_started = time.monotonic()
    try:
        result = generator.generate(body, mode)
    finally:
        timings.model_execution += time.monotonic() - model_started
    # Exact prompt tokens come from the service's usage field when the
    # generator exposes them; the deterministic estimate is the fallback.
    metrics.input_tokens += \
        getattr(generator, "last_prompt_tokens", 0) \
        or generator.count_tokens(body)
    if not result.strip():
        raise RuntimeError("thread summarizer returned empty text")
    return result.strip()


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
        block = _render_message(message, text, position, total_messages)
        conservative = _render_segment([block], 9_999, 9_999)
        if count_tokens(conservative) <= SUMMARY_SEGMENT_TOKENS:
            message_blocks.append(block)
            continue

        pieces = _split_text_token_aware(
            text, SUMMARY_SEGMENT_TOKENS,
            lambda piece: count_tokens(_render_segment([
                _render_message(message, piece, position, total_messages,
                                9_999, 9_999)
            ], 9_999, 9_999)))
        for part, piece in enumerate(pieces, 1):
            excerpt = _render_message(
                message, piece, position, total_messages, part, len(pieces))
            if count_tokens(_render_segment(
                    [excerpt], 9_999, 9_999)) > SUMMARY_SEGMENT_TOKENS:
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


def _reduce(thread_id: int, summaries: list[str],
            generator: SummaryGenerator, metrics: _GenerationMetrics,
            timings) -> str:
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
            body = _render_reduction(
                group, round_number, group_number)
            if generator.count_tokens(body) > SUMMARY_ONE_SHOT_TOKENS:
                raise RuntimeError(
                    "summary reduction input exceeds measured quality"
                    " threshold")
            metrics.reductions += 1
            reduced.append(_call_generator(
                generator, body, "reduce", metrics, timings))
        if len(reduced) >= len(current):
            raise RuntimeError("summary reduction made no progress")
        current = reduced
    return current[0]


def _generate_thread_summary(generator: SummaryGenerator, job: _ThreadWork,
                             metrics: _GenerationMetrics,
                             timings) -> tuple[str, str]:
    """Generate one thread's navigation summary (worker-safe).

    Returns ``(summary_text, note)`` where ``note`` is a short human label
    for progress reporting. Never touches ``Progress`` or shared telemetry.
    """
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
        summary = _call_generator(
            generator, complete, "thread", metrics, timings)
        note = f"thread {job.thread_id} · one-shot"
    else:
        metrics.strategy = "hierarchical"
        render_started = time.monotonic()
        segments = _structural_segments(
            job, raw_messages, generator.count_tokens)
        metrics.segments = len(segments)
        timings.input_render += time.monotonic() - render_started
        summaries = [
            _call_generator(
                generator, segment, "segment", metrics, timings)
            for segment in segments
        ]
        summary = _reduce(job.thread_id, summaries, generator,
                          metrics, timings)
        note = f"thread {job.thread_id} · hierarchical"
    if not summary.strip():
        raise RuntimeError("thread summary is empty")
    return summary.strip(), note
