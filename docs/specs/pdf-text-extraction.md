# PDF text extraction

**Status:** SHIPPED  
**Date:** 2026-07-17

## Decision

Every canonical PDF or image text extraction uses exactly one logical sequence:

1. OCRmyPDF redo OCR, retaining existing visible text and adding a
   positioned text layer for text found in raster images.
2. Poppler `pdftotext -layout` over that temporary OCR derivative.

This is the only PDF or image text sequence, including per-page PDF text
associated with the visual retrieval channel.

The OCR derivative is regenerable working data and is deleted after text
extraction. Evidence originals are never modified.

## Failure semantics

OCRmyPDF or `pdftotext` failure aborts extraction for that PDF. Existing
ingestion callers record the item/attachment as an extraction error with
the command failure; they must not silently choose a different algorithm.
This includes PDFs that OCRmyPDF redo cannot process, such as digitally
signed fillable forms.

## Acceptance

- `extract_pdf(path)` invokes OCRmyPDF with redo OCR before invoking
  `pdftotext -layout` on the generated derivative.
- It returns only the `pdftotext -layout` output and records one stable
  extraction method identifier.
- The temporary OCR PDF is not retained after success or failure.
- Non-zero exit from either program raises a useful extraction error.
- Every PDF text consumer uses the OCRmyPDF derivative rather than a
  separate extraction algorithm.
- Setup documentation declares OCRmyPDF as a required system dependency.

## Verification

- Focused unit tests assert command order, returned text, method metadata,
  cleanup, and hard failures for each command.
- Run the focused test against the installed local binaries.
- Run the existing project test suite.
- Re-run the known `tmp/test/20230331.pdf` fixture and confirm its output
  equals direct `pdftotext -layout` while retaining statement layout.
