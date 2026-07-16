"""Ingestion orchestrator.

Pipeline stages (positional):

    ingest all              # parse + documents + attachments + thread
                            # + --embed all (gated by ingestion.embed_*)
    ingest parse            # .eml -> DB + body text + attachment copies
    ingest documents        # standalone documents
    ingest attachments      # attachment text extraction / OCR
    ingest thread           # thread reconstruction (full recompute)
    ingest transactions     # heuristic transaction extraction

Embedding channels (--embed):

    ingest --embed text     # chunk + text embed + vector index
    ingest --embed images   # page-image rasterize + omni embed
    ingest --embed all      # text iff ingestion.embed_text;
                            # images iff ingestion.embed_images

Stages and --embed may be combined, e.g.:

    ingest all --embed images   # full pipeline + force image index
    ingest --embed all          # re-embed only (no re-parse)

(All spellings via the single entrypoint: ./pocket-advisor.py ingest …)

Safe to re-run: already-ingested files / embedded units are skipped.
"""
from __future__ import annotations

import sys

import config
import db

_STAGES = ("all", "parse", "documents", "attachments", "thread", "transactions")
# Deprecated bare stage names → --embed modes (still accepted for one release)
_LEGACY_EMBED_STAGES = {
    "text": "text",
    "embed": "text",
    "images": "images",
}


def _run_embed_text() -> None:
    import embed
    embed.run()


def _run_embed_images() -> None:
    import embed_images
    embed_images.run()


def _run_embed(mode: str, *, respect_config: bool) -> None:
    """mode: text | images | all.

    When respect_config is True (only for --embed all / stage all's
    default embed pass), honor ingestion.embed_text / embed_images.
    Explicit --embed text|images always runs the named channel.
    """
    if mode == "text":
        if respect_config and not config.EMBED_TEXT:
            print("embed text: skipped (ingestion.embed_text is false)")
            return
        _run_embed_text()
        return

    if mode == "images":
        if respect_config and not config.EMBED_IMAGES:
            print("embed images: skipped (ingestion.embed_images is false)")
            return
        _run_embed_images()
        return

    if mode == "all":
        # Always gated by the two knobs
        if config.EMBED_TEXT:
            _run_embed_text()
        else:
            print("embed all: skipping text (ingestion.embed_text is false)")
        if config.EMBED_IMAGES:
            _run_embed_images()
        else:
            print("embed all: skipping images (ingestion.embed_images is false)")
        return

    raise SystemExit(f"unknown --embed mode {mode!r}")


def cli(stage: str | None, embed_mode: str | None) -> int:
    """CLI body — the parser lives in pocket-advisor.py.
    stage: one of _STAGES (or a legacy embed stage name) or None;
    embed_mode: text | images | all | None."""
    # Legacy: `ingest text|embed|images` → --embed …
    if stage in _LEGACY_EMBED_STAGES:
        if embed_mode is not None:
            raise SystemExit(
                f"ingest: do not combine legacy stage {stage!r} with --embed "
                f"(use: ingest --embed {_LEGACY_EMBED_STAGES[stage]})")
        legacy = stage
        embed_mode = _LEGACY_EMBED_STAGES[stage]
        stage = None
        print(
            f"note: 'ingest {legacy}' is deprecated; "
            f"use 'ingest --embed {embed_mode}'",
            file=sys.stderr,
        )

    if stage is not None and stage not in _STAGES:
        raise SystemExit(
            f"ingest: unknown stage {stage!r}; choose from "
            f"{', '.join(_STAGES)} or --embed text|images|all")

    # Default: full core pipeline when nothing specified
    if stage is None and embed_mode is None:
        stage = "all"

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
    # `all` includes gated embed-all (text/images per config knobs)
    if stage == "all":
        _run_embed("all", respect_config=True)
    if stage == "transactions":
        # R-04b: structured transactions moved to scripts/transactions.py
        # (parse + link are explicit steps, never auto-run on ingest)
        import transactions
        conn = db.connect()
        try:
            transactions.run_parse(conn, transactions.workspace_dir())
            transactions.run_link(conn, transactions.workspace_dir())
        finally:
            conn.close()

    # Explicit --embed (and legacy mapped modes); may follow a stage
    if embed_mode is not None:
        if stage == "all" and embed_mode == "all":
            # already ran gated embed-all above
            pass
        elif stage == "all" and embed_mode == "text":
            # stage all already tried text (if enabled); explicit text
            # re-runs the channel even if embed_text is false
            _run_embed("text", respect_config=False)
        elif stage == "all" and embed_mode == "images":
            _run_embed("images", respect_config=False)
        else:
            # Standalone --embed: `all` respects knobs; named modes force
            respect = embed_mode == "all"
            _run_embed(embed_mode, respect_config=respect)

    return 0
