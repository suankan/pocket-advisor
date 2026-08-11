package email

import (
	"net/mail"
	"strings"
	"testing"
)

// parseHeadersOf reads a synthetic header block the same way ParseEmail does,
// so every case here exercises the real folding and unfolding behaviour of
// net/mail rather than a hand-built header map.
func parseHeadersOf(t *testing.T, headerBlock string) Headers {
	t.Helper()
	msg, err := mail.ReadMessage(strings.NewReader(headerBlock + "\r\n"))
	if err != nil {
		t.Fatalf("ReadMessage: %v", err)
	}
	return ParseHeaders(msg.Header)
}

func warningValues(h Headers, code WarningCode) []string {
	var out []string
	for _, w := range h.Warnings {
		if w.Code == code {
			out = append(out, w.Value)
		}
	}
	return out
}

func addressesOf(t *testing.T, list AddressList) []string {
	t.Helper()
	return list.Addresses()
}

// TestParseHeadersCanonicalIdentifiers is the core of the reply model: the
// join keys must come out without angle brackets, in header order, with the
// first occurrence of a repeated identifier kept.
func TestParseHeadersCanonicalIdentifiers(t *testing.T) {
	h := parseHeadersOf(t, strings.Join([]string{
		"Message-ID: <child.2@mail.example.com>",
		"In-Reply-To: <parent.1@mail.example.com>",
		"References: <root.0@mail.example.com> <parent.1@mail.example.com>",
		"Date: Mon, 20 Apr 2026 09:00:00 +1000",
		"From: Alice <alice@example.com>",
		"To: bob@example.com",
	}, "\r\n")+"\r\n")

	if h.MessageID != "child.2@mail.example.com" {
		t.Errorf("MessageID = %q", h.MessageID)
	}
	if h.RawMessageID != "<child.2@mail.example.com>" {
		t.Errorf("RawMessageID = %q", h.RawMessageID)
	}
	if got := strings.Join(h.InReplyTo, ","); got != "parent.1@mail.example.com" {
		t.Errorf("InReplyTo = %q", got)
	}
	want := "root.0@mail.example.com,parent.1@mail.example.com"
	if got := strings.Join(h.References, ","); got != want {
		t.Errorf("References = %q, want %q", got, want)
	}
	if len(h.Warnings) != 0 {
		t.Errorf("unexpected warnings: %+v", h.Warnings)
	}
	if h.Automation.Class != AutomationHuman || h.Automation.Automated() {
		t.Errorf("clean message classified as %q", h.Automation.Class)
	}
}

// TestParseHeadersFoldedAndRunTogetherReferences covers the two shapes real
// mail clients emit: a References header folded across several lines, and
// identifiers written with no separator between the brackets.
func TestParseHeadersFoldedAndRunTogetherReferences(t *testing.T) {
	h := parseHeadersOf(t, "Message-ID:\r\n <folded.msg@example.com>\r\n"+
		"References: <a.1@example.com>\r\n"+
		"\t<b.2@example.com>,\r\n"+
		" <c.3@example.com><d.4@example.com>\r\n"+
		"Date: Mon, 20 Apr 2026 09:00:00 +1000\r\n")

	if h.MessageID != "folded.msg@example.com" {
		t.Errorf("folded Message-ID = %q", h.MessageID)
	}
	want := "a.1@example.com,b.2@example.com,c.3@example.com,d.4@example.com"
	if got := strings.Join(h.References, ","); got != want {
		t.Errorf("References = %q, want %q", got, want)
	}
	if h.HasWarning(WarnMalformedIdentifier) {
		t.Errorf("folded header produced malformed warnings: %+v", h.Warnings)
	}
}

// TestParseHeadersBracketlessIdentifier accepts an identifier written without
// angle brackets — it is unambiguous and common in older archives.
func TestParseHeadersBracketlessIdentifier(t *testing.T) {
	h := parseHeadersOf(t, "Message-ID: bare.id@example.com\r\n"+
		"In-Reply-To: parent.id@example.com\r\n")

	if h.MessageID != "bare.id@example.com" {
		t.Errorf("MessageID = %q", h.MessageID)
	}
	if len(h.InReplyTo) != 1 || h.InReplyTo[0] != "parent.id@example.com" {
		t.Errorf("InReplyTo = %v", h.InReplyTo)
	}
}

