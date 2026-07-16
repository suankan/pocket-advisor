"""Shared text-extraction primitives for binary files.

Used by BOTH ingestion paths — email attachments
(extract_attachments.py) and standalone documents
(ingest_documents.py) — so extraction and OCR-caveat behavior cannot
drift between them. Every function here is pure with respect to the
database: Path in, text out.
"""
import subprocess
import tempfile
from contextlib import contextmanager
from pathlib import Path

import config


def run_cmd(args, timeout=120):
    return subprocess.run(args, capture_output=True, timeout=timeout)


@contextmanager
def ocrmypdf_redo_derivative(path: Path):
    """Yield a temporary OCRmyPDF-redone PDF from a PDF or image input."""
    with tempfile.TemporaryDirectory(prefix="pa_ocrmypdf_") as tmp:
        derivative = Path(tmp) / "ocr.pdf"
        ocr = run_cmd(
            [
                "ocrmypdf",
                "--redo-ocr",
                "--output-type", "pdf",
                "--optimize", "0",
                "--jobs", "1",
                "--language", config.OCR_LANGS,
                str(path),
                str(derivative),
            ],
            timeout=1800,
        )
        if ocr.returncode != 0:
            detail = ocr.stderr.decode("utf-8", errors="replace").strip()
            raise RuntimeError(
                f"ocrmypdf --redo-ocr failed ({ocr.returncode})"
                + (f": {detail}" if detail else "")
            )
        yield derivative


def extract_pdf_layout(path: Path, page_number: int | None = None):
    """Run pdftotext -layout on a PDF, optionally for one page."""
    args = ["pdftotext", "-layout"]
    if page_number is not None:
        args.extend(["-f", str(page_number), "-l", str(page_number)])
    args.extend([str(path), "-"])
    extracted = run_cmd(args, timeout=300)
    if extracted.returncode != 0:
        detail = extracted.stderr.decode("utf-8", errors="replace").strip()
        raise RuntimeError(
            f"pdftotext -layout failed ({extracted.returncode})"
            + (f": {detail}" if detail else "")
        )
    return extracted.stdout.decode("utf-8", errors="replace")


def extract_pdf(path: Path):
    """OCRmyPDF redo, then pdftotext -layout; return text + metadata."""
    with ocrmypdf_redo_derivative(path) as derivative:
        text = extract_pdf_layout(derivative)

    return text, "ocrmypdf_redo_pdftotext_layout", None


def extract_image(path: Path):
    """Use the same OCRmyPDF → layout-text sequence for an image input."""
    return extract_pdf(path)


def extract_docx(path: Path):
    import docx
    d = docx.Document(str(path))
    parts = [p.text for p in d.paragraphs if p.text.strip()]
    for table in d.tables:
        for row in table.rows:
            parts.append("\t".join(c.text for c in row.cells))
    return "\n".join(parts)

def extract_xlsx(path: Path):
    import openpyxl
    wb = openpyxl.load_workbook(str(path), data_only=True, read_only=True)
    parts = []
    for ws in wb.worksheets:
        parts.append(f"[sheet: {ws.title}]")
        for row in ws.iter_rows(values_only=True):
            cells = [str(c) for c in row if c is not None]
            if cells:
                parts.append("\t".join(cells))
    wb.close()
    return "\n".join(parts)
