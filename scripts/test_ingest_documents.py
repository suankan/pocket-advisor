"""Self-test for standalone-document ingestion (ingest_documents.py)
and date extraction (doc_dates.py).

Runs against a THROWAWAY temp fixture with config monkeypatched —
never touches the real ingestion-sources/ (AGENTS.md rule 1) or the
real DB. Covers: fresh ingest, idempotent re-run, duplicate-content
handling, chain-of-custody alarm, privilege retroactivity, and the
date-extraction priority ladder.

    venv/bin/python scripts/test_ingest_documents.py
"""
import shutil
import sys
import tempfile
from pathlib import Path

import config

# ---- monkeypatch config to a throwaway sandbox BEFORE anything runs ----
TMP = Path(tempfile.mkdtemp(prefix="pocket_advisor_doc_test_"))
config.PROJECT_ROOT = TMP  # paths are stored relative to PROJECT_ROOT
config.INGESTION_SOURCES = TMP / "sources"
config.OUTPUT_DIR = TMP / "output"
config.DB_PATH = config.OUTPUT_DIR / "test.db"
config.LOGS_DIR = config.OUTPUT_DIR / "logs"
config.REVIEW_QUEUE_CSV = config.LOGS_DIR / "review_queue.csv"
config.TEXT_DOCUMENTS_DIR = config.OUTPUT_DIR / "text" / "documents"
config.DOCUMENTS_EXTRACTED_DIR = config.OUTPUT_DIR / "documents_extracted"
config.OCR_REVIEW_DIR = config.OUTPUT_DIR / "ocr_review"
config.DOCUMENT_FOLDERS = {"docs"}
config.PRIVILEGED_FOLDERS = {"privileged-placeholder"}  # not "docs" initially

import db                # noqa: E402  (after monkeypatch, reads config at call time)
import doc_dates         # noqa: E402
import ingest_documents  # noqa: E402

DOCS = config.INGESTION_SOURCES / "docs"
FAILURES = []


def check(name, cond, detail=""):
    status = "ok" if cond else "FAIL"
    print(f"  [{status}] {name}" + (f" — {detail}" if detail and not cond else ""))
    if not cond:
        FAILURES.append(name)


def make_xlsx(path: Path, cell_lines):
    import openpyxl
    path.parent.mkdir(parents=True, exist_ok=True)
    wb = openpyxl.Workbook()
    ws = wb.active
    for i, line in enumerate(cell_lines, 1):
        ws.cell(row=i, column=1, value=line)
    wb.save(str(path))


def q1(sql, *params):
    conn = db.connect()
    row = conn.execute(sql, params).fetchone()
    conn.close()
    return row


