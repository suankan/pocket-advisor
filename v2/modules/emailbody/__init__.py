"""Email body handling: MIME decoding, extraction, quoted-reply compaction."""
from v2.modules.emailbody.artifacts import (body_bytes, body_text,
                                         render_message)
from v2.modules.emailbody.compaction import (COMPACTION_VERSION,
                                          CompactionResult,
                                          compact_authored_bodies,
                                          find_quote_start)
from v2.modules.emailbody.extract import ExtractedBody, extract_body
from v2.modules.emailbody.mime import (decode_maybe_encoded,
                                    decode_with_fallbacks,
                                    normalize_message_id, normalize_subject,
                                    sanitize_filename)

__all__ = [
    "COMPACTION_VERSION", "CompactionResult", "ExtractedBody", "body_bytes",
    "body_text", "compact_authored_bodies", "decode_maybe_encoded",
    "decode_with_fallbacks", "extract_body", "find_quote_start",
    "normalize_message_id", "normalize_subject", "render_message",
    "sanitize_filename",
]
