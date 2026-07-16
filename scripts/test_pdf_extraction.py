"""Focused tests for the canonical PDF extraction sequence."""
import subprocess
import sys
import tempfile
from pathlib import Path
from unittest.mock import patch

sys.path.insert(0, str(Path(__file__).resolve().parent))

import extraction


def completed(args, returncode=0, stdout=b"", stderr=b""):
    return subprocess.CompletedProcess(args, returncode, stdout, stderr)


def test_success():
    calls = []
    derivative_path = None

    def fake_run(args, timeout=120):
        nonlocal derivative_path
        calls.append((args, timeout))
        if args[0] == "ocrmypdf":
            derivative_path = Path(args[-1])
            derivative_path.write_bytes(b"derived PDF")
            return completed(args)
        assert Path(args[2]) == derivative_path
        assert derivative_path.is_file()
        return completed(args, stdout="Date  Description      Debit  Credit\n".encode())

    with tempfile.TemporaryDirectory() as tmp:
        source = Path(tmp) / "source.pdf"
        source.write_bytes(b"evidence PDF")
        with patch.object(extraction, "run_cmd", side_effect=fake_run):
            text, method, confidence = extraction.extract_pdf(source)

    assert text == "Date  Description      Debit  Credit\n"
    assert method == "ocrmypdf_redo_pdftotext_layout"
    assert confidence is None
    assert [call[0][0] for call in calls] == ["ocrmypdf", "pdftotext"]
    assert calls[0][0][1:3] == ["--redo-ocr", "--output-type"]
    assert calls[1][0][:3] == ["pdftotext", "-layout", str(derivative_path)]
    assert derivative_path is not None and not derivative_path.exists()


def test_ocrmypdf_failure_is_hard_failure():
    calls = []

    def fake_run(args, timeout=120):
        calls.append(args)
        return completed(args, returncode=2, stderr=b"fillable form")

    with tempfile.TemporaryDirectory() as tmp:
        source = Path(tmp) / "signed-form.pdf"
        source.write_bytes(b"evidence PDF")
        with patch.object(extraction, "run_cmd", side_effect=fake_run):
            try:
                extraction.extract_pdf(source)
            except RuntimeError as exc:
                assert "ocrmypdf --redo-ocr failed (2): fillable form" in str(exc)
            else:
                raise AssertionError("OCRmyPDF failure did not propagate")

    assert len(calls) == 1


def test_page_layout_uses_same_derivative():
    calls = []

    def fake_run(args, timeout=120):
        calls.append(args)
        return completed(args, stdout=b"page two\n")

    with patch.object(extraction, "run_cmd", side_effect=fake_run):
        text = extraction.extract_pdf_layout(Path("ocr.pdf"), page_number=2)

    assert text == "page two\n"
    assert calls == [[
        "pdftotext", "-layout", "-f", "2", "-l", "2", "ocr.pdf", "-",
    ]]


def test_image_uses_same_extraction_sequence():
    expected = ("image text\n", "ocrmypdf_redo_pdftotext_layout", None)
    with patch.object(extraction, "extract_pdf", return_value=expected) as run:
        result = extraction.extract_image(Path("scan.png"))
    assert result == expected
    run.assert_called_once_with(Path("scan.png"))


def test_pdftotext_failure_is_hard_failure_and_cleans_up():
    derivative_path = None

    def fake_run(args, timeout=120):
        nonlocal derivative_path
        if args[0] == "ocrmypdf":
            derivative_path = Path(args[-1])
            derivative_path.write_bytes(b"derived PDF")
            return completed(args)
        return completed(args, returncode=1, stderr=b"syntax error")

    with tempfile.TemporaryDirectory() as tmp:
        source = Path(tmp) / "source.pdf"
        source.write_bytes(b"evidence PDF")
        with patch.object(extraction, "run_cmd", side_effect=fake_run):
            try:
                extraction.extract_pdf(source)
            except RuntimeError as exc:
                assert "pdftotext -layout failed (1): syntax error" in str(exc)
            else:
                raise AssertionError("pdftotext failure did not propagate")

    assert derivative_path is not None and not derivative_path.exists()


def main():
    tests = (
        test_success,
        test_ocrmypdf_failure_is_hard_failure,
        test_page_layout_uses_same_derivative,
        test_image_uses_same_extraction_sequence,
        test_pdftotext_failure_is_hard_failure_and_cleans_up,
    )
    for test in tests:
        test()
        print(f"  OK  {test.__name__}")
    print("ALL PASS")


if __name__ == "__main__":
    main()
