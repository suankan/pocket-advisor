"""Leaf retrieval payloads.

Chunk text remains a pure evidentiary quote. This module derives the separate
envelope-enriched payload used by both the dense embedder and the FTS shadow.
"""
import json


PAYLOAD_RECIPE = "envelope-v1"


def _single_line(value: object | None) -> str:
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


def _email_envelope(row) -> tuple[str, ...]:
    return (
        f"From: {_display_address(row['from_name'], row['from_addr'])}",
        f"Date: {_single_line(row['date_utc'] or row['date_raw'])}",
        f"Subject: {_single_line(row['subject'])}",
        f"To: {_display_address_list(row['to_addrs'])}",
    )


def enriched_payload(row) -> str:
    """Derive the current recipe's payload for one joined chunk row."""
    prefix: list[str] = []
    if row["source_type"] == "document_text":
        prefix.append(f"Document: {_single_line(row['document_name'])}")
    elif row["source_type"] == "email_body":
        prefix.extend(_email_envelope(row))
    else:
        raise ValueError(f"unsupported chunk source_type: {row['source_type']}")
    return "\n".join(prefix) + "\n\n" + row["text"]
