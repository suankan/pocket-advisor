"""Stage 4a — deterministic thread summaries via the inference server."""
import hashlib
from dataclasses import dataclass
from pathlib import Path

from modules.domain import StageStats
from modules.embedding.dispatch import shared_dispatcher
from modules.pipeline.base import Stage
from modules.pipeline.summary_dispatch import (
    EmailThreadsSummaryDispatcher, SummaryOutcome)
from modules.pipeline.summaries_core import _GenerationMetrics, _ThreadWork
from modules.progress import Progress
from modules.review import now_iso
from modules.summarization import (SUMMARY_PROMPT_VERSION,
                                    get_summary_generator)


@dataclass(frozen=True, slots=True)
class _BrokenThread:
    """Eligible thread whose readable artifacts are missing; its digest
    cannot be verified, so any existing summary is held stale until the
    emails stage restores the artifact."""
    thread_id: int
    stable_key: str
    missing_path: Path


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
              f" {'thread' if len(stale) == 1 else 'threads'} —"
              f" {self.config.summarisation_endpoint}")
        generator = get_summary_generator(self.config)
        # Each finished summary's vector is dispatched at readiness
        # (design decision 5) without waiting — the embed stage drains;
        # embed_text=false disables all embedding.
        embed_dispatcher = shared_dispatcher(self.ctx) \
            if self.config.embed_text else None
        progress = Progress(
            "generate thread summaries", total=len(stale))

        dispatcher = EmailThreadsSummaryDispatcher(self.ctx, generator)
        for job in stale:
            dispatcher.submit(job)
        done, failed, skipped, outcomes = dispatcher.drain(progress)
        progress.done()
        dispatcher.close()

        if skipped and dispatcher.unavailable is not None:
            print(f"summaries: {dispatcher.unavailable} — {skipped} threads"
                  " left un-summarized; rerun 'ingest all' after starting"
                  " oMLX")

        for outcome in outcomes:
            self._settle(outcome, embed_dispatcher, stats, perf)

        self.conn.commit()
        stats.inc("generated", done)
        stats.inc("failed", failed)
        stats.inc("skipped", skipped)
        return stats

    def _settle(self, outcome: SummaryOutcome, embed_dispatcher,
                stats: StageStats, perf) -> None:
        """Main-thread settlement for one generated thread: merge telemetry,
        write the DB row, dispatch the summary's embedding, and flag
        failures. DB, Progress, and ReviewLog are touched only here."""
        metrics = outcome.metrics
        perf.timings_seconds.input_render += outcome.timings.input_render
        perf.timings_seconds.model_execution += outcome.timings.model_execution
        perf.input_messages += len(outcome.job.messages)
        perf.input_segments += metrics.segments
        perf.generation_calls += metrics.calls
        perf.total_input_tokens += metrics.input_tokens
        perf.overflow_reductions += metrics.reductions
        tier = perf.tier_for(metrics.source_tokens)
        tier.threads += 1
        tier.generation_calls += metrics.calls

        if outcome.skipped:
            perf.pending_threads = max(0, perf.pending_threads - 1)
            return
        if outcome.error is not None:
            self.conn.rollback()
            self.review.flag(
                f"thread:{outcome.stable_key}", self.name, "error",
                outcome.error)
            self.conn.commit()
            perf.failed_threads += 1
            return

        self.conn.execute(
            """INSERT INTO thread_summaries
               (thread_id, summary_text, source_digest,
                generator_model, prompt_version, is_stale, generated_at)
               VALUES (?, ?, ?, ?, ?, 0, ?)
               ON CONFLICT(thread_id) DO UPDATE SET
                 summary_text=excluded.summary_text,
                 source_digest=excluded.source_digest,
                 generator_model=excluded.generator_model,
                 prompt_version=excluded.prompt_version,
                 is_stale=0,
                 generated_at=excluded.generated_at""",
            (outcome.job.thread_id, outcome.summary_text,
             outcome.job.source_digest, "",
              SUMMARY_PROMPT_VERSION, now_iso()))
        self.conn.commit()
        if embed_dispatcher is not None:
            embed_dispatcher.submit_summary(
                outcome.job.thread_id, outcome.summary_text,
                at_readiness=True)
        perf.completed_threads += 1
        if metrics.strategy == "thread":
            perf.one_shot_threads += 1
            stats.inc("one_shot")
        else:
            perf.hierarchical_threads += 1
            stats.inc("hierarchical")

    def _load_work(self) -> tuple[list[_ThreadWork], list[_BrokenThread]]:
        from modules.pipeline.summaries_core import _MessageSource
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
            messages: list[_MessageSource] = []
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
            and row["prompt_version"] == SUMMARY_PROMPT_VERSION
