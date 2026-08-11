// Header parsing for the email browse and conversation model: canonical
// message identifiers, normalized mailbox identities, bounded automated-traffic
// classification, and typed parse warnings (ingestion-design.md §4.1).
//
// Nothing here fabricates an identifier or infers a participant. A header that
// is missing, folded oddly, duplicated or malformed is an ordinary input
// condition: the salvageable part is kept, the original raw form is retained
// for evidence rendering, and the defect is reported as a typed warning rather
// than silently repaired.
package email

import (
	"mime"
	"net/mail"
	"net/textproto"
	"strings"
	"time"
	"unicode"
)

// WarningCode names a deterministic parse defect. Codes are stable strings so
// they can be persisted and shown to a caller without re-deriving them.
type WarningCode string

const (
	WarnMissingMessageID    WarningCode = "missing_message_id"
	WarnMalformedIdentifier WarningCode = "malformed_identifier"
	WarnDuplicateIdentifier WarningCode = "duplicate_identifier"
	WarnMalformedAddress    WarningCode = "malformed_address"
	WarnMissingDate         WarningCode = "missing_date"
	WarnUnparsableDate      WarningCode = "unparsable_date"
)

// ParseWarning is one defect found in one header. Value carries the offending
// token verbatim — never a repaired or synthesised form.
type ParseWarning struct {
	Code   WarningCode
	Header string // canonical header name, empty when the header is absent
	Value  string
}

// Mailbox is one parsed RFC 5322 mailbox.
//
// Address is the identity used for exact matching: lowercased, angle brackets
// stripped, surrounding whitespace removed. DisplayName is decoded from any
// encoded-word form. Raw is the mailbox exactly as it appeared, so evidence can
// render what the sender actually wrote. An unparsable mailbox keeps Raw, has
// an empty Address, and is reported as invalid rather than guessed at.
type Mailbox struct {
	Address     string
	DisplayName string
	Raw         string
	Valid       bool
}

// AddressList is one address header: its original display-form value and the
// mailboxes parsed out of it, in header order. Group syntax contributes its
// members; the group name itself is not a mailbox.
type AddressList struct {
	Header    string // original display-form header value, unfolded
	Mailboxes []Mailbox
}

// Addresses returns the normalized addresses of the valid mailboxes, in order.
func (a AddressList) Addresses() []string {
	out := make([]string, 0, len(a.Mailboxes))
	for _, m := range a.Mailboxes {
		if m.Valid {
			out = append(out, m.Address)
		}
	}
	return out
}

// AutomationClass is the bounded classification of automated traffic. It is
// derived from an explicit, closed set of header rules — never from body text.
type AutomationClass string

const (
	AutomationHuman          AutomationClass = "human"
	AutomationMailingList    AutomationClass = "mailing_list"
	AutomationDeliveryStatus AutomationClass = "delivery_status"
	AutomationAutoSubmitted  AutomationClass = "auto_submitted"
	AutomationBulk           AutomationClass = "bulk"
)

// AutomationSignal is a single header rule that fired. Signals are what makes
// a classification explainable: a caller can say which header caused it.
type AutomationSignal string

const (
	SignalListID               AutomationSignal = "list_id"
	SignalListUnsubscribe      AutomationSignal = "list_unsubscribe"
	SignalPrecedence           AutomationSignal = "precedence"
	SignalAutoSubmitted        AutomationSignal = "auto_submitted"
	SignalDeliveryStatusReport AutomationSignal = "delivery_status_report"
	SignalMailerDaemonSender   AutomationSignal = "mailer_daemon_sender"
	SignalNullReturnPath       AutomationSignal = "null_return_path"
)

// Automation is the typed classification result. Only the few header values
// the rules depend on are retained — arbitrary headers are not stored.
type Automation struct {
	Class         AutomationClass
	Signals       []AutomationSignal
	ListID        string // canonical List-Id, angle brackets stripped
	Precedence    string // lowercased Precedence value when present
	AutoSubmitted string // lowercased Auto-Submitted value when present
}

// Automated reports whether any automation rule fired.
func (a Automation) Automated() bool { return a.Class != "" && a.Class != AutomationHuman }

// Headers is the structured header model of one message.
//
// Canonical identifiers carry no angle brackets and are the join keys for reply
// linkage. The Raw* fields keep the header exactly as received so evidence can
// show the original and so a defect stays inspectable after parsing.
type Headers struct {
	MessageID    string // canonical, empty when absent or malformed
	RawMessageID string

	InReplyTo    []string // canonical, deduplicated, first occurrence wins
	RawInReplyTo string

	References    []string // canonical, ordered, deduplicated, first occurrence wins
	RawReferences string

	From    AddressList
	ReplyTo AddressList
	To      AddressList
	Cc      AddressList
	Bcc     AddressList

	Date    time.Time // zero unless HasDate
	HasDate bool

	Automation Automation
	Warnings   []ParseWarning
}

