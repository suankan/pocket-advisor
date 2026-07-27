"""Deterministic readable email artifacts.

Both message files carry the same decoded five-field envelope.  The bytes
after the first blank-line separator are authoritative body bytes: lossless
in ``email_message_full.txt`` and authored-only in ``email_message.txt``.
"""
import json


SEPARATOR = b"\n\n"


def _single_line(value: object | None) -> str:
    """Render one decoded header value without allowing line injection."""
    return " ".join(str(value or "").split())


def _display_address(name: object | None, addr: object | None) -> str:
    clean_name = _single_line(name)
    clean_addr = _single_line(addr)
    if clean_name and clean_addr:
        return f"{clean_name} <{clean_addr}>"
    return clean_addr or clean_name


def _display_address_list(raw: str | None) -> str:
    addresses = json.loads(raw or "[]")
    return ", ".join(
        rendered for entry in addresses
        if (rendered := _display_address(entry.get("name"),
                                         entry.get("addr"))))


def render_message(row, body: bytes) -> bytes:
    """Return the stable readable envelope followed by exact body bytes."""
    headers = (
        f"Date: {_single_line(row['date_raw'] or row['date_utc'])}",
        f"From: {_display_address(row['from_name'], row['from_addr'])}",
        f"To: {_display_address_list(row['to_addrs'])}",
        f"Cc: {_display_address_list(row['cc_addrs'])}",
        f"Subject: {_single_line(row['subject'])}",
    )
    return ("\n".join(headers) + "\n\n").encode("utf-8") + body


def body_bytes(rendered: bytes, *, source: object = "message") -> bytes:
    """Extract the body region from a rendered message artifact."""
    _, separator, body = rendered.partition(SEPARATOR)
    if not separator:
        raise ValueError(f"message artifact has no envelope separator: {source}")
    return body


def body_text(rendered: bytes, *, source: object = "message") -> str:
    return body_bytes(rendered, source=source).decode("utf-8")