func TestParseHeadersMissingMessageID(t *testing.T) {
	h := parseHeadersOf(t, "From: alice@example.com\r\nDate: Mon, 20 Apr 2026 09:00:00 +1000\r\n")

	if h.MessageID != "" {
		t.Errorf("fabricated MessageID %q", h.MessageID)
	}
	if !h.HasWarning(WarnMissingMessageID) {
		t.Errorf("no missing_message_id warning: %+v", h.Warnings)
	}
}

// TestParseHeadersDuplicateMessageID: a repeated header and a header carrying
// two identifiers are both ambiguous. The first identifier is kept and the
// ambiguity is reported rather than resolved arbitrarily.
func TestParseHeadersDuplicateMessageID(t *testing.T) {
	repeated := parseHeadersOf(t, "Message-ID: <first@example.com>\r\n"+
		"Message-ID: <second@example.com>\r\n")
	if repeated.MessageID != "first@example.com" {
		t.Errorf("MessageID = %q, want the first header", repeated.MessageID)
	}
	if got := warningValues(repeated, WarnDuplicateIdentifier); len(got) != 1 ||
		got[0] != "<second@example.com>" {
		t.Errorf("duplicate warnings = %v", got)
	}

	inline := parseHeadersOf(t, "Message-ID: <first@example.com> <extra@example.com>\r\n")
	if inline.MessageID != "first@example.com" {
		t.Errorf("inline MessageID = %q", inline.MessageID)
	}
	if !inline.HasWarning(WarnDuplicateIdentifier) {
		t.Errorf("no duplicate warning for two ids in one header: %+v", inline.Warnings)
	}
}

func TestParseHeadersDuplicateReferences(t *testing.T) {
	h := parseHeadersOf(t, "References: <a@example.com> <b@example.com> <a@example.com>\r\n"+
		"In-Reply-To: <b@example.com> <b@example.com>\r\n")

	if got := strings.Join(h.References, ","); got != "a@example.com,b@example.com" {
		t.Errorf("References = %q, want first-occurrence order without repeats", got)
	}
	if got := strings.Join(h.InReplyTo, ","); got != "b@example.com" {
		t.Errorf("InReplyTo = %q", got)
	}
	if got := warningValues(h, WarnDuplicateIdentifier); len(got) != 2 {
		t.Errorf("duplicate warnings = %v, want one per repeated header", got)
	}
	// An identifier appearing in both In-Reply-To and References is normal
	// threading, not a duplicate within one header.
	if len(h.References) == 0 || len(h.InReplyTo) == 0 {
		t.Fatalf("lost identifiers: %+v", h)
	}
}

// TestParseHeadersMalformedIdentifiers checks that unusable tokens are dropped
// with a warning instead of becoming join keys.
func TestParseHeadersMalformedIdentifiers(t *testing.T) {
	h := parseHeadersOf(t, "Message-ID: <no-at-sign>\r\n"+
		"References: <good@example.com> <unterminated@example.com <@example.com> <>\r\n")

	if h.MessageID != "" {
		t.Errorf("malformed Message-ID accepted as %q", h.MessageID)
	}
	if got := warningValues(h, WarnMalformedIdentifier); len(got) != 4 {
		t.Errorf("malformed warnings = %v, want one per bad token", got)
	}
	if got := strings.Join(h.References, ","); got != "good@example.com" {
		t.Errorf("References = %q, want only the usable identifier", got)
	}
}

func TestCanonicalIdentifier(t *testing.T) {
	cases := []struct {
		in    string
		want  string
		valid bool
	}{
		{"<a@b.example>", "a@b.example", true},
		{"  <a@b.example>  ", "a@b.example", true},
		{"a@b.example", "a@b.example", true},
		{"<a b@example.com>", "", false},
		{"<no-at>", "", false},
		{"<@example.com>", "", false},
		{"<a@>", "", false},
		{"", "", false},
		{"<>", "", false},
	}
	for _, c := range cases {
		got, ok := CanonicalIdentifier(c.in)
		if got != c.want || ok != c.valid {
			t.Errorf("CanonicalIdentifier(%q) = %q,%v want %q,%v", c.in, got, ok, c.want, c.valid)
		}
	}
}

