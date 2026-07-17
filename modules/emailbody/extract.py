"""Lossless MIME body extraction: prefer plain text, fall back to
tag-stripped HTML. Pure: EmailMessage in, text out."""
import re
from dataclasses import dataclass
from email.message import EmailMessage

from bs4 import BeautifulSoup

from modules.emailbody.mime import decode_with_fallbacks


@dataclass(frozen=True, slots=True)
class ExtractedBody:
    text: str
    source: str            # plain | html_stripped | none
    charset: str | None


def _clean_text(text: str) -> str:
    text = text.replace("\r\n", "\n").replace("\r", "\n")
    return re.sub(r"\n{3,}", "\n\n", text).strip()


def _part_text(part: EmailMessage) -> tuple[str, str | None]:
    charset = part.get_content_charset()
    try:
        return part.get_content(), charset
    except (LookupError, UnicodeDecodeError):
        payload = part.get_payload(decode=True) or b""
        return (decode_with_fallbacks(payload, charset),
                f"{charset}(fallback)")


def extract_body(msg: EmailMessage) -> ExtractedBody:
    """Prefer plain MIME text; fall back to tag-stripped HTML."""
    plain = msg.get_body(preferencelist=("plain",))
    if plain is not None:
        raw, charset = _part_text(plain)
        if raw and raw.strip():
            return ExtractedBody(_clean_text(raw), "plain", charset)

    html = msg.get_body(preferencelist=("html",))
    if html is not None:
        raw, charset = _part_text(html)
        if raw:
            soup = BeautifulSoup(raw, "html.parser")
            text = _clean_text(soup.get_text(separator="\n"))
            if text:
                return ExtractedBody(text, "html_stripped", charset)
    return ExtractedBody("", "none", None)