// HasWarning reports whether a warning with this code was emitted.
func (h Headers) HasWarning(code WarningCode) bool {
	for _, w := range h.Warnings {
		if w.Code == code {
			return true
		}
	}
	return false
}

// ParseHeaders builds the structured model from an already-read message header
// block. Warnings are emitted in a fixed order — identifiers, then addresses,
// then the date — so two parses of the same bytes always agree.
func ParseHeaders(h mail.Header) Headers {
	out := Headers{}

	out.RawMessageID = headerValue(h, "Message-Id")
	out.MessageID, out.Warnings = parseMessageID(h, out.Warnings)

	out.RawInReplyTo = headerValue(h, "In-Reply-To")
	out.InReplyTo, out.Warnings = parseIdentifierList("In-Reply-To", out.RawInReplyTo, out.Warnings)

	out.RawReferences = headerValue(h, "References")
	out.References, out.Warnings = parseIdentifierList("References", out.RawReferences, out.Warnings)

	for _, f := range []struct {
		name string
		dst  *AddressList
	}{
		{"From", &out.From},
		{"Reply-To", &out.ReplyTo},
		{"To", &out.To},
		{"Cc", &out.Cc},
		{"Bcc", &out.Bcc},
	} {
		list := ParseMailboxes(headerValue(h, f.name))
		*f.dst = list
		for _, m := range list.Mailboxes {
			if !m.Valid {
				out.Warnings = append(out.Warnings,
					ParseWarning{Code: WarnMalformedAddress, Header: f.name, Value: m.Raw})
			}
		}
	}

	raw := headerValue(h, "Date")
	switch {
	case strings.TrimSpace(raw) == "":
		out.Warnings = append(out.Warnings, ParseWarning{Code: WarnMissingDate, Header: "Date"})
	default:
		d, err := mail.ParseDate(raw)
		if err != nil {
			out.Warnings = append(out.Warnings,
				ParseWarning{Code: WarnUnparsableDate, Header: "Date", Value: raw})
		} else {
			out.Date, out.HasDate = d, true
		}
	}

	out.Automation = classifyAutomation(h, out.From)
	return out
}

// headerValue reads one header without the panic-on-missing-map behaviour of
// mail.Header.Get on a nil map, and takes the first occurrence when a header
// was repeated.
func headerValue(h mail.Header, name string) string {
	v := h[textproto.CanonicalMIMEHeaderKey(name)]
	if len(v) == 0 {
		return ""
	}
	return v[0]
}

func parseMessageID(h mail.Header, warns []ParseWarning) (string, []ParseWarning) {
	values := h[textproto.CanonicalMIMEHeaderKey("Message-Id")]
	if len(values) == 0 || strings.TrimSpace(values[0]) == "" {
		return "", append(warns, ParseWarning{Code: WarnMissingMessageID, Header: "Message-ID"})
	}
	// A message carrying more than one Message-ID header, or more than one
	// identifier in the header, is ambiguous: keep the first and say so.
	for _, extra := range values[1:] {
		warns = append(warns,
			ParseWarning{Code: WarnDuplicateIdentifier, Header: "Message-ID", Value: extra})
	}

	tokens := splitIdentifiers(values[0])
	if len(tokens) == 0 {
		return "", append(warns, ParseWarning{Code: WarnMissingMessageID, Header: "Message-ID"})
	}
	for _, extra := range tokens[1:] {
		warns = append(warns,
			ParseWarning{Code: WarnDuplicateIdentifier, Header: "Message-ID", Value: extra})
	}

	id, ok := CanonicalIdentifier(tokens[0])
	if !ok {
		return "", append(warns,
			ParseWarning{Code: WarnMalformedIdentifier, Header: "Message-ID", Value: tokens[0]})
	}
	return id, warns
}

// parseIdentifierList canonicalises a reference-style header, preserving order
// and keeping the first occurrence of a repeated identifier.
func parseIdentifierList(name, raw string, warns []ParseWarning) ([]string, []ParseWarning) {
	if strings.TrimSpace(raw) == "" {
		return nil, warns
	}
	var ids []string
	seen := map[string]bool{}
	for _, tok := range splitIdentifiers(raw) {
		id, ok := CanonicalIdentifier(tok)
		if !ok {
			warns = append(warns,
				ParseWarning{Code: WarnMalformedIdentifier, Header: name, Value: tok})
			continue
		}
		if seen[id] {
			warns = append(warns,
				ParseWarning{Code: WarnDuplicateIdentifier, Header: name, Value: id})
			continue
		}
		seen[id] = true
		ids = append(ids, id)
	}
	return ids, warns
}

