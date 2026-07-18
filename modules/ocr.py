"""PDF → text pipeline wrappers: ocrmypdf then pdftotext (PDF-only).

Stage 3.2 command sequence (`docs/design.md`):

    ocrmypdf --redo-ocr --clean ... pdf-original/X  pdf-ocr/X-ocrmypdf.pdf
    pdftotext -layout pdf-ocr/X-ocrmypdf.pdf  pdf-to-text/X.txt

`--redo-ocr` preserves born-digital text layers; `--clean` denoises
scan input for OCR only (the design's --deskew/--clean-final are
incompatible with --redo-ocr and were dropped — see design decisions).
The OCR derivative is a persistent artifact: auditability, and
re-running pdftotext is free.

Subprocesses go through `run_command` so tests can patch one seam.
"""
import hashlib
import json
import os
import subprocess
from pathlib import Path

from modules.config import OCRMYPDF_TIMEOUT_SEC, PDFTOTEXT_TIMEOUT_SEC

# Versioned wrapper recipe. The complete extraction-method value also includes
# local tool versions and language configuration, so successful old text is
# reprocessed when its producing recipe is no longer current.
PDF_TEXT_RECIPE_VERSION = 2
EXTRACTION_METHOD = "pdf-text-v2"


class OcrError(RuntimeError):
    """ocrmypdf or pdftotext failed for one file."""


def run_command(args: list[str],
                timeout: int) -> subprocess.CompletedProcess[bytes]:
    return subprocess.run(args, capture_output=True, timeout=timeout)


def _detail(result: subprocess.CompletedProcess[bytes]) -> str:
    return result.stderr.decode("utf-8", errors="replace").strip()


def _tool_version(args: list[str]) -> str:
    result = run_command(args, timeout=20)
    if result.returncode != 0:
        detail = _detail(result)
        raise OcrError(
            f"cannot fingerprint {' '.join(args)} ({result.returncode})"
            + (f": {detail}" if detail else ""))
    output = (result.stdout + result.stderr).decode(
        "utf-8", errors="replace").strip()
    if not output:
        raise OcrError(f"cannot fingerprint {' '.join(args)}: empty version")
    return output.splitlines()[0].strip()


def pdf_text_extraction_method(*, langs: str) -> str:
    """Return the current Stage 3 recipe fingerprint recorded in SQLite."""
    recipe = {
        "recipe_version": PDF_TEXT_RECIPE_VERSION,
        "ocrmypdf": {
            "version": _tool_version(["ocrmypdf", "--version"]),
            "args": ["--redo-ocr", "--clean", "--output-type", "pdf",
                     "--optimize", "0", "--language", langs],
        },
        "pdftotext": {
            "version": _tool_version(["pdftotext", "-v"]),
            "args": ["-layout"],
        },
        "accept_nonzero_ocr_output_when_pdftotext_succeeds": True,
        "fallback_to_verified_original_when_derivative_missing": True,
    }
    payload = json.dumps(
        recipe, sort_keys=True, separators=(",", ":"),
        ensure_ascii=True).encode("utf-8")
    digest = hashlib.sha256(payload).hexdigest()[:20]
    return f"{EXTRACTION_METHOD}:{digest}"


def ocr_to_derivative(source: Path, derivative: Path,
                      *, langs: str) -> str | None:
    """OCRmyPDF the source into a persistent derivative PDF.

    OCRmyPDF may produce a derivative and then return non-zero because its
    final structural validation found warnings.  Such an output is still
    eligible for the authoritative ``pdftotext`` extraction attempt.  Return
    the OCR diagnostic in that case so the caller can record it after text
    extraction succeeds.  A stale derivative is removed first so a failed
    attempt can never make an older output look current.
    """
    derivative.parent.mkdir(parents=True, exist_ok=True)
    derivative.unlink(missing_ok=True)
    result = run_command(
        ["ocrmypdf", "--redo-ocr", "--clean",
         "--output-type", "pdf", "--optimize", "0",
         "--language", langs,
         "--jobs", str(os.process_cpu_count() or 1),
         str(source), str(derivative)],
        timeout=OCRMYPDF_TIMEOUT_SEC)
    if result.returncode == 0 and derivative.is_file():
        return None
    detail = _detail(result)
    if result.returncode == 0:
        return "ocrmypdf exited 0 but did not create the derivative"
    return (f"ocrmypdf exited {result.returncode}"
            + (f": {detail}" if detail else ""))


def pdf_to_text(pdf: Path, txt: Path) -> str:
    """pdftotext -layout into txt; returns the extracted text."""
    txt.parent.mkdir(parents=True, exist_ok=True)
    txt.unlink(missing_ok=True)
    result = run_command(
        ["pdftotext", "-layout", str(pdf), str(txt)],
        timeout=PDFTOTEXT_TIMEOUT_SEC)
    if result.returncode != 0:
        detail = _detail(result)
        raise OcrError(f"pdftotext -layout failed ({result.returncode})"
                       + (f": {detail}" if detail else ""))
    if not txt.is_file():
        raise OcrError("pdftotext -layout exited 0 but did not create "
                       f"the output file: {txt}")
    try:
        return txt.read_text(encoding="utf-8", errors="replace")
    except OSError as exc:
        raise OcrError(f"pdftotext output is not readable: {txt}: {exc}") \
            from exc
