package worker

import (
	"github.com/suankan/pocket-advisor/internal/domain"
	"github.com/suankan/pocket-advisor/internal/engine/email"
)

// Mapping from the parsed header model to the durable browse metadata
// (ingestion-design.md §2.5).
//
// It lives here, in the worker, for the same reason headersOf does: the mail
// parser knows nothing about rows and storage knows nothing about MIME, and
// this is the one place already holding both. Nothing is derived that the
// message did not say — an absent header becomes an empty value, and a defect
// the parser reported travels through as a warning rather than being repaired
// on the way to the database.

// emailMessageOf builds the durable metadata for one parsed message.
func emailMessageOf(docID, workspaceID string, p *email.Parsed) domain.EmailMessage {
	h := p.Headers

	m := domain.EmailMessage{
		DocID:             docID,
		WorkspaceID:       workspaceID,
		MessageID:         h.MessageID,
		SubjectRaw:        p.Subject,
		SubjectNormalized: email.NormalizeSubject(p.Subject),
		AutomatedClass:    automatedClass(h.Automation),
		ListID:            h.Automation.ListID,
	}
	if h.HasDate {
		m.SentAt = h.Date
	}

	for _, f := range []struct {
		kind domain.EmailAddressKind
		list email.AddressList
	}{
		{domain.EmailAddressFrom, h.From},
		{domain.EmailAddressReplyTo, h.ReplyTo},
		{domain.EmailAddressTo, h.To},
		{domain.EmailAddressCc, h.Cc},
		{domain.EmailAddressBcc, h.Bcc},
	} {
		for i, mb := range f.list.Mailboxes {
			// An unparsable mailbox is kept, not dropped: its raw text is
			// evidence of what the header actually said, and its empty address
			// is what makes it unmatchable by an exact filter.
			m.Addresses = append(m.Addresses, domain.EmailAddress{
				Kind:        f.kind,
				Ordinal:     i,
				Address:     mb.Address,
				DisplayName: mb.DisplayName,
				Raw:         mb.Raw,
				Valid:       mb.Valid,
			})
		}
	}

	for i, id := range h.InReplyTo {
		m.References = append(m.References, domain.EmailReference{
			Kind: domain.EmailReferenceInReplyTo, Ordinal: i, MessageID: id,
		})
	}
	for i, id := range h.References {
		m.References = append(m.References, domain.EmailReference{
			Kind: domain.EmailReferenceReferences, Ordinal: i, MessageID: id,
		})
	}

	for _, w := range h.Warnings {
		m.Warnings = append(m.Warnings, domain.EmailParseWarning{
			Code: string(w.Code), Header: w.Header, Value: w.Value,
		})
	}
	return m
}

// automatedClass narrows the parser's classification to the four values a row
// stores. The parser distinguishes more cases than a browse filter needs, so
// bulk traffic folds into the class that explains it: an automatic reply is
// auto-submitted, and anything else marked bulk is mass mail, which is what
// 'list' means to a caller filtering it out.
func automatedClass(a email.Automation) domain.EmailAutomatedClass {
	switch a.Class {
	case email.AutomationDeliveryStatus:
		return domain.EmailAutomatedBounce
	case email.AutomationMailingList:
		return domain.EmailAutomatedList
	case email.AutomationAutoSubmitted:
		return domain.EmailAutomatedAutoSubmitted
	case email.AutomationBulk:
		if a.Precedence == "auto_reply" {
			return domain.EmailAutomatedAutoSubmitted
		}
		return domain.EmailAutomatedList
	}
	return domain.EmailAutomatedNone
}
