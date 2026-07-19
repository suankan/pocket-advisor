"""Chain-of-custody primitives: SHA-256 and write-then-verify.

Every derived copy of evidence goes through write_verified(), which
re-reads the bytes from disk and RAISES on mismatch — a corrupt copy
must never be processed further, and making the check an exception
(rather than a return value the caller must remember to compare)
means it cannot be skipped by accident.
"""
import hashlib
import shutil
from pathlib import Path


class CustodyError(RuntimeError):
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

    Raises CustodyError when the re-read hash differs from the
    in-memory hash (disk corruption); the partial file is left in
    place for inspection.
    """
    expected = sha256_bytes(data)
    path.parent.mkdir(parents=True, exist_ok=True)
    with open(path, "wb") as fh:
        fh.write(data)
    actual = sha256_file(path)
    if actual != expected:
        raise CustodyError(
            f"write verification FAILED for {path}: "
            f"expected {expected[:12]}…, disk has {actual[:12]}…")
    return actual


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
        raise CustodyError(
            f"copy source verification FAILED for {source}: expected "
            f"{expected_sha256[:12]}…, disk has {source_sha[:12]}…")
    target.parent.mkdir(parents=True, exist_ok=True)
    with source.open("rb") as src, target.open("wb") as dst:
        shutil.copyfileobj(src, dst, length=1 << 20)
    target_sha = sha256_file(target)
    if target_sha != source_sha:
        raise CustodyError(
            f"copy verification FAILED for {target}: source "
            f"{source_sha[:12]}…, disk has {target_sha[:12]}…")
    return target_sha