// splitIdentifiers cuts a msg-id header into tokens. Angle-bracketed ids are
// taken as written even when they are run together without separators; anything
// outside brackets is split on whitespace and commas, so folded headers and
// bracketless ids both survive to canonicalisation.
func splitIdentifiers(raw string) []string {
	var out []string
	var buf strings.Builder
	inAngle := false

	flush := func(wrap bool) {
		s := buf.String()
		buf.Reset()
		if wrap {
			// An unterminated "<" is kept visible so it is reported malformed.
			out = append(out, "<"+s)
			return
		}
		if s = strings.TrimSpace(s); s != "" {
			out = append(out, s)
		}
	}

	for _, r := range raw {
		switch {
		case r == '<':
			flush(inAngle)
			inAngle = true
		case r == '>' && inAngle:
			out = append(out, "<"+buf.String()+">")
			buf.Reset()
			inAngle = false
		case !inAngle && (r == ',' || unicode.IsSpace(r)):
			flush(false)
		default:
			buf.WriteRune(r)
		}
	}
	if buf.Len() > 0 || inAngle {
		flush(inAngle)
	}
	return out
}

// CanonicalIdentifier strips the angle brackets from one msg-id token and
// reports whether what remains is a usable identifier. It is deliberately
// strict — a token that cannot be trusted as a join key is rejected rather
// than repaired, because a wrong identifier cross-links conversations.
func CanonicalIdentifier(token string) (string, bool) {
	s := strings.TrimSpace(token)
	if strings.HasPrefix(s, "<") && strings.HasSuffix(s, ">") && len(s) >= 2 {
		s = s[1 : len(s)-1]
	}
	s = strings.TrimSpace(s)
	if s == "" || strings.ContainsAny(s, "<>") {
		return "", false
	}
	if strings.IndexFunc(s, unicode.IsSpace) >= 0 {
		return "", false
	}
	at := strings.LastIndex(s, "@")
	if at <= 0 || at == len(s)-1 {
		return "", false
	}
	return s, true
}

// NormalizeAddress is the exact-match form of a mailbox address: angle brackets
// stripped, trimmed, lowercased. Case folding the local part is not strictly
// RFC-safe, but mailbox identity in practice is case-insensitive and browse
// filters have to agree with what was indexed.
func NormalizeAddress(addr string) string {
	s := strings.TrimSpace(addr)
	s = strings.TrimPrefix(s, "<")
	s = strings.TrimSuffix(s, ">")
	return strings.ToLower(strings.TrimSpace(s))
}

// ParseMailboxes parses one address header, preserving order and the original
// display form. Unlike net/mail's list parser, one malformed mailbox does not
// discard the rest of the header: each mailbox is parsed on its own, and a
// failure is recorded as an invalid mailbox that still carries its raw text.
func ParseMailboxes(raw string) AddressList {
	list := AddressList{Header: raw}
	parser := mail.AddressParser{WordDecoder: headerWordDecoder()}

	for _, tok := range splitMailboxTokens(raw) {
		addr, err := parser.Parse(tok)
		if err != nil {
			list.Mailboxes = append(list.Mailboxes, Mailbox{Raw: tok})
			continue
		}
		list.Mailboxes = append(list.Mailboxes, Mailbox{
			Address:     NormalizeAddress(addr.Address),
			DisplayName: addr.Name,
			Raw:         tok,
			Valid:       true,
		})
	}
	return list
}

// splitMailboxTokens splits an address header on the commas that separate
// mailboxes, ignoring commas inside quoted strings, angle brackets and
// comments. A group ("Team: a@x, b@y;") contributes its members: the group name
// before the colon is dropped and the terminating semicolon ends the group, so
// an empty group such as "undisclosed-recipients:;" yields no mailboxes.
func splitMailboxTokens(raw string) []string {
	var out []string
	var buf strings.Builder
	var inQuote, inAngle bool
	comment := 0

	flush := func() {
		if s := strings.TrimSpace(buf.String()); s != "" {
			out = append(out, s)
		}
		buf.Reset()
	}

	escaped := false
	for _, r := range raw {
		if escaped {
			buf.WriteRune(r)
			escaped = false
			continue
		}
		switch {
		case inQuote:
			buf.WriteRune(r)
			switch r {
			case '\\':
				escaped = true
			case '"':
				inQuote = false
			}
		case r == '"':
			buf.WriteRune(r)
			inQuote = true
		case comment > 0:
			buf.WriteRune(r)
			if r == '(' {
				comment++
			} else if r == ')' {
				comment--
			}
		case r == '(':
			buf.WriteRune(r)
			comment++
		case r == '<':
			buf.WriteRune(r)
			inAngle = true
		case r == '>':
			buf.WriteRune(r)
			inAngle = false
		case r == ':' && !inAngle:
			// Group name: not a mailbox of its own.
			buf.Reset()
		case (r == ',' || r == ';') && !inAngle:
			flush()
		default:
			buf.WriteRune(r)
		}
	}
	flush()
	return out
}

