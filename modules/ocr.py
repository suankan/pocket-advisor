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
import signal
import subprocess
import threading
import time
from dataclasses import dataclass
from pathlib import Path

from modules.config import OCRMYPDF_TIMEOUT_SEC, PDFTOTEXT_TIMEOUT_SEC

# Versioned wrapper recipe. The complete extraction-method value also includes
# local tool versions and language configuration, so successful old text is
# reprocessed when its producing recipe is no longer current.
OCR_RECIPE_VERSION = 1
TEXT_RECIPE_VERSION = 1
PDF_TEXT_RECIPE_VERSION = 3
EXTRACTION_METHOD = "pdf-text-v3"


@dataclass(frozen=True, slots=True)
class PdfRecipes:
    ocr: str
    text: str
    combined: str


_ACTIVE_LOCK = threading.Lock()
_ACTIVE_PROCESSES: set[subprocess.Popen[bytes]] = set()

# Set on Ctrl+C / SIGTERM so in-flight workers stop pulling new work and
# long subprocess waits return promptly.  One-shot per process lifetime;
# the pipeline does not resume a cancelled run.
_INTERRUPTED = threading.Event()


def request_interrupt() -> None:
    """Signal the pipeline that an interrupt arrived; idempotent."""
    _INTERRUPTED.set()


def is_interrupted() -> bool:
    """True once an interrupt has been requested."""
    return _INTERRUPTED.is_set()


class OcrError(RuntimeError):
    """ocrmypdf or pdftotext failed for one file."""


def run_command(args: list[str],
                timeout: int) -> subprocess.CompletedProcess[bytes]:
    process = subprocess.Popen(
        args, stdout=subprocess.PIPE, stderr=subprocess.PIPE,
        start_new_session=True)
    with _ACTIVE_LOCK:
        _ACTIVE_PROCESSES.add(process)
    try:
        try:
            stdout, stderr = _communicate_until_done(process, timeout)
        except subprocess.TimeoutExpired:
            _terminate_process(process)
            stdout, stderr = process.communicate()
            raise
        return subprocess.CompletedProcess(
            args, process.returncode, stdout, stderr)
    finally:
        with _ACTIVE_LOCK:
            _ACTIVE_PROCESSES.discard(process)


def _communicate_until_done(
        process: subprocess.Popen[bytes], timeout: int,
) -> tuple[bytes, bytes]:
    """communicate() that also bails early when an interrupt is requested.

    A plain communicate(timeout=) blocks until the child exits or the timeout
    elapses; on Ctrl+C we want to stop waiting promptly so the worker can
    unwind instead of finishing a long OCR job we no longer need.
    """
    deadline = time.monotonic() + timeout
    while True:
        remaining = deadline - time.monotonic()
        if remaining <= 0:
            raise subprocess.TimeoutExpired(process.args, timeout)
        if is_interrupted():
            _terminate_process(process)
            return process.communicate()
        try:
            return process.communicate(timeout=min(remaining, 0.25))
        except subprocess.TimeoutExpired:
            continue


def _terminate_process(process: subprocess.Popen[bytes]) -> None:
    if process.poll() is not None:
        return
    try:
        os.killpg(process.pid, signal.SIGTERM)
    except (ProcessLookupError, PermissionError):
        process.terminate()
    try:
        process.wait(timeout=2)
        return
    except subprocess.TimeoutExpired:
        pass
    try:
        os.killpg(process.pid, signal.SIGKILL)
    except (ProcessLookupError, PermissionError):
        process.kill()
    process.wait()


def cancel_active_commands() -> None:
    """Terminate Stage 3 child process groups after an interrupt."""
    _INTERRUPTED.set()
    with _ACTIVE_LOCK:
        active = tuple(_ACTIVE_PROCESSES)
    for process in active:
        _terminate_process(process)


def _detail(result: subprocess.CompletedProcess[bytes]) -> str:
    return result.stderr.decode("utf-8", errors="replace").strip()


