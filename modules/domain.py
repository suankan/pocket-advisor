"""Domain objects shared across pipeline stages. Requires Python 3.14."""
from dataclasses import dataclass, field
from enum import StrEnum
from pathlib import Path


class DocumentType(StrEnum):
    """What discovery decides a corpus file IS (extension-based only —
    Stage 1 never opens files)."""

    EMAIL = "email"
    PDF = "pdf"
    OTHER = "other"

    @classmethod
    def classify(cls, path: Path) -> DocumentType:
        match path.suffix.lower():
            case ".eml":
                return cls.EMAIL
            case ".pdf":
                return cls.PDF
            case _:
                return cls.OTHER


class CandidateStatus(StrEnum):
    """Pipeline state of one ingestion_candidates row."""

    CANDIDATE = "candidate"   # discovered, not yet processed
    INGESTED = "ingested"     # its stage completed
    SKIPPED = "skipped"       # deliberately not processed (e.g. OTHER type)
    ERROR = "error"           # stage failed; retried on next run


@dataclass(frozen=True, slots=True)
class Candidate:
    """One row of ingestion_candidates — the Stage 1 working set.

    Identity is (collection_id, sha256): pathless, custody-consistent.
    relpath is provenance (first-seen path within the collection root),
    not identity.
    """

    id: int
    workspace_id: str
    collection_id: str
    relpath: str
    sha256: str
    size_bytes: int
    document_type: DocumentType
    status: CandidateStatus
    discovered_at: str

    @property
    def filename(self) -> str:
        return Path(self.relpath).name


@dataclass(slots=True)
class StageStats:
    """Per-stage counters, printed as the stage's one-line summary."""

    counts: dict[str, int] = field(default_factory=dict)

    def inc(self, key: str, by: int = 1) -> None:
        self.counts[key] = self.counts.get(key, 0) + by

    def get(self, key: str) -> int:
        return self.counts.get(key, 0)

    def merge(self, other: StageStats) -> None:
        for key, value in other.counts.items():
            self.inc(key, value)

    def __str__(self) -> str:
        if not self.counts:
            return "nothing to do"
        return ", ".join(f"{k}={v}" for k, v in sorted(self.counts.items()))
