"""Chain-of-custody verification via content hashes (path-agnostic).

Rebuilds the regenerable source_blob_index, then checks every recorded
(collection_id, sha256) in item_memberships is present on disk under that
collection with a matching hash. Schema B membership table.
"""
import sys

import blob_index
import config
import db
import utils_hash


def run():
    conn = db.connect()
    db.migrate(conn)

    blob_index.rebuild_all(conn)

    recorded = conn.execute(
        """SELECT collection_id, sha256, filename, membership_kind
           FROM item_memberships
           WHERE collection_id IS NOT NULL AND sha256 IS NOT NULL"""
    ).fetchall()
    conn.close()

    problems = []
    on_disk = {}
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

    for r in recorded:
        key = r["collection_id"]
        sha = r["sha256"]
        disk = on_disk.get(key, set())
        if sha not in disk:
            tag = r["filename"] or sha[:12]
            problems.append(
                f"MISSING {r['membership_kind']}: collection={key}"
                f" sha={sha[:12]}… ({tag})")

    recorded_set = {(r["collection_id"], r["sha256"]) for r in recorded}
    unrecorded = 0
    for sid, shas in on_disk.items():
        for sha in shas:
            if (sid, sha) not in recorded_set:
                unrecorded += 1

    n_email = sum(1 for r in recorded if r["membership_kind"] == "email")
    n_file = sum(1 for r in recorded if r["membership_kind"] == "file")
    print(f"verify_integrity: memberships email={n_email} file={n_file};"
          f" {len(problems)} problems, {unrecorded} on-disk blobs not in DB")
    for msg in problems:
        print(f"  !! {msg}")
    if unrecorded:
        print("  (not-yet-ingested blobs are not an error; run ingest.py)")
    return 1 if problems else 0


if __name__ == "__main__":
    sys.exit(run())
