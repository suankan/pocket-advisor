"""Per-document content-addressed PDF transform cache and workers.

This module owns no SQLite connection and never touches collection evidence.
Workers consume a verified document source original, write only
stage-temporary outputs, and return typed results. The coordinating PDF
stage publishes the canonical products directly into `documents.*` columns
and performs all database/review mutation.

`PdfTransformCache` is constructed once per PDF document, rooted at that
document's own `documents/<sha256>/transforms/` (`Config.document_artifacts`)
— every unique PDF already lives in its own top-level content-addressed
folder, so there is exactly one canonical product location per document,
referenced directly by every occurrence (native mount and/or email
attachment). No per-occurrence fan-out copy is made or needed.
"""
import json
import os
import tempfile
import time
from dataclasses import dataclass
from pathlib import Path

from modules.custody import (CustodyError, copy_verified, sha256_file,
                             write_verified)
from modules.ocr import (OcrError, PdfRecipes, ocr_to_derivative,
                         pdf_to_text)


_MANIFEST_VERSION = 1


def _recipe_digest(recipe: str) -> str:
    digest = recipe.rsplit(":", 1)[-1]
    if len(digest) != 20 or any(c not in "0123456789abcdef" for c in digest):
        raise ValueError(f"invalid PDF recipe identity: {recipe!r}")
    return digest


def _source_digest(source_sha256: str) -> str:
    if len(source_sha256) != 64 or any(
            c not in "0123456789abcdef" for c in source_sha256):
        raise ValueError(f"invalid PDF source identity: {source_sha256!r}")
    return source_sha256


def _atomic_json(target: Path, value: dict) -> None:
    target.parent.mkdir(parents=True, exist_ok=True)
    fd, raw_temp = tempfile.mkstemp(
        prefix=f".{target.name}.", suffix=".tmp", dir=target.parent)
    os.close(fd)
    temp = Path(raw_temp)
    try:
        write_verified(temp, json.dumps(
            value, sort_keys=True, indent=2).encode("utf-8"))
        os.replace(temp, target)
    finally:
        temp.unlink(missing_ok=True)


def copy_verified_atomic(source: Path, target: Path,
                         expected_sha256: str) -> bool:
    """Publish an independently verified plain copy; return whether written."""
    if target.is_file() and not target.is_symlink():
        try:
            if sha256_file(target) == expected_sha256:
                return False
        except OSError:
            pass
    target.parent.mkdir(parents=True, exist_ok=True)
    fd, raw_temp = tempfile.mkstemp(
        prefix=f".{target.name}.", suffix=".tmp", dir=target.parent)
    os.close(fd)
    temp = Path(raw_temp)
    try:
        copy_verified(source, temp, expected_sha256=expected_sha256)
        os.replace(temp, target)
    finally:
        temp.unlink(missing_ok=True)
    if sha256_file(target) != expected_sha256:
        raise CustodyError(
            f"atomic copy publication verification failed: {target}")
    return True


@dataclass(frozen=True, slots=True)
class OcrProduct:
    source_sha256: str
    recipe: str
    derivative_path: Path | None
    derivative_sha256: str | None
    direct_original_fallback: bool
    warning: str | None


@dataclass(frozen=True, slots=True)
class TextProduct:
    source_sha256: str
    ocr_recipe: str
    text_recipe: str
    text_path: Path
    text_sha256: str
    ocr: OcrProduct


@dataclass(frozen=True, slots=True)
class TransformRequest:
    source_sha256: str
    source_path: Path
    recipes: PdfRecipes
    cached_ocr: OcrProduct | None
    work_dir: Path
    langs: str
    ocrmypdf_jobs: int


@dataclass(frozen=True, slots=True)
class TransformResult:
    source_sha256: str
    derivative_temp: Path | None
    text_temp: Path | None
    warning: str | None
    direct_original_fallback: bool
    used_cached_ocr: bool
    ocr_seconds: float
    text_seconds: float
    started_at: float
    finished_at: float
    error: str | None


