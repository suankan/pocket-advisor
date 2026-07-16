"""Ingestion orchestrator.

Pipeline stages (positional):

    ingest all              # parse + documents + attachments + thread
                            # + text embedding when enabled
    ingest parse            # .eml -> DB + body text + attachment copies
    ingest documents        # standalone documents
    ingest attachments      # attachment text extraction / OCR
    ingest thread           # thread reconstruction (full recompute)
    ingest transactions     # heuristic transaction extraction

Embedding (--embed):

    ingest --embed text     # chunk + text embed + vector index
    ingest --embed all      # alias that respects ingestion.embed_text

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
}


def _run_embed_text() -> None:
    import embed
    embed.run()


def _run_embed(mode: str, *, respect_config: bool) -> None:
    """mode: text | all.

    When respect_config is True, honor ingestion.embed_text.
    Explicit --embed text always runs the channel.
    """
    if mode == "text":
        if respect_config and not config.EMBED_TEXT:
            print("embed text: skipped (ingestion.embed_text is false)")
            return
        _run_embed_text()
        return

    if mode == "all":
        if config.EMBED_TEXT:
            _run_embed_text()
        else:
            print("embed all: skipping text (ingestion.embed_text is false)")
        return

    raise SystemExit(f"unknown --embed mode {mode!r}")


def cli(stage: str | None, embed_mode: str | None) -> int:
    """CLI body — the parser lives in pocket-advisor.py.
    stage: one of _STAGES (or a legacy embed stage name) or None;
    embed_mode: text | all | None."""
    # Legacy: `ingest text|embed` → --embed text.
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
            f"{', '.join(_STAGES)} or --embed text|all")

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
    # `all` includes the config-gated text embed pass.
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
        else:
            # Standalone `all` respects the knob; named text forces it.
            respect = embed_mode == "all"
            _run_embed(embed_mode, respect_config=respect)

    return 0
