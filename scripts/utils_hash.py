"""SHA-256 helpers with write-then-verify for chain-of-custody handling."""
import hashlib
from pathlib import Path


def sha256_bytes(data: bytes) -> str:
    return hashlib.sha256(data).hexdigest()


def sha256_file(path: Path) -> str:
    h = hashlib.sha256()
    with open(path, "rb") as f:
        for block in iter(lambda: f.read(1 << 20), b""):
            h.update(block)
    return h.hexdigest()


def write_and_verify(path: Path, data: bytes) -> str:
    """Write bytes, re-read from disk, return the re-read hash.

    Caller must compare against the in-memory hash; mismatch means
    disk corruption and the artifact must not be processed further.
    """
    path.parent.mkdir(parents=True, exist_ok=True)
    with open(path, "wb") as f:
        f.write(data)
    return sha256_file(path)
