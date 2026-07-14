"""Ingestion orchestrator.

Pipeline stages (positional):

    ingest.py all              # parse + documents + attachments + thread
                               # + --embed all (gated by ingestion.embed_*)
    ingest.py parse            # .eml -> DB + body text + attachment copies
    ingest.py documents        # standalone documents
    ingest.py attachments      # attachment text extraction / OCR
    ingest.py thread           # thread reconstruction (full recompute)
    ingest.py transactions     # heuristic transaction extraction

Embedding channels (--embed):

    ingest.py --embed text     # chunk + text embed + vector index
    ingest.py --embed images   # page-image rasterize + omni embed
    ingest.py --embed all      # text iff ingestion.embed_text;
                               # images iff ingestion.embed_images

Stages and --embed may be combined, e.g.:

    ingest.py all --embed images   # full pipeline + force image index
    ingest.py --embed all          # re-embed only (no re-parse)

Safe to re-run: already-ingested files / embedded units are skipped.
"""
from __future__ import annotations

import argparse
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


def _build_parser() -> argparse.ArgumentParser:
    p = argparse.ArgumentParser(
        prog="ingest.py",
        description="Ingestion orchestrator (parse / thread / embed).",
        formatter_class=argparse.RawDescriptionHelpFormatter,
        epilog=__doc__,
    )
    p.add_argument(
        "stage",
        nargs="?",
        default=None,
        help="Pipeline stage: all | parse | documents | attachments | "
             "thread | transactions. Default: all when no --embed; "
             "omitted when only --embed is used.",
    )
    p.add_argument(
        "--embed",
        choices=("text", "images", "all"),
        default=None,
        metavar="MODE",
        help="Embedding channel: text | images | all "
             "(all respects ingestion.embed_text / embed_images).",
    )
    return p


def main(argv: list[str] | None = None) -> int:
    argv = list(sys.argv[1:] if argv is None else argv)
    parser = _build_parser()
    args = parser.parse_args(argv)

    stage = args.stage
    embed_mode = args.embed

    # Legacy: `ingest.py text|embed|images` → --embed …
    if stage in _LEGACY_EMBED_STAGES:
        if embed_mode is not None:
            parser.error(
                f"do not combine legacy stage {stage!r} with --embed "
                f"(use: ingest.py --embed {_LEGACY_EMBED_STAGES[stage]})")
        legacy = stage
        embed_mode = _LEGACY_EMBED_STAGES[stage]
        stage = None
        print(
            f"note: 'ingest.py {legacy}' is deprecated; "
            f"use 'ingest.py --embed {embed_mode}'",
            file=sys.stderr,
        )

    if stage is not None and stage not in _STAGES:
        parser.error(
            f"unknown stage {stage!r}; choose from {', '.join(_STAGES)} "
            f"or --embed text|images|all")

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
        import extract_transactions
        extract_transactions.run()

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


if __name__ == "__main__":
    sys.exit(main())
