"""Chain-of-custody verification: re-hash every original under
workspace corpora/ (both .eml emails and standalone documents) and
compare against the recorded manifests.

Run before anything sensitive (privilege log, exporting material).
Exit 0 = clean; exit 1 = drift detected (details printed).
"""
import sys

import config
import db
import utils_hash


def check(recorded, on_disk, label):
    problems = []
    for rel, sha in recorded.items():
        p = on_disk.get(rel)
        if p is None:
            problems.append(f"MISSING {label}: {rel} (recorded but no longer on disk)")
        elif utils_hash.sha256_file(p) != sha:
            problems.append(f"MODIFIED {label}: {rel} (hash differs from ingestion record)")
    unrecorded = set(on_disk) - set(recorded)
    return problems, unrecorded


def run():
    conn = db.connect()
    db.migrate(conn)
    recorded_emails = {r["source_path"]: r["sha256"]
                       for r in conn.execute("SELECT source_path, sha256 FROM email_files")}
    recorded_docs = {r["source_path"]: r["sha256"]
                     for r in conn.execute("SELECT source_path, sha256 FROM documents")}
    conn.close()

    on_disk_emails = {str(p.relative_to(config.INGESTION_SOURCES)): p
                      for p in config.INGESTION_SOURCES.rglob("*.eml")}
    # identical filters to ingestion, guaranteed by sharing the walk
    from ingest_documents import iter_source_files
    on_disk_docs = {str(p.relative_to(config.INGESTION_SOURCES)): p
                    for p, _folder in iter_source_files()}

    email_problems, email_unrecorded = check(recorded_emails, on_disk_emails, "email")
    doc_problems, doc_unrecorded = check(recorded_docs, on_disk_docs, "document")
    problems = email_problems + doc_problems
    unrecorded = email_unrecorded | doc_unrecorded

    print(f"verify_integrity: emails {len(recorded_emails)} recorded /"
          f" {len(on_disk_emails)} on disk; documents {len(recorded_docs)}"
          f" recorded / {len(on_disk_docs)} on disk;"
          f" {len(problems)} problems, {len(unrecorded)} not yet ingested")
    for msg in problems:
        print(f"  !! {msg}")
    if unrecorded:
        print(f"  (not-yet-ingested files are not an error; run ingest.py)")
    return 1 if problems else 0


if __name__ == "__main__":
    sys.exit(run())
