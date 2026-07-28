"""Write-then-verify primitives: SHA-256 integrity checks.

Every derived copy goes through write_verified(), which re-reads the
bytes from disk and RAISES on mismatch — a corrupt copy must never be
processed further, and making the check an exception (rather than a
return value the caller must remember to compare) means it cannot be
skipped by accident.
"""
import hashlib
import os
import shutil
import tempfile
from pathlib import Path


class IntegrityError(RuntimeError):
    """A derived copy failed its write-back hash verification."""


def sha256_bytes(data: bytes) -> str:
    return hashlib.sha256(data).hexdigest()


def sha256_file(path: Path) -> str:
    digest = hashlib.sha256()
    with open(path, "rb") as fh:
        for block in iter(lambda: fh.read(1 << 20), b""):
            digest.update(block)
    return digest.hexdigest()


def write_verified(path: Path, data: bytes) -> str:
    """Write bytes, re-read from disk, verify, return the sha256.

    Raises IntegrityError when the re-read hash differs from the
    in-memory hash (disk corruption); the partial file is left in
    place for inspection.
    """
    expected = sha256_bytes(data)
    path.parent.mkdir(parents=True, exist_ok=True)
    with open(path, "wb") as fh:
        fh.write(data)
    actual = sha256_file(path)
    if actual != expected:
        raise IntegrityError(
            f"write verification FAILED for {path}: "
            f"expected {expected[:12]}\u2026, disk has {actual[:12]}\u2026")
    return actual


def write_verified_shared(path: Path, data: bytes) -> str:
    """write_verified for a content-addressed path several writers may target.

    Attached content deduplicates by SHA-256, so two workers extracting two
    different emails can reach for the same `documents/<sha>/source/` file at
    the same moment. Interleaving their writes would make the write-back check
    fail on bytes that were never wrong, so the payload lands in a private
    temporary beside the target, is verified there, and is then renamed into
    place — `os.replace` is atomic, so the verified bytes are exactly the
    bytes that become visible.

    A target that already holds this content is left untouched: identical
    bytes at a content-addressed path are the expected steady state, not work
    to redo.
    """
    expected = sha256_bytes(data)
    path.parent.mkdir(parents=True, exist_ok=True)
    try:
        if path.stat().st_size == len(data) and sha256_file(path) == expected:
            return expected
    except OSError:
        pass
    handle, raw_temp = tempfile.mkstemp(
        dir=path.parent, prefix=f".{path.name}.", suffix=".part")
    temp = Path(raw_temp)
    try:
        with os.fdopen(handle, "wb") as fh:
            fh.write(data)
        actual = sha256_file(temp)
        if actual != expected:
            raise IntegrityError(
                f"write verification FAILED for {path}: "
                f"expected {expected[:12]}…, disk has {actual[:12]}…")
        os.replace(temp, path)
    finally:
        temp.unlink(missing_ok=True)
    return expected


def copy_verified(source: Path, target: Path,
                  *, expected_sha256: str | None = None) -> str:
    """Stream-copy one derived artifact and verify both source and target.

    Large PDF derivatives must not be materialized as one in-memory bytes
    object. The source is hashed before copying, the target is re-read after
    copying, and an optional expected identity prevents a wrong source from
    being propagated.
    """
    source_sha = sha256_file(source)
    if expected_sha256 is not None and source_sha != expected_sha256:
        raise IntegrityError(
            f"copy source verification FAILED for {source}: expected "
            f"{expected_sha256[:12]}\u2026, disk has {source_sha[:12]}\u2026")
    target.parent.mkdir(parents=True, exist_ok=True)
    with source.open("rb") as src, target.open("wb") as dst:
        shutil.copyfileobj(src, dst, length=1 << 20)
    target_sha = sha256_file(target)
    if target_sha != source_sha:
        raise IntegrityError(
            f"copy verification FAILED for {target}: source "
            f"{source_sha[:12]}\u2026, disk has {target_sha[:12]}\u2026")
    return target_sha
