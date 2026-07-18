"""Stage 4a — deterministic, local-LLM summaries of complete threads."""
import hashlib
from dataclasses import dataclass
from pathlib import Path

from modules.domain import StageStats
from modules.embedding.loader import ModelStore
from modules.pipeline.base import Stage
from modules.progress import Progress
from modules.review import now_iso
from modules.summarization import (SUMMARY_PROMPT_VERSION,
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


def _segments(text: str, size: int):
    if size <= 0:
        raise ValueError("thread_summary_segment_chars must be positive")
    if not text:
        yield ""
        return
    for start in range(0, len(text), size):
        yield text[start:start + size]


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
            stats.inc("generation_disabled")
            return stats
        if not stale:
            return stats

        print(f"summaries: {len(stale)} stale"
              f" {'thread' if len(stale) == 1 else 'threads'} — loading"
              f" {self.config.mlx_model_thread_summary}")
        generator = get_summary_generator(
            self.config, ModelStore(self.config.models_dir))
        progress = Progress(
            "generate thread summaries",
            total=sum(len(job.messages) for job in stale))
        for job in stale:
            try:
                summary = self._generate(job, generator, progress)
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
                     self.config.mlx_model_thread_summary,
                     SUMMARY_PROMPT_VERSION, now_iso()))
                self.conn.commit()
                stats.inc("generated")
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
        progress.done()
        return stats

    def _load_work(self) -> tuple[list[_ThreadWork], list[_BrokenThread]]:
        root = self.config.project_root
        threads = self.conn.execute(
            """SELECT threads.id, threads.stable_key
                 FROM threads
                WHERE (SELECT COUNT(*) FROM items
                       WHERE items.thread_id = threads.id
                         AND items.item_kind = 'email') >= 2
                ORDER BY threads.stable_key""").fetchall()
        work: list[_ThreadWork] = []
        broken: list[_BrokenThread] = []
        for thread in threads:
            rows = self.conn.execute(
                """SELECT message_id, COALESCE(date_utc, '') AS date_utc,
                          body_text_path
                     FROM items
                    WHERE thread_id = ? AND item_kind = 'email'
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
            and row["generator_model"] == \
            self.config.mlx_model_thread_summary \
            and row["prompt_version"] == SUMMARY_PROMPT_VERSION

    def _generate(self, job: _ThreadWork, generator: SummaryGenerator,
                  progress: Progress) -> str:
        summary = ""
        for position, message in enumerate(job.messages, 1):
            progress.step(note=(
                f"thread {job.thread_id} · msg {position}/"
                f"{len(job.messages)} · {message.message_id}"))
            text = message.path.read_text(encoding="utf-8")
            parts = tuple(_segments(
                text, self.config.thread_summary_segment_chars))
            for index, segment in enumerate(parts, 1):
                label = (f"{message.message_id}, {message.date_utc},"
                         f" segment {index}/{len(parts)}")
                summary = generator.update(summary, segment, label)
        if not summary.strip():
            raise RuntimeError("thread summary is empty")
        return summary.strip()