// TestParseHeadersAddressForms covers the display forms that must survive:
// bare address, angle-addr with a display name, quoted display name holding a
// comma, and mixed case that has to normalize to one identity.
func TestParseHeadersAddressForms(t *testing.T) {
	h := parseHeadersOf(t, "From: \"Doe, Jane\" <Jane.Doe@Example.COM>\r\n"+
		"Reply-To: replies@example.com\r\n"+
		"To: bob@example.com, Carol <CAROL@example.com>\r\n"+
		"Cc:  Dave  <dave@example.com>  \r\n"+
		"Bcc: <erin@example.com>\r\n")

	if got := addressesOf(t, h.From); len(got) != 1 || got[0] != "jane.doe@example.com" {
		t.Errorf("From = %v", got)
	}
	if h.From.Mailboxes[0].DisplayName != "Doe, Jane" {
		t.Errorf("display name = %q", h.From.Mailboxes[0].DisplayName)
	}
	if h.From.Mailboxes[0].Raw != "\"Doe, Jane\" <Jane.Doe@Example.COM>" {
		t.Errorf("raw mailbox = %q", h.From.Mailboxes[0].Raw)
	}
	if h.From.Header != "\"Doe, Jane\" <Jane.Doe@Example.COM>" {
		t.Errorf("From header value not preserved: %q", h.From.Header)
	}
	if got := strings.Join(addressesOf(t, h.To), ","); got != "bob@example.com,carol@example.com" {
		t.Errorf("To = %q, want header order preserved", got)
	}
	if got := addressesOf(t, h.ReplyTo); len(got) != 1 || got[0] != "replies@example.com" {
		t.Errorf("Reply-To = %v", got)
	}
	if got := addressesOf(t, h.Cc); len(got) != 1 || got[0] != "dave@example.com" {
		t.Errorf("Cc = %v", got)
	}
	if got := addressesOf(t, h.Bcc); len(got) != 1 || got[0] != "erin@example.com" {
		t.Errorf("Bcc = %v", got)
	}
	if h.HasWarning(WarnMalformedAddress) {
		t.Errorf("valid addresses produced warnings: %+v", h.Warnings)
	}
}

// TestParseHeadersEncodedDisplayName: an encoded-word display name is decoded
// for display, while the raw header form is retained for evidence.
func TestParseHeadersEncodedDisplayName(t *testing.T) {
	h := parseHeadersOf(t, "From: =?utf-8?q?Jos=C3=A9_Garc=C3=ADa?= <jose@example.com>\r\n"+
		"To: =?iso-8859-1?Q?J=FCrgen?= <jurgen@example.com>\r\n")

	if got := h.From.Mailboxes[0].DisplayName; got != "José García" {
		t.Errorf("From display name = %q", got)
	}
	if got := h.To.Mailboxes[0].DisplayName; got != "Jürgen" {
		t.Errorf("To display name = %q", got)
	}
	if !strings.Contains(h.From.Header, "=?utf-8?q?") {
		t.Errorf("raw From header was rewritten: %q", h.From.Header)
	}
	if h.From.Mailboxes[0].Address != "jose@example.com" {
		t.Errorf("From address = %q", h.From.Mailboxes[0].Address)
	}
}

// TestParseHeadersGroupAddresses: group members are mailboxes, the group name
// is not, and an empty group contributes nothing.
func TestParseHeadersGroupAddresses(t *testing.T) {
	h := parseHeadersOf(t, "To: Project Team: alice@example.com, Bob <bob@example.com>;, carol@example.com\r\n"+
		"Cc: undisclosed-recipients:;\r\n")

	want := "alice@example.com,bob@example.com,carol@example.com"
	if got := strings.Join(addressesOf(t, h.To), ","); got != want {
		t.Errorf("To = %q, want %q", got, want)
	}
	if len(h.Cc.Mailboxes) != 0 {
		t.Errorf("empty group produced mailboxes: %+v", h.Cc.Mailboxes)
	}
	if h.HasWarning(WarnMalformedAddress) {
		t.Errorf("group syntax produced malformed warnings: %+v", h.Warnings)
	}
}

