"""Email body handling: MIME decoding, extraction, quoted-reply compaction."""
from modules.emailbody.compaction import (COMPACTION_VERSION,
                                          compact_authored_bodies,
                                          find_quote_start)
from modules.emailbody.extract import ExtractedBody, extract_body
from modules.emailbody.mime import (decode_maybe_encoded,
                                    decode_with_fallbacks,
                                    normalize_message_id, normalize_subject,
                                    sanitize_filename)

__all__ = [
    "COMPACTION_VERSION", "ExtractedBody", "compact_authored_bodies",
    "decode_maybe_encoded", "decode_with_fallbacks", "extract_body",
    "find_quote_start", "normalize_message_id", "normalize_subject",
    "sanitize_filename",
]
