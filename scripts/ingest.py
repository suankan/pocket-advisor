"""Ingestion orchestrator.

    ingest.py all          # run every stage (idempotent, incremental)
    ingest.py parse        # Stage 1: .eml -> DB + body text + attachment copies
    ingest.py documents    # Stage 1b: standalone documents (additional-documents/)
    ingest.py attachments  # Stage 2: attachment text extraction / OCR
    ingest.py thread       # Stage 3: thread reconstruction (full recompute)
    ingest.py embed        # Stage 4: chunk + embed + vector index

Safe to re-run at any time: already-ingested files, processed
attachments, and embedded chunks are skipped automatically.
"""
import sys

import db


def main():
    stage = sys.argv[1] if len(sys.argv) > 1 else "all"
    db.init()
    if stage in ("all", "parse"):
        import parse_eml
        parse_eml.run()
    if stage in ("all", "documents"):
        import ingest_documents
        ingest_documents.run()
    if stage in ("all", "attachments"):
        import extract_attachments
        extract_attachments.run()
    if stage in ("all", "thread"):
        import thread_linker
        thread_linker.run()
    if stage in ("all", "embed"):
        import embed
        embed.run()
    if stage in ("images",):
        import embed_images
        embed_images.run()
    if stage in ("transactions",):
        import extract_transactions
        extract_transactions.run()
    if stage not in ("all", "parse", "documents", "attachments", "thread",
                     "embed", "images", "transactions"):
        print(__doc__)
        return 2
    return 0


if __name__ == "__main__":
    sys.exit(main())