// TestParseHeadersMalformedAddress: one bad mailbox must not discard the good
// ones, and the bad one keeps its raw text without an invented address.
func TestParseHeadersMalformedAddress(t *testing.T) {
	h := parseHeadersOf(t, "From: not an address\r\n"+
		"To: good@example.com, Jane <jane@example.com, broken@\r\n")

	if len(h.From.Mailboxes) != 1 || h.From.Mailboxes[0].Valid {
		t.Fatalf("From mailboxes = %+v", h.From.Mailboxes)
	}
	if h.From.Mailboxes[0].Address != "" {
		t.Errorf("fabricated address %q for malformed From", h.From.Mailboxes[0].Address)
	}
	if h.From.Mailboxes[0].Raw != "not an address" {
		t.Errorf("raw form lost: %q", h.From.Mailboxes[0].Raw)
	}
	if got := addressesOf(t, h.To); len(got) != 1 || got[0] != "good@example.com" {
		t.Errorf("valid recipient lost: %v", got)
	}
	// An unclosed angle-addr swallows the separators that follow it, so the
	// damaged tail of the To header is reported as one malformed mailbox.
	got := warningValues(h, WarnMalformedAddress)
	if len(got) != 2 || got[0] != "not an address" ||
		!strings.HasPrefix(got[1], "Jane <jane@example.com") {
		t.Errorf("malformed address warnings = %v", got)
	}
	for _, w := range h.Warnings {
		if w.Code == WarnMalformedAddress && w.Header != "From" && w.Header != "To" {
			t.Errorf("warning attributed to %q", w.Header)
		}
	}
}

func TestParseHeadersDates(t *testing.T) {
	ok := parseHeadersOf(t, "Date: Mon, 20 Apr 2026 09:00:00 +1000\r\n")
	if !ok.HasDate || ok.Date.IsZero() {
		t.Fatalf("parsable date not kept: %+v", ok)
	}
	if ok.Date.UTC().Format("2006-01-02T15:04:05Z") != "2026-04-19T23:00:00Z" {
		t.Errorf("Date = %s", ok.Date.UTC())
	}

	missing := parseHeadersOf(t, "From: alice@example.com\r\n")
	if missing.HasDate || !missing.Date.IsZero() {
		t.Errorf("absent date not distinguished: %+v", missing)
	}
	if !missing.HasWarning(WarnMissingDate) {
		t.Errorf("no missing_date warning: %+v", missing.Warnings)
	}

	bad := parseHeadersOf(t, "Date: yesterday afternoon\r\n")
	if bad.HasDate {
		t.Errorf("unparsable date accepted: %+v", bad)
	}
	if got := warningValues(bad, WarnUnparsableDate); len(got) != 1 || got[0] != "yesterday afternoon" {
		t.Errorf("unparsable date warnings = %v", got)
	}
	if bad.HasWarning(WarnMissingDate) {
		t.Errorf("present-but-unparsable date reported as missing: %+v", bad.Warnings)
	}
}

func TestClassifyMailingList(t *testing.T) {
	h := parseHeadersOf(t, "From: Council Updates <updates@lists.example.org>\r\n"+
		"List-Id: Council Updates <council.lists.example.org>\r\n"+
		"List-Unsubscribe: <mailto:unsubscribe@lists.example.org>\r\n"+
		"Precedence: list\r\n")

	if h.Automation.Class != AutomationMailingList {
		t.Errorf("class = %q", h.Automation.Class)
	}
	if h.Automation.ListID != "council.lists.example.org" {
		t.Errorf("ListID = %q", h.Automation.ListID)
	}
	if !containsSignal(h.Automation.Signals, SignalListID) ||
		!containsSignal(h.Automation.Signals, SignalListUnsubscribe) ||
		!containsSignal(h.Automation.Signals, SignalPrecedence) {
		t.Errorf("signals = %v", h.Automation.Signals)
	}
	if h.Automation.Precedence != "list" {
		t.Errorf("Precedence = %q", h.Automation.Precedence)
	}
}

