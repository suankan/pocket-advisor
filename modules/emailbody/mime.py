"""MIME decoding helpers: RFC 2047 headers/filenames, charset fallbacks,
filesystem-safe names.

Parsing must always use email.policy.default (the modern EmailPolicy):
it transparently decodes RFC 2047 encoded-words in headers and
filenames, including mixed-charset multi-token values, which the legacy
compat32 policy does not.
"""
import re
from email.header import decode_header

from modules.config import CHARSET_FALLBACKS

_ENCODED_WORD = re.compile(r"=\?[^?]+\?[BQbq]\?[^?]*\?=")

# Strip leading Re:/Fwd:/FW: repeats (incl. Russian mail client variants)
_REPLY_PREFIX = re.compile(
    r"^\s*((re|fwd?|fw|отв|пересл)\s*(\[\d+\])?\s*:\s*)+", re.IGNORECASE)

_UNSAFE_FS = re.compile(r"[^\w.\-]+", re.UNICODE)


def decode_maybe_encoded(value: str | None) -> str | None:
    """Defensive fallback: if a header value still contains raw =?..?=
    tokens after policy.default parsing, decode them manually."""
    if not value or not _ENCODED_WORD.search(value):
        return value
    parts: list[str] = []
    for data, charset in decode_header(value):
        if isinstance(data, bytes):
            parts.append(decode_with_fallbacks(data, charset))
        else:
            parts.append(data)
    return "".join(parts)


def decode_with_fallbacks(data: bytes,
                          declared_charset: str | None = None) -> str:
    """Decode bytes trying the declared charset then the fallback chain
    (cp1251 matters: Cyrillic corpus). latin-1 at the end cannot fail."""
    charsets: list[str] = []
    if declared_charset:
        charsets.append(declared_charset)
    charsets.extend(c for c in CHARSET_FALLBACKS if c not in charsets)
    for charset in charsets:
        try:
            return data.decode(charset)
        except (UnicodeDecodeError, LookupError):
            continue
    return data.decode("latin-1", errors="replace")


def normalize_subject(subject: str | None) -> str:
    if not subject:
        return ""
    return _REPLY_PREFIX.sub("", subject).strip().lower()


def sanitize_filename(name: str | None, max_len: int = 120) -> str:
    """Filesystem-safe version of a decoded attachment filename.
    Preserves Cyrillic (\\w matches it with re.UNICODE)."""
    if not name:
        return "unnamed"
    name = name.strip().replace("/", "_").replace("\x00", "")
    name = _UNSAFE_FS.sub("_", name)
    if len(name) > max_len:
        stem, dot, ext = name.rpartition(".")
        if dot and len(ext) <= 10:
            name = stem[: max_len - len(ext) - 1] + "." + ext
        else:
            name = name[:max_len]
    return name or "unnamed"


def normalize_message_id(mid: str | None) -> str | None:
    """Trim whitespace; keep angle brackets for storage consistency."""
    if not mid:
        return None
    mid = mid.strip()
    if not mid.startswith("<"):
        mid = f"<{mid}>"
    return mid
