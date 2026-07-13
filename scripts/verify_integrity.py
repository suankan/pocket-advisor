"""Chain-of-custody verification via content hashes (path-agnostic).

Rebuilds the regenerable source_blob_index, then checks every recorded
(source_id, sha256) is present on disk under that source with a matching
hash. File renames inside a source do not fail. Identity is collection
(source_id) + content — not workspace_id (schema-items-membership Phase A).

Run before anything sensitive. Exit 0 = clean; exit 1 = drift.
"""
import sys

import blob_index
import config
import db
import utils_hash


def run():
    conn = db.connect()
    db.migrate(conn)

    # Refresh sha→path cache from disk (regenerable; not identity).
    blob_index.rebuild_all(conn)

    recorded_emails = conn.execute(
        """SELECT source_id, sha256 FROM email_files
           WHERE source_id IS NOT NULL AND sha256 IS NOT NULL""").fetchall()
    recorded_docs = conn.execute(
        """SELECT source_id, sha256, filename FROM documents
           WHERE source_id IS NOT NULL AND sha256 IS NOT NULL""").fetchall()
    conn.close()

    problems = []
    # Disk hashes by source_id (collection)
    on_disk = {}  # src -> set(sha)
    for s in blob_index.list_sources():
        key = s.source_id
        shas = on_disk.setdefault(key, set())
        if s.root.is_dir():
            for path in s.root.rglob("*"):
                if not path.is_file() or path.name.startswith("."):
                    continue
                if path.name in config.IGNORED_FILENAMES:
                    continue
                try:
                    shas.add(utils_hash.sha256_file(path))
                except OSError:
                    continue

    def check_rows(rows, label):
        for r in rows:
            key = r["source_id"]
            sha = r["sha256"]
            disk = on_disk.get(key, set())
            if sha not in disk:
                tag = r["filename"] if "filename" in r.keys() else sha[:12]
                problems.append(
                    f"MISSING {label}: source={r['source_id']} sha={sha[:12]}… ({tag})")

    check_rows(recorded_emails, "email")
    check_rows(recorded_docs, "document")

    recorded_set = {(r["source_id"], r["sha256"])
                    for r in list(recorded_emails) + list(recorded_docs)}
    unrecorded = 0
    for sid, shas in on_disk.items():
        for sha in shas:
            if (sid, sha) not in recorded_set:
                unrecorded += 1

    print(f"verify_integrity: emails {len(recorded_emails)} recorded;"
          f" documents {len(recorded_docs)} recorded;"
          f" {len(problems)} problems, {unrecorded} on-disk blobs not in DB")
    for msg in problems:
        print(f"  !! {msg}")
    if unrecorded:
        print("  (not-yet-ingested blobs are not an error; run ingest.py)")
    return 1 if problems else 0


if __name__ == "__main__":
    sys.exit(run())
