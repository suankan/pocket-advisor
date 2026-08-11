package worker

import (
	"reflect"
	"testing"

	"github.com/suankan/pocket-advisor/internal/domain"
	"github.com/suankan/pocket-advisor/internal/engine/email"
)

// Mapping from parsed headers to durable browse metadata. Every fixture here
// is synthetic: .test and .invalid can never name a real mailbox.

const threadedMessage = "Message-ID: <c@mail.example.test>\r\n" +
	"In-Reply-To: <b@mail.example.test>\r\n" +
	"References: <a@mail.example.test> <b@mail.example.test>\r\n" +
	"From: Ada Adviser <Ada@Example.test>\r\n" +
	"To: Bob Client <bob@example.test>, carol@example.test\r\n" +
	"Cc: \"Dana, D.\" <dana@example.test>\r\n" +
	"Subject: Re: Fwd: Quarterly review\r\n" +
	"Date: Wed, 07 Jan 2026 08:12:30 +1100\r\n" +
	"Content-Type: text/plain; charset=utf-8\r\n" +
	"\r\n" +
	"Body prose.\r\n"

func parseFixture(t *testing.T, raw string) *email.Parsed {
	t.Helper()
	p, err := email.ParseEmail([]byte(raw))
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	return p
}

func TestEmailMessageOfCarriesIdentifiersAndAddresses(t *testing.T) {
	m := emailMessageOf("doc-1", "workspace-1", parseFixture(t, threadedMessage))

	if m.MessageID != "c@mail.example.test" {
		t.Errorf("message id = %q", m.MessageID)
	}
	if m.SubjectRaw != "Re: Fwd: Quarterly review" {
		t.Errorf("subject_raw = %q", m.SubjectRaw)
	}
	if m.SubjectNormalized != "quarterly review" {
		t.Errorf("subject_normalized = %q", m.SubjectNormalized)
	}
	if m.SentAt.IsZero() {
		t.Error("sent_at not carried")
	}
	if m.AutomatedClass != domain.EmailAutomatedNone {
		t.Errorf("automated_class = %q for ordinary mail", m.AutomatedClass)
	}

	wantRefs := []domain.EmailReference{
		{Kind: domain.EmailReferenceInReplyTo, Ordinal: 0, MessageID: "b@mail.example.test"},
		{Kind: domain.EmailReferenceReferences, Ordinal: 0, MessageID: "a@mail.example.test"},
		{Kind: domain.EmailReferenceReferences, Ordinal: 1, MessageID: "b@mail.example.test"},
	}
	if !reflect.DeepEqual(m.References, wantRefs) {
		t.Errorf("references = %+v, want %+v", m.References, wantRefs)
	}

	// Normalized for matching, display name and raw kept for evidence, ordinal
	// preserving header order.
	wantAddrs := []domain.EmailAddress{
		{Kind: domain.EmailAddressFrom, Ordinal: 0, Address: "ada@example.test",
			DisplayName: "Ada Adviser", Raw: "Ada Adviser <Ada@Example.test>", Valid: true},
		{Kind: domain.EmailAddressTo, Ordinal: 0, Address: "bob@example.test",
			DisplayName: "Bob Client", Raw: "Bob Client <bob@example.test>", Valid: true},
		{Kind: domain.EmailAddressTo, Ordinal: 1, Address: "carol@example.test",
			Raw: "carol@example.test", Valid: true},
		{Kind: domain.EmailAddressCc, Ordinal: 0, Address: "dana@example.test",
			DisplayName: "Dana, D.", Raw: `"Dana, D." <dana@example.test>`, Valid: true},
	}
	if !reflect.DeepEqual(m.Addresses, wantAddrs) {
		t.Errorf("addresses = %+v, want %+v", m.Addresses, wantAddrs)
	}
}

// The identifier set is what folds a message into a conversation: its own id
// first, then its reply headers, each contributing once however often it was
// repeated across them.
func TestIdentifierSetIsDeduplicatedAndOrdered(t *testing.T) {
	m := emailMessageOf("doc-1", "workspace-1", parseFixture(t, threadedMessage))

	want := []string{"c@mail.example.test", "b@mail.example.test", "a@mail.example.test"}
	if got := m.Identifiers(); !reflect.DeepEqual(got, want) {
		t.Errorf("identifiers = %v, want %v", got, want)
	}
}

