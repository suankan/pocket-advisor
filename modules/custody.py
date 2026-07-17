"""Chain-of-custody primitives: SHA-256 and write-then-verify.

Every derived copy of evidence goes through write_verified(), which
re-reads the bytes from disk and RAISES on mismatch — a corrupt copy
must never be processed further, and making the check an exception
(rather than a return value the caller must remember to compare)
means it cannot be skipped by accident.
"""
import hashlib
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