def run_transform(request: TransformRequest) -> TransformResult:
    """Worker entrypoint: OCR if needed, then run the authoritative text gate."""
    started = time.monotonic()
    ocr_seconds = text_seconds = 0.0
    derivative: Path | None = None
    warning: str | None = None
    direct_fallback = False
    used_cached = request.cached_ocr is not None
    text_path = request.work_dir / "output.txt"
    try:
        if sha256_file(request.source_path) != request.source_sha256:
            raise CustodyError(
                "workspace-cache PDF original no longer matches its"
                " recorded SHA-256")
        request.work_dir.mkdir(parents=True, exist_ok=True)
        if request.cached_ocr is not None:
            derivative = request.cached_ocr.derivative_path
            warning = request.cached_ocr.warning
            direct_fallback = request.cached_ocr.direct_original_fallback
        else:
            candidate = request.work_dir / "ocr.pdf"
            ocr_started = time.monotonic()
            try:
                warning = ocr_to_derivative(
                    request.source_path, candidate, langs=request.langs,
                    jobs=request.ocrmypdf_jobs)
            finally:
                ocr_seconds = time.monotonic() - ocr_started
            derivative = candidate if candidate.is_file() else None
            direct_fallback = derivative is None
            if direct_fallback:
                fallback = (
                    "used verified original because no OCR derivative exists")
                warning = f"{warning}; {fallback}" if warning else fallback

        text_started = time.monotonic()
        try:
            pdf_to_text(derivative or request.source_path, text_path)
        finally:
            text_seconds = time.monotonic() - text_started
        return TransformResult(
            source_sha256=request.source_sha256,
            derivative_temp=derivative if not used_cached else None,
            text_temp=text_path, warning=warning,
            direct_original_fallback=direct_fallback,
            used_cached_ocr=used_cached, ocr_seconds=ocr_seconds,
            text_seconds=text_seconds, started_at=started,
            finished_at=time.monotonic(), error=None)
    except (CustodyError, OcrError, OSError, ValueError) as exc:
        detail = f"{type(exc).__name__}: {exc}"
        if warning:
            detail = f"{warning}; {detail}"
        return TransformResult(
            source_sha256=request.source_sha256,
            derivative_temp=derivative if not used_cached else None,
            text_temp=None, warning=warning,
            direct_original_fallback=direct_fallback,
            used_cached_ocr=used_cached, ocr_seconds=ocr_seconds,
            text_seconds=text_seconds, started_at=started,
            finished_at=time.monotonic(),
            error=detail)