func TestClassifyAutoSubmitted(t *testing.T) {
	auto := parseHeadersOf(t, "From: noreply@example.com\r\nAuto-Submitted: auto-replied\r\n")
	if auto.Automation.Class != AutomationAutoSubmitted || !auto.Automation.Automated() {
		t.Errorf("class = %q", auto.Automation.Class)
	}
	if auto.Automation.AutoSubmitted != "auto-replied" {
		t.Errorf("AutoSubmitted = %q", auto.Automation.AutoSubmitted)
	}

	// RFC 3834 "no" is an explicit statement that a human wrote it.
	human := parseHeadersOf(t, "From: alice@example.com\r\nAuto-Submitted: no\r\n")
	if human.Automation.Class != AutomationHuman || human.Automation.Automated() {
		t.Errorf("Auto-Submitted: no classified as %q", human.Automation.Class)
	}
	if len(human.Automation.Signals) != 0 {
		t.Errorf("signals = %v", human.Automation.Signals)
	}
}

func TestClassifyBulk(t *testing.T) {
	h := parseHeadersOf(t, "From: marketing@example.com\r\nPrecedence: bulk\r\n")
	if h.Automation.Class != AutomationBulk {
		t.Errorf("class = %q", h.Automation.Class)
	}
}

func TestClassifyDeliveryStatusReports(t *testing.T) {
	report := parseHeadersOf(t, "From: Mail Delivery Subsystem <MAILER-DAEMON@mx.example.com>\r\n"+
		"Content-Type: multipart/report; report-type=delivery-status; boundary=\"b\"\r\n"+
		"Return-Path: <>\r\n"+
		"Precedence: bulk\r\n")

	if report.Automation.Class != AutomationDeliveryStatus {
		t.Errorf("class = %q, want a bounce to outrank the bulk marker", report.Automation.Class)
	}
	for _, want := range []AutomationSignal{
		SignalDeliveryStatusReport, SignalMailerDaemonSender, SignalNullReturnPath,
	} {
		if !containsSignal(report.Automation.Signals, want) {
			t.Errorf("missing signal %q in %v", want, report.Automation.Signals)
		}
	}

	daemon := parseHeadersOf(t, "From: postmaster@mx.example.com\r\n")
	if daemon.Automation.Class != AutomationDeliveryStatus {
		t.Errorf("postmaster sender classified as %q", daemon.Automation.Class)
	}
}

// TestClassifyOrdinaryMailStaysHuman guards against over-eager rules: an
// ordinary reply with no automation headers must never be filtered out.
func TestClassifyOrdinaryMailStaysHuman(t *testing.T) {
	h := parseHeadersOf(t, "From: Alice <alice@example.com>\r\n"+
		"To: bob@example.com\r\n"+
		"Subject: Re: quote\r\n"+
		"Content-Type: multipart/mixed; boundary=\"b\"\r\n"+
		"Date: Mon, 20 Apr 2026 09:00:00 +1000\r\n")

	if h.Automation.Class != AutomationHuman || len(h.Automation.Signals) != 0 {
		t.Errorf("ordinary mail classified as %q %v", h.Automation.Class, h.Automation.Signals)
	}
}

func TestNormalizeAddress(t *testing.T) {
	for in, want := range map[string]string{
		"  <Alice@Example.COM> ": "alice@example.com",
		"BOB@example.com":        "bob@example.com",
		"":                       "",
	} {
		if got := NormalizeAddress(in); got != want {
			t.Errorf("NormalizeAddress(%q) = %q, want %q", in, got, want)
		}
	}
}