def _tool_version(args: list[str]) -> str:
    try:
        result = run_command(args, timeout=20)
    except subprocess.TimeoutExpired as exc:
        raise OcrError(
            f"cannot fingerprint {' '.join(args)}: timed out after 20s") \
            from exc
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


def _recipe_id(prefix: str, recipe: dict) -> str:
    payload = json.dumps(
        recipe, sort_keys=True, separators=(",", ":"),
        ensure_ascii=True).encode("utf-8")
    return f"{prefix}:{hashlib.sha256(payload).hexdigest()[:20]}"


def pdf_recipes(*, langs: str) -> PdfRecipes:
    """Return independently fingerprinted OCR and text recipes."""
    ocr_recipe = {
        "recipe_version": OCR_RECIPE_VERSION,
        "ocrmypdf": {
            "version": _tool_version(["ocrmypdf", "--version"]),
            # Nested OCR process pools are forbidden: each Stage 3 worker
            # runs exactly one ocrmypdf child with --jobs 1
            # (docs/features/pdf-to-text-pipeline-design.md).
            "args": ["--redo-ocr", "--clean", "--output-type", "pdf",
                      "--optimize", "0", "--language", langs, "--jobs", "1"],
        },
        "accept_nonzero_output_for_text_gate": True,
        "fallback_to_verified_original_when_derivative_missing": True,
    }
    text_recipe = {
        "recipe_version": TEXT_RECIPE_VERSION,
        "pdftotext": {
            "version": _tool_version(["pdftotext", "-v"]),
            "args": ["-layout"],
        },
        "successful_present_readable_output_is_acceptance_gate": True,
    }
    ocr_id = _recipe_id("pdf-ocr-v1", ocr_recipe)
    text_id = _recipe_id("pdftotext-v1", text_recipe)
    combined = _recipe_id(EXTRACTION_METHOD, {
        "recipe_version": PDF_TEXT_RECIPE_VERSION,
        "ocr_recipe": ocr_id,
        "text_recipe": text_id,
    })
    return PdfRecipes(ocr=ocr_id, text=text_id, combined=combined)


def pdf_text_extraction_method(*, langs: str) -> str:
    """Current combined Stage 3 freshness fingerprint recorded in SQLite."""
    return pdf_recipes(langs=langs).combined


def ocr_to_derivative(source: Path, derivative: Path,
                      *, langs: str, jobs: int) -> str | None:
    """OCRmyPDF the source into a persistent derivative PDF.

    OCRmyPDF may produce a derivative and then return non-zero because its
    final structural validation found warnings.  Such an output is still
    eligible for the authoritative ``pdftotext`` extraction attempt.  Return
    the OCR diagnostic in that case so the caller can record it after text
    extraction succeeds.  A stale derivative is removed first so a failed
    attempt can never make an older output look current.
    """
    if jobs < 1:
        raise ValueError("ocrmypdf jobs must be positive")
    derivative.parent.mkdir(parents=True, exist_ok=True)
    derivative.unlink(missing_ok=True)
    try:
        result = run_command(
            ["ocrmypdf", "--redo-ocr", "--clean",
             "--output-type", "pdf", "--optimize", "0",
             "--language", langs,
             "--jobs", str(jobs),
             str(source), str(derivative)],
            timeout=OCRMYPDF_TIMEOUT_SEC)
    except subprocess.TimeoutExpired as exc:
        raise OcrError(
            f"ocrmypdf timed out after {OCRMYPDF_TIMEOUT_SEC}s") from exc
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
    try:
        result = run_command(
            ["pdftotext", "-layout", str(pdf), str(txt)],
            timeout=PDFTOTEXT_TIMEOUT_SEC)
    except subprocess.TimeoutExpired as exc:
        raise OcrError(
            f"pdftotext timed out after {PDFTOTEXT_TIMEOUT_SEC}s") from exc
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
