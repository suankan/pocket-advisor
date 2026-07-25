"""On-demand thread-summary text: read the filesystem artifact.

Summary text lives at `summaries/<thread_id>/summary.txt`
(`docs/storage/separate-db-and-fs-concerns.md` decision 3); the
`thread_summaries` row keeps only `summary_sha256` so vector-filename
binding, verification, and staleness never require reading the file.
Every consumer that needs the actual text — embedding payloads, rerank
input, result snippets, thread packets — reads through here.
"""
from modules.config import Config


class SummaryArtifactMissing(RuntimeError):
    """A thread's summary file is absent or unreadable — retryable
    derived state; the next summaries stage run regenerates it."""


def read_summary_text(config: Config, thread_id: int) -> str:
    path = config.summary_path(thread_id)
    try:
        return path.read_text(encoding="utf-8")
    except OSError as exc:
        raise SummaryArtifactMissing(
            f"summary artifact unreadable: {path} (thread {thread_id}):"
            f" {exc}; re-run ingest summaries to restore derived"
            " artifacts") from exc
