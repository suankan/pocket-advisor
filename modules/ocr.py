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
import os
import subprocess
from pathlib import Path

from modules.config import OCRMYPDF_TIMEOUT_SEC, PDFTOTEXT_TIMEOUT_SEC

# Method label recorded on every row this pipeline extracts.
EXTRACTION_METHOD = "ocrmypdf_redo_clean_pdftotext_layout"


class OcrError(RuntimeError):
    """ocrmypdf or pdftotext failed for one file."""


def run_command(args: list[str],
                timeout: int) -> subprocess.CompletedProcess[bytes]:
    return subprocess.run(args, capture_output=True, timeout=timeout)


def _detail(result: subprocess.CompletedProcess[bytes]) -> str:
    return result.stderr.decode("utf-8", errors="replace").strip()


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
    if result.returncode == 0:
        return None
    detail = _detail(result)
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