// A malformed mailbox is stored, not dropped: the raw text is evidence, and an
// empty address is what keeps it out of exact filters.
func TestEmailMessageOfKeepsMalformedAddresses(t *testing.T) {
	m := emailMessageOf("doc-1", "workspace-1", parseFixture(t,
		"From: not-an-address\r\nTo: bob@example.test\r\nSubject: Hello\r\n\r\nBody.\r\n"))

	var from domain.EmailAddress
	for _, a := range m.Addresses {
		if a.Kind == domain.EmailAddressFrom {
			from = a
		}
	}
	if from.Valid || from.Address != "" {
		t.Errorf("malformed sender parsed as %+v", from)
	}
	if from.Raw != "not-an-address" {
		t.Errorf("raw sender = %q, want it kept verbatim", from.Raw)
	}
	if !hasWarning(m.Warnings, string(email.WarnMalformedAddress)) {
		t.Errorf("warnings = %+v, want a malformed address warning", m.Warnings)
	}
}

// Missing headers are ordinary input, reported rather than repaired.
func TestEmailMessageOfReportsMissingIdentifierAndDate(t *testing.T) {
	m := emailMessageOf("doc-1", "workspace-1", parseFixture(t,
		"From: bob@example.test\r\nSubject: No id, no date\r\n\r\nBody.\r\n"))

	if m.MessageID != "" {
		t.Errorf("message id = %q, want none synthesised", m.MessageID)
	}
	if !m.SentAt.IsZero() {
		t.Errorf("sent_at = %v, want zero", m.SentAt)
	}
	if len(m.Identifiers()) != 0 {
		t.Errorf("identifiers = %v, want none", m.Identifiers())
	}
	for _, code := range []string{string(email.WarnMissingMessageID), string(email.WarnMissingDate)} {
		if !hasWarning(m.Warnings, code) {
			t.Errorf("warnings = %+v, want %s", m.Warnings, code)
		}
	}
}

func TestAutomatedClassNarrowsToStoredValues(t *testing.T) {
	cases := []struct {
		name string
		raw  string
		want domain.EmailAutomatedClass
		list string
	}{
		{
			name: "mailing list",
			raw: "From: list@example.test\r\nList-Id: Advice <advice.lists.example.test>\r\n" +
				"Subject: Digest\r\n\r\nBody.\r\n",
			want: domain.EmailAutomatedList,
			list: "advice.lists.example.test",
		},
		{
			name: "auto submitted",
			raw:  "From: robot@example.test\r\nAuto-Submitted: auto-replied\r\nSubject: Out of office\r\n\r\nBody.\r\n",
			want: domain.EmailAutomatedAutoSubmitted,
		},
		{
			name: "bounce",
			raw: "From: MAILER-DAEMON@example.test\r\nSubject: Undelivered mail\r\n" +
				"Content-Type: multipart/report; report-type=delivery-status; boundary=b\r\n\r\nBody.\r\n",
			want: domain.EmailAutomatedBounce,
		},
		{
			name: "bulk precedence is mass mail",
			raw:  "From: news@example.test\r\nPrecedence: bulk\r\nSubject: Newsletter\r\n\r\nBody.\r\n",
			want: domain.EmailAutomatedList,
		},
		{
			name: "auto reply precedence",
			raw:  "From: robot@example.test\r\nPrecedence: auto_reply\r\nSubject: Ack\r\n\r\nBody.\r\n",
			want: domain.EmailAutomatedAutoSubmitted,
		},
		{
			name: "human",
			raw:  "From: bob@example.test\r\nSubject: Hello\r\n\r\nBody.\r\n",
			want: domain.EmailAutomatedNone,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			m := emailMessageOf("doc-1", "workspace-1", parseFixture(t, tc.raw))
			if m.AutomatedClass != tc.want {
				t.Errorf("automated_class = %q, want %q", m.AutomatedClass, tc.want)
			}
			if m.ListID != tc.list {
				t.Errorf("list_id = %q, want %q", m.ListID, tc.list)
			}
		})
	}
}

func hasWarning(warnings []domain.EmailParseWarning, code string) bool {
	for _, w := range warnings {
		if w.Code == code {
			return true
		}
	}
	return false
}