class PdfTransformCache:
    """Verified canonical products keyed by content+recipe, rooted at one
    document's own transforms_dir.

    A fresh instance is constructed per document (`root` is that document's
    `documents/<sha256>/transforms/`), so the identity-shard prefix an
    older, single-shared-cache-directory design needed
    (`source_sha256[:2]/source_sha256/...`) is no longer necessary — the
    directory itself already scopes everything under it to one document.
    """

    def __init__(self, root: Path):
        self.root = root

    def _safe_directory(self, directory: Path) -> None:
        root = self.root.resolve(strict=False)
        resolved = directory.resolve(strict=False)
        try:
            resolved.relative_to(root)
        except ValueError as exc:
            raise CustodyError(
                f"canonical PDF transform path escapes cache: {directory}") \
                from exc

    def _ocr_dir(self, source_sha256: str, ocr_recipe: str) -> Path:
        # This instance is already rooted at one document's transforms_dir,
        # so source_sha256 no longer needs to appear in the path — it is
        # still validated and carried in the manifest (see load_ocr) as a
        # confused-deputy guard against a cache instance being reused
        # across documents.
        _source_digest(source_sha256)
        return self.root / f"ocr-{_recipe_digest(ocr_recipe)}"

    def _text_dir(self, source_sha256: str, recipes: PdfRecipes) -> Path:
        return self._ocr_dir(source_sha256, recipes.ocr) / "text" / \
            _recipe_digest(recipes.text)

    def load_ocr(self, source_sha256: str,
                 ocr_recipe: str) -> OcrProduct | None:
        try:
            directory = self._ocr_dir(source_sha256, ocr_recipe)
            self._safe_directory(directory)
            manifest_path = directory / "manifest.json"
            manifest = json.loads(manifest_path.read_text(encoding="utf-8"))
            if set(manifest) != {
                    "schema_version", "source_sha256", "ocr_recipe",
                    "derivative_sha256", "direct_original_fallback",
                    "warning"}:
                return None
            if manifest["schema_version"] != _MANIFEST_VERSION \
                    or manifest["source_sha256"] != source_sha256 \
                    or manifest["ocr_recipe"] != ocr_recipe:
                return None
            warning = manifest["warning"]
            if warning is not None and not isinstance(warning, str):
                return None
            derivative_sha = manifest["derivative_sha256"]
            fallback = manifest["direct_original_fallback"]
            if not isinstance(fallback, bool):
                return None
            derivative = directory / "derivative.pdf"
            if derivative_sha is None:
                if not fallback or derivative.exists():
                    return None
                derivative_path = None
            elif not isinstance(derivative_sha, str) \
                    or fallback or not derivative.is_file() \
                    or derivative.is_symlink() \
                    or sha256_file(derivative) != derivative_sha:
                return None
            else:
                derivative_path = derivative
            return OcrProduct(
                source_sha256=source_sha256, recipe=ocr_recipe,
                derivative_path=derivative_path,
                derivative_sha256=derivative_sha,
                direct_original_fallback=fallback, warning=warning)
        except (CustodyError, OSError, ValueError, TypeError,
                json.JSONDecodeError):
            return None

    def load_text(self, source_sha256: str,
                  recipes: PdfRecipes) -> TextProduct | None:
        try:
            ocr_product = self.load_ocr(source_sha256, recipes.ocr)
            if ocr_product is None:
                return None
            directory = self._text_dir(source_sha256, recipes)
            self._safe_directory(directory)
            manifest_path = directory / "manifest.json"
            text_path = directory / "output.txt"
            manifest = json.loads(manifest_path.read_text(encoding="utf-8"))
            if set(manifest) != {
                    "schema_version", "source_sha256", "ocr_recipe",
                    "text_recipe", "source_artifact_sha256",
                    "text_sha256"}:
                return None
            expected_source = ocr_product.derivative_sha256 or source_sha256
            if manifest["schema_version"] != _MANIFEST_VERSION \
                    or manifest["source_sha256"] != source_sha256 \
                    or manifest["ocr_recipe"] != recipes.ocr \
                    or manifest["text_recipe"] != recipes.text \
                    or manifest["source_artifact_sha256"] != expected_source \
                    or not isinstance(manifest["text_sha256"], str) \
                    or not text_path.is_file() or text_path.is_symlink() \
                    or sha256_file(text_path) != manifest["text_sha256"]:
                return None
            text_path.read_text(encoding="utf-8", errors="replace")
            return TextProduct(
                source_sha256=source_sha256, ocr_recipe=recipes.ocr,
                text_recipe=recipes.text, text_path=text_path,
                text_sha256=manifest["text_sha256"], ocr=ocr_product)
        except (CustodyError, OSError, ValueError, TypeError,
                json.JSONDecodeError):
            return None

    def publish_ocr(self, source_sha256: str, recipe: str,
                    derivative: Path | None, warning: str | None,
                    direct_original_fallback: bool) -> OcrProduct:
        directory = self._ocr_dir(source_sha256, recipe)
        self._safe_directory(directory)
        derivative_target = directory / "derivative.pdf"
        if derivative is None:
            if not direct_original_fallback:
                raise ValueError("missing derivative without original fallback")
            derivative_target.unlink(missing_ok=True)
            derivative_sha = None
            derivative_path = None
        else:
            if direct_original_fallback:
                raise ValueError(
                    "OCR derivative and original fallback are mutually"
                    " exclusive")
            derivative_sha = sha256_file(derivative)
            copy_verified_atomic(derivative, derivative_target, derivative_sha)
            derivative_path = derivative_target
        _atomic_json(directory / "manifest.json", {
            "schema_version": _MANIFEST_VERSION,
            "source_sha256": source_sha256,
            "ocr_recipe": recipe,
            "derivative_sha256": derivative_sha,
            "direct_original_fallback": direct_original_fallback,
            "warning": warning,
        })
        return OcrProduct(
            source_sha256=source_sha256, recipe=recipe,
            derivative_path=derivative_path,
            derivative_sha256=derivative_sha,
            direct_original_fallback=direct_original_fallback,
            warning=warning)

    def publish_text(self, source_sha256: str, recipes: PdfRecipes,
                     ocr_product: OcrProduct,
                     text_temp: Path) -> TextProduct:
        if ocr_product.source_sha256 != source_sha256 \
                or ocr_product.recipe != recipes.ocr:
            raise ValueError(
                "OCR product identity does not match text transform")
        directory = self._text_dir(source_sha256, recipes)
        self._safe_directory(directory)
        text_target = directory / "output.txt"
        text_sha = sha256_file(text_temp)
        copy_verified_atomic(text_temp, text_target, text_sha)
        _atomic_json(directory / "manifest.json", {
            "schema_version": _MANIFEST_VERSION,
            "source_sha256": source_sha256,
            "ocr_recipe": recipes.ocr,
            "text_recipe": recipes.text,
            "source_artifact_sha256": (
                ocr_product.derivative_sha256 or source_sha256),
            "text_sha256": text_sha,
        })
        return TextProduct(
            source_sha256=source_sha256, ocr_recipe=recipes.ocr,
            text_recipe=recipes.text, text_path=text_target,
            text_sha256=text_sha, ocr=ocr_product)
