package discovery

import (
	"context"
	"strings"

	"github.com/suankan/pocket-advisor/internal/domain"
	"github.com/suankan/pocket-advisor/internal/engine/charset"
	"github.com/suankan/pocket-advisor/internal/storage/postgres"
)

// SaveText writes plain text as a document's canonical text, decoded by
// charset.Decode, and notes on the document how it was decoded when that was
// not plain UTF-8 (§6.3). Text holding NUL is not written but declined, by
// the reason returned: PostgreSQL text cannot hold it, and stripping it would
// make the canonical text differ from its source. Discovery and the email
// worker admit plain text through this one path, so a staged file and an
// attachment read alike.
func SaveText(ctx context.Context, docs *postgres.DocumentRepo, docID string, data []byte, docType string) (string, domain.FailureReason, error) {
	decoded := charset.Decode(data)
	if strings.IndexByte(decoded.Text, 0) >= 0 {
		return "", domain.ReasonNULBytes, nil
	}
	if err := docs.SaveText(ctx, docID, decoded.Text, docType); err != nil {
		return "", "", err
	}
	if note := decoded.Note(); note != "" {
		if err := docs.NoteTextDecoding(ctx, docID, note); err != nil {
			return "", "", err
		}
	}
	return decoded.Text, "", nil
}