func headerWordDecoder() *mime.WordDecoder {
	dec := new(mime.WordDecoder)
	dec.CharsetReader = charsetReader
	return dec
}

// classifyAutomation applies the closed set of automated-traffic rules. Each
// rule is one header check; nothing is inferred from body text, and no header
// outside this set is retained.
func classifyAutomation(h mail.Header, from AddressList) Automation {
	a := Automation{Class: AutomationHuman}
	var list, report bool

	if v := strings.TrimSpace(headerValue(h, "List-Id")); v != "" {
		list = true
		a.Signals = append(a.Signals, SignalListID)
		a.ListID = listID(v)
	}
	if strings.TrimSpace(headerValue(h, "List-Unsubscribe")) != "" {
		list = true
		a.Signals = append(a.Signals, SignalListUnsubscribe)
	}
	var bulk bool
	if v := strings.ToLower(strings.TrimSpace(headerValue(h, "Precedence"))); v != "" {
		a.Precedence = v
		switch v {
		case "bulk", "list", "junk", "auto_reply":
			bulk = true
			a.Signals = append(a.Signals, SignalPrecedence)
		}
	}
	var auto bool
	if v := strings.ToLower(strings.TrimSpace(headerValue(h, "Auto-Submitted"))); v != "" {
		a.AutoSubmitted = v
		// RFC 3834: "no" is the explicit statement that this is human mail.
		if !strings.HasPrefix(v, "no") {
			auto = true
			a.Signals = append(a.Signals, SignalAutoSubmitted)
		}
	}
	if isDeliveryReport(headerValue(h, "Content-Type")) {
		report = true
		a.Signals = append(a.Signals, SignalDeliveryStatusReport)
	}
	for _, m := range from.Mailboxes {
		if m.Valid && isDaemonAddress(m.Address) {
			report = true
			a.Signals = append(a.Signals, SignalMailerDaemonSender)
			break
		}
	}
	if v, ok := h[textproto.CanonicalMIMEHeaderKey("Return-Path")]; ok && len(v) > 0 {
		if s := strings.TrimSpace(v[0]); s == "<>" || s == "" {
			report = true
			a.Signals = append(a.Signals, SignalNullReturnPath)
		}
	}

	// A bounce explains the message better than the list or bulk markers a
	// report may also carry, so it wins; list membership outranks the weaker
	// auto-submitted and bulk markers for the same reason.
	switch {
	case report:
		a.Class = AutomationDeliveryStatus
	case list:
		a.Class = AutomationMailingList
	case auto:
		a.Class = AutomationAutoSubmitted
	case bulk:
		a.Class = AutomationBulk
	}
	return a
}

// listID extracts the identifier from a List-Id value, which is a description
// followed by the identifier in angle brackets.
func listID(v string) string {
	if i := strings.LastIndex(v, "<"); i >= 0 {
		if j := strings.Index(v[i:], ">"); j > 0 {
			return strings.ToLower(strings.TrimSpace(v[i+1 : i+j]))
		}
	}
	return strings.ToLower(strings.TrimSpace(v))
}

func isDeliveryReport(contentType string) bool {
	if strings.TrimSpace(contentType) == "" {
		return false
	}
	mediaType, params, err := mime.ParseMediaType(contentType)
	if err != nil {
		return false
	}
	switch strings.ToLower(mediaType) {
	case "message/delivery-status", "message/disposition-notification":
		return true
	case "multipart/report":
		switch strings.ToLower(params["report-type"]) {
		case "delivery-status", "disposition-notification":
			return true
		}
	}
	return false
}

// isDaemonAddress recognises the conventional bounce senders. The check is on
// the local part only: the domain belongs to whichever host generated it.
func isDaemonAddress(address string) bool {
	local := address
	if i := strings.LastIndex(address, "@"); i > 0 {
		local = address[:i]
	}
	switch local {
	case "mailer-daemon", "mail-daemon", "postmaster":
		return true
	}
	return false
}