// TestParseEmailPopulatesHeaders confirms the model is attached by the normal
// parse path and that the pre-existing flat fields are unchanged.
func TestParseEmailPopulatesHeaders(t *testing.T) {
	raw := "From: Alice <Alice@Example.com>\r\n" +
		"To: Bob <bob@example.com>, carol@example.com\r\n" +
		"Subject: Re: Occupancy certificate\r\n" +
		"Message-ID: <reply.9@mail.example.com>\r\n" +
		"In-Reply-To: <root.1@mail.example.com>\r\n" +
		"References: <root.1@mail.example.com>\r\n" +
		"Date: Sun, 19 Apr 2026 09:52:00 +1000\r\n" +
		"Content-Type: text/plain; charset=\"utf-8\"\r\n" +
		"\r\n" +
		"Thanks, that works.\r\n"

	p, err := ParseEmail([]byte(raw))
	if err != nil {
		t.Fatalf("ParseEmail: %v", err)
	}

	if p.MessageID != "reply.9@mail.example.com" || p.InReplyTo != "root.1@mail.example.com" {
		t.Errorf("flat identifier fields changed: %q %q", p.MessageID, p.InReplyTo)
	}
	if p.From != "Alice <Alice@Example.com>" || p.To != "Bob <bob@example.com>, carol@example.com" {
		t.Errorf("flat display fields changed: %q %q", p.From, p.To)
	}
	if p.Headers.MessageID != "reply.9@mail.example.com" {
		t.Errorf("Headers.MessageID = %q", p.Headers.MessageID)
	}
	if got := strings.Join(p.Headers.From.Addresses(), ","); got != "alice@example.com" {
		t.Errorf("Headers.From = %q", got)
	}
	if got := strings.Join(p.Headers.To.Addresses(), ","); got != "bob@example.com,carol@example.com" {
		t.Errorf("Headers.To = %q", got)
	}
	if !p.Headers.HasDate || !p.Headers.Date.Equal(p.Date) {
		t.Errorf("Headers.Date %v disagrees with Parsed.Date %v", p.Headers.Date, p.Date)
	}
	if len(p.Headers.Warnings) != 0 {
		t.Errorf("clean message warned: %+v", p.Headers.Warnings)
	}
}

// TestParseEmailWithoutDateKeepsZeroValue: a message with no Date must stay
// distinguishable from one dated at the zero time.
func TestParseEmailWithoutDateKeepsZeroValue(t *testing.T) {
	raw := "From: alice@example.com\r\n" +
		"To: bob@example.com\r\n" +
		"Subject: No date\r\n" +
		"\r\n" +
		"Body.\r\n"

	p, err := ParseEmail([]byte(raw))
	if err != nil {
		t.Fatalf("ParseEmail: %v", err)
	}
	if !p.Date.IsZero() || p.Headers.HasDate {
		t.Errorf("absent date not distinguished: %v %v", p.Date, p.Headers.HasDate)
	}
	if !p.Headers.HasWarning(WarnMissingDate) || !p.Headers.HasWarning(WarnMissingMessageID) {
		t.Errorf("warnings = %+v", p.Headers.Warnings)
	}
}

// TestParseHeadersIsDeterministic: the same bytes must produce the same
// warnings in the same order every time, because they are persisted.
func TestParseHeadersIsDeterministic(t *testing.T) {
	block := "Message-ID: <bad-id>\r\n" +
		"References: <a@example.com> <a@example.com> <broken>\r\n" +
		"From: nobody\r\n" +
		"Date: not a date\r\n"

	first := parseHeadersOf(t, block)
	for i := 0; i < 5; i++ {
		again := parseHeadersOf(t, block)
		if len(again.Warnings) != len(first.Warnings) {
			t.Fatalf("warning count varies: %d vs %d", len(again.Warnings), len(first.Warnings))
		}
		for j := range first.Warnings {
			if again.Warnings[j] != first.Warnings[j] {
				t.Fatalf("warning %d varies: %+v vs %+v", j, again.Warnings[j], first.Warnings[j])
			}
		}
	}
	// Identifier defects are reported before address and date defects.
	if first.Warnings[0].Code != WarnMalformedIdentifier ||
		first.Warnings[len(first.Warnings)-1].Code != WarnUnparsableDate {
		t.Errorf("warning order = %+v", first.Warnings)
	}
}

func containsSignal(signals []AutomationSignal, want AutomationSignal) bool {
	for _, s := range signals {
		if s == want {
			return true
		}
	}
	return false
}