def main():
    print("== doc_dates unit checks ==")
    d = doc_dates.extract_document_date(
        "Statement Period\n14 November 2025 - 16 February 2026\ntxns...",
        "x.pdf", "2026-01-01")
    check("keyword range takes later date",
          d[:3] == ("2026-02-16", "extracted_text", "keyword:statement period"), str(d))
    d = doc_dates.extract_document_date(
        "Period Dates: 01/04/2026 - 30/04/2026\nPay date: 15/04/2026",
        "x.pdf", "2026-01-01")
    check("pay date beats period range",
          d[:3] == ("2026-04-15", "extracted_text", "keyword:pay date"), str(d))
    d = doc_dates.extract_document_date(
        "                     Statement Period\n"
        "                     14 November 2025 - 16 February 2026\n",
        "x.pdf", "2026-01-01")
    check("pdftotext column padding: range still takes later date",
          d[:3] == ("2026-02-16", "extracted_text", "keyword:statement period"),
          str(d))
    d = doc_dates.extract_document_date(
        "Pay date: 15/04/2026\nSuper contribution due 28/07/2026\n",
        "x.pdf", "2026-01-01")
    check("unrelated later date in window does NOT win (not a range)",
          d[0] == "2026-04-15", str(d))
    d = doc_dates.extract_document_date(
        "AustralianSuper\n23 September 2025\nDear Member,", "x.pdf", "2026-01-01")
    check("bare dateline", d[:3] == ("2025-09-23", "extracted_text", "dateline"), str(d))
    d = doc_dates.extract_document_date(
        "Дата выписки: 18 мая 2026", "x.pdf", "2026-01-01")
    check("russian keyword + month",
          d[:2] == ("2026-05-18", "extracted_text"), str(d))
    d = doc_dates.extract_document_date(
        "MR J CITIZEN     Period            19 Jul 2025 - 18 Oct 2025\n"
        "19 Jul OPENING BALANCE 11,068.68 CR", "x.pdf", "2026-01-01")
    check("CBA bare-'period' keyword, abbrev months, range end",
          d[:3] == ("2025-10-18", "extracted_text", "keyword:period"), str(d))
    d = doc_dates.extract_document_date(
        "Statement Period                 06 Jan 26 to 05 Feb 26\n",
        "x.pdf", "2026-01-01")
    check("Qantas 2-digit years parsed, range end",
          d[:3] == ("2026-02-05", "extracted_text", "keyword:statement period"),
          str(d))
    d = doc_dates.extract_document_date(
        "Statement starts 15 February 2025\nStatement ends 14 August 2025\n",
        "x.pdf", "2026-01-01")
    check("NAB 'statement ends' beats 'statement starts' line",
          d[0] == "2025-08-14", str(d))
    d = doc_dates.extract_document_date(
        "no header dates\n" + "body text " * 200 + "promo since 09/09/2025",
        "Statement - 06.01.2026 to 05.02.2026.pdf", "2026-01-01")
    check("filename range beats noisy fulltext body date",
          d[:2] == ("2026-02-05", "filename"), str(d))
    d = doc_dates.extract_document_date("", "JCitizen_payslip_20260415.pdf", "2026-01-01")
    check("filename compact fallback",
          d[:3] == ("2026-04-15", "filename", "filename_yyyymmdd"), str(d))
    d = doc_dates.extract_document_date("no dates at all", "nodate.pdf", "2026-01-07")
    check("mtime last resort", d[:2] == ("2026-01-07", "mtime"), str(d))
    d = doc_dates.extract_document_date(
        "This was updated recently. Nothing else.", "nodate.pdf", "2026-01-07")
    check("'dated' does not match inside 'updated'", d[1] == "mtime", str(d))

    print("== fixture ingest ==")
    make_xlsx(DOCS / "statement.xlsx", ["Statement date: 15/04/2026", "txn txn txn"])
    make_xlsx(DOCS / "sub" / "letter_ru.xlsx", ["18 мая 2026", "Уважаемый Суан"])
    (DOCS / "notes.txt").write_text("unsupported filetype", encoding="utf-8")
    (DOCS / ".DS_Store").write_bytes(b"junk")

    db.init()
    stats = ingest_documents.run()
    check("fresh run: 3 new", stats["new"] == 3, str(stats))
    check("fresh run: 0 errors/alarms",
          stats["errors"] == 0 and stats["custody_alarm"] == 0, str(stats))

    row = q1("SELECT d.doc_date, d.doc_date_source, d.doc_date_detail, e.date_utc,"
             " e.source_kind, e.subject FROM documents d JOIN emails e"
             " ON e.id=d.email_id WHERE d.filename='statement.xlsx'")
    check("statement date extracted",
          row["doc_date"] == "2026-04-15"
          and row["doc_date_source"] == "extracted_text"
          and row["doc_date_detail"] == "keyword:statement date", dict(row).__repr__())
    check("emails row: full ISO timestamp + source_kind",
          row["date_utc"] == "2026-04-15T00:00:00+00:00"
          and row["source_kind"] == "document", dict(row).__repr__())

    row = q1("SELECT doc_date, doc_date_detail FROM documents"
             " WHERE filename='letter_ru.xlsx'")
    check("russian dateline in nested subfolder",
          row["doc_date"] == "2026-05-18", dict(row).__repr__())

    row = q1("SELECT d.is_skipped, e.body_text_path FROM documents d"
             " JOIN emails e ON e.id=d.email_id WHERE d.filename='notes.txt'")
    check("unsupported: skipped, body_text_path NULL (inert downstream)",
          row["is_skipped"] == 1 and row["body_text_path"] is None, dict(row).__repr__())

    print("== idempotency ==")
    stats = ingest_documents.run()
    check("re-run: 0 new, 3 skipped",
          stats["new"] == 0 and stats["skipped"] == 3, str(stats))

    print("== duplicate content ==")
    shutil.copyfile(DOCS / "statement.xlsx", DOCS / "copy_of_statement.xlsx")
    stats = ingest_documents.run()
    check("duplicate flagged, not indexed",
          stats["duplicate_content"] == 1 and stats["new"] == 0, str(stats))
    row = q1("SELECT COUNT(*) c FROM documents")
    check("still 3 documents rows", row["c"] == 3, str(row["c"]))
    (DOCS / "copy_of_statement.xlsx").unlink()

    print("== chain-of-custody alarm (fixture only — never real sources) ==")
    (DOCS / "notes.txt").write_text("TAMPERED CONTENT", encoding="utf-8")
    stats = ingest_documents.run()
    check("tamper detected, not re-ingested",
          stats["custody_alarm"] == 1 and stats["new"] == 0, str(stats))

    print("== privilege retroactivity ==")
    row = q1("SELECT COUNT(*) c FROM emails WHERE source_kind='document'"
             " AND is_privileged=1")
    check("not privileged before folder added", row["c"] == 0, str(row["c"]))
    config.PRIVILEGED_FOLDERS = {"privileged-placeholder", "docs"}
    ingest_documents.run()
    row = q1("SELECT COUNT(*) c FROM emails WHERE source_kind='document'"
             " AND is_privileged=0")
    check("all documents upgraded retroactively", row["c"] == 0, str(row["c"]))

    shutil.rmtree(TMP)
    print(f"\n{'ALL PASS' if not FAILURES else f'{len(FAILURES)} FAILURE(S): {FAILURES}'}")
    return 0 if not FAILURES else 1


if __name__ == "__main__":
    sys.exit(main())
