"""Stage 1 — Discovery: build the ingestion working set.

Walks every collection mounted on the selected workspace. Originals are
opened read-only (hashing only — classification is extension-based;
Stage 1 never parses content). One walk feeds BOTH tables:

- ingestion_candidates — the working set, keyed (collection_id, sha256).
  email/pdf files enter as status='candidate'; anything else is
  recorded as status='skipped' (tracked, never processed).
- source_blob_index — sha256-to-path cache of every observed source path,
  replaced per collection on every run. The durable blob identity remains
  `(collection_id, sha256)`; duplicate paths deliberately remain visible
  as distinct occurrences.

Idempotency and integrity:
- A sha already recorded for the collection is counted as known.
- A KNOWN relpath whose content hash changed — while the old hash is
  gone from the walk — is an integrity alarm: flagged to the
  review queue and NOT ingested. Content is immutable; a changed file
  needs a human decision, not silent re-ingestion.
- A moved/renamed file (same sha, new relpath) is just known; the blob
  index picks up the new location.
"""
import sqlite3
from collections import defaultdict
from collections.abc import Iterator
from dataclasses import dataclass
from pathlib import Path

from v2.modules.config import IGNORED_FILENAMES
from v2.modules.integrity import sha256_file
from v2.modules.domain import Candidate, CandidateStatus, DocumentType, StageStats
from v2.modules.pipeline.base import Stage
from v2.modules.review import now_iso
from v2.modules.workspace import Collection

# Workspace/agent metadata that may sit inside source trees — never content.
_SKIP_NAMES = IGNORED_FILENAMES | {"WORKSPACE.md", "workspace-config.yaml"}


@dataclass(frozen=True, slots=True)
class FoundFile:
    relpath: str
    sha256: str
    size_bytes: int
    mtime_ns: int


class DiscoverStage(Stage):
    name = "discover"

    def run(self) -> StageStats:
        stats = StageStats()
        workspace = self.ctx.workspace
        collections = workspace.collections

        jobs: list[tuple[Collection, Path]] = []
        walkable: list[Collection] = []
        for coll in collections:
            if not coll.root.is_dir():
                self.review.flag(
                    coll.root, self.name, "warning",
                    f"collection {coll.id} root missing: {coll.root}")
                stats.inc("missing_roots")
                continue
            walkable.append(coll)
            jobs.extend((coll, path) for path in _iter_files(coll.root))

        found: dict[str, list[FoundFile]] = defaultdict(list)
        progress = self.log.progress(self.name, total=len(jobs))
        for coll, path in jobs:
            progress.step(note=path.name)
            relpath = str(path.relative_to(coll.root)).replace("\\", "/")
            try:
                st = path.stat()
                sha = sha256_file(path)
            except OSError as exc:
                stats.inc("errors")
                self.review.flag(f"{coll.id}/{relpath}", self.name, "error",
                                 f"unreadable: {exc}")
                continue
            found[coll.id].append(FoundFile(
                relpath=relpath, sha256=sha, size_bytes=st.st_size,
                mtime_ns=getattr(st, "st_mtime_ns",
                                 int(st.st_mtime * 1e9))))
            stats.inc("bytes", st.st_size)
        progress.done()

        for coll in walkable:
            self._refresh_blob_index(workspace.id, coll, found[coll.id],
                                     stats)
            self._upsert_candidates(workspace.id, coll, found[coll.id],
                                    stats)
        self.conn.commit()
        return stats

    # -- source_blob_index (sha256-to-path cache) -------------------------

    def _refresh_blob_index(self, workspace_id: str, coll: Collection,
                            files: list[FoundFile],
                            stats: StageStats) -> None:
        """Replace this collection's rows from the walk.

        Multiple paths may carry identical bytes. They are one durable blob
        identity but several source occurrences, so all rows are retained.
        """
        now = now_iso()
        self.conn.execute(
            "DELETE FROM source_blob_index WHERE source_id = ?", (coll.id,))
        for file in files:
            self.conn.execute(
                "INSERT INTO source_blob_index"
                " (workspace_id, source_id, sha256, relpath_within_source,"
                "  size_bytes, mtime_ns, indexed_at)"
                " VALUES (?, ?, ?, ?, ?, ?, ?)",
                (workspace_id, coll.id, file.sha256, file.relpath,
                 file.size_bytes, file.mtime_ns, now))
            stats.inc("blob_rows")

    # -- ingestion_candidates (working set) --------------------------------

    def _upsert_candidates(self, workspace_id: str, coll: Collection,
                           files: list[FoundFile],
                           stats: StageStats) -> None:
        rows = self.conn.execute(
            "SELECT relpath, sha256 FROM ingestion_candidates"
            " WHERE collection_id = ?", (coll.id,)).fetchall()
        known_shas = {row["sha256"] for row in rows}
        sha_by_relpath = {row["relpath"]: row["sha256"] for row in rows}
        walk_shas = {file.sha256 for file in files}

        for file in files:
            if file.sha256 in known_shas:
                stats.inc("known")
                continue
            prior = sha_by_relpath.get(file.relpath)
            if prior is not None and prior not in walk_shas:
                # Same path, different content, old content gone: alarm.
                self.review.flag(
                    f"{coll.id}/{file.relpath}", self.name, "error",
                    f"content CHANGED at known path (was {prior[:12]}…,"
                    f" now {file.sha256[:12]}…) — integrity alarm,"
                    " NOT ingested; resolve in review queue")
                stats.inc("integrity_alarms")
                continue
            document_type = DocumentType.classify(Path(file.relpath))
            status = (CandidateStatus.CANDIDATE
                      if document_type is not DocumentType.OTHER
                      else CandidateStatus.SKIPPED)
            self.conn.execute(
                "INSERT INTO ingestion_candidates"
                " (workspace_id, collection_id, relpath, sha256,"
                "  size_bytes, document_type, status, discovered_at)"
                " VALUES (?, ?, ?, ?, ?, ?, ?, ?)",
                (workspace_id, coll.id, file.relpath, file.sha256,
                 file.size_bytes, document_type, status, now_iso()))
            known_shas.add(file.sha256)
            match document_type:
                case DocumentType.EMAIL:
                    stats.inc("new_emails")
                case DocumentType.PDF:
                    stats.inc("new_pdfs")
                case DocumentType.OTHER:
                    stats.inc("other_skipped")


def _iter_files(root: Path) -> Iterator[Path]:
    for path in sorted(root.rglob("*")):
        if not path.is_file():
            continue
        if path.name in _SKIP_NAMES or path.name.startswith("."):
            continue
        yield path


# -- working-set access for downstream stages ------------------------------


def load_candidates(conn: sqlite3.Connection,
                    document_type: DocumentType,
                    status: CandidateStatus = CandidateStatus.CANDIDATE,
                    ) -> list[Candidate]:
    rows = conn.execute(
        "SELECT * FROM ingestion_candidates"
        " WHERE document_type = ? AND status = ?"
        " ORDER BY collection_id, relpath",
        (document_type, status)).fetchall()
    return [Candidate(
        id=row["id"], workspace_id=row["workspace_id"],
        collection_id=row["collection_id"], relpath=row["relpath"],
        sha256=row["sha256"], size_bytes=row["size_bytes"],
        document_type=DocumentType(row["document_type"]),
        status=CandidateStatus(row["status"]),
        discovered_at=row["discovered_at"]) for row in rows]


def set_candidate_status(conn: sqlite3.Connection, candidate_id: int,
                         status: CandidateStatus) -> None:
    conn.execute("UPDATE ingestion_candidates SET status = ? WHERE id = ?",
                 (status, candidate_id))
