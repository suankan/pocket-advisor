package email

import (
	"strings"
	"testing"
)

func TestCompactDropsQuotedReplyChain(t *testing.T) {
	// The quoted chain is already indexed as its own message, so removing it
	// is deduplication rather than summarisation (§4.3).
	in := "Yes, agreed.\n\nOn Mon, 3 Feb 2025 at 10:00, Someone <s@x.com> wrote:\n> the original question\n> second line\n"
	got := Compact(in)

	if !strings.Contains(got, "Yes, agreed.") {
		t.Errorf("the author's own words were lost: %q", got)
	}
	if strings.Contains(got, "original question") {
		t.Errorf("quoted chain survived: %q", got)
	}
}

func TestCompactDropsSignature(t *testing.T) {
	in := "The invoice is attached.\n\n-- \nJane Doe\nPartner, Firm LLP\n+61 400 000 000"
	got := Compact(in)

	if !strings.Contains(got, "invoice is attached") {
		t.Errorf("body lost: %q", got)
	}
	if strings.Contains(got, "Partner, Firm LLP") {
		t.Errorf("signature survived: %q", got)
	}
}

func TestCompactPreservesContentVerbatim(t *testing.T) {
	// Compaction must never rewrite the author's words — that distinction is
	// the whole basis for indexing source text only (pillar 8).
	in := "Payment of $12,345.67 was made on 3 February 2025 to ACME Pty Ltd."
	if got := Compact(in); got != in {
		t.Errorf("content was altered:\n got %q\nwant %q", got, in)
	}
}

func TestCompactHandlesCyrillic(t *testing.T) {
	in := "Здравствуйте, документы приложены."
	if got := Compact(in); got != in {
		t.Errorf("cyrillic altered:\n got %q\nwant %q", got, in)
	}
}

func TestStripHTMLDropsScriptAndStyle(t *testing.T) {
	in := `<html><head><style>.x{color:red}</style></head>` +
		`<body><p>Real content</p><script>alert(1)</script></body></html>`
	got := StripHTML(in)

	if !strings.Contains(got, "Real content") {
		t.Errorf("content lost: %q", got)
	}
	for _, junk := range []string{"color:red", "alert"} {
		if strings.Contains(got, junk) {
			t.Errorf("%q survived html stripping: %q", junk, got)
		}
	}
}

func TestThreadKeyNormalisesReplyPrefixes(t *testing.T) {
	a := ThreadKey("Re: Fwd: Settlement offer", "Jane@Example.com")
	b := ThreadKey("Settlement offer", "jane@example.com")
	if a != b {
		t.Errorf("reply prefixes not normalised:\n %q\n %q", a, b)
	}
	if ThreadKey("Different subject", "jane@example.com") == b {
		t.Error("distinct subjects collapsed into one thread")
	}
}

func TestParseEmailExtractsAttachment(t *testing.T) {
	raw := "From: a@example.com\r\n" +
		"To: b@example.com\r\n" +
		"Subject: With attachment\r\n" +
		"MIME-Version: 1.0\r\n" +
		"Content-Type: multipart/mixed; boundary=BOUND\r\n\r\n" +
		"--BOUND\r\n" +
		"Content-Type: text/plain\r\n\r\n" +
		"Please see attached.\r\n" +
		"--BOUND\r\n" +
		"Content-Type: application/pdf; name=\"statement.pdf\"\r\n" +
		"Content-Disposition: attachment; filename=\"statement.pdf\"\r\n" +
		"Content-Transfer-Encoding: base64\r\n\r\n" +
		"JVBERi0xLjcK\r\n" +
		"--BOUND--\r\n"

	p, err := ParseEmail([]byte(raw))
	if err != nil {
		t.Fatal(err)
	}
	if p.Subject != "With attachment" {
		t.Errorf("subject: %q", p.Subject)
	}
	if !strings.Contains(p.BodyText, "Please see attached") {
		t.Errorf("body: %q", p.BodyText)
	}
	if len(p.Children) != 1 {
		t.Fatalf("expected 1 attachment, got %d", len(p.Children))
	}
	if p.Children[0].Filename != "statement.pdf" {
		t.Errorf("filename: %q", p.Children[0].Filename)
	}
	// base64 must be decoded, not stored encoded.
	if !strings.HasPrefix(string(p.Children[0].Data), "%PDF-1.7") {
		t.Errorf("attachment not decoded: %q", p.Children[0].Data)
	}
}

func TestUnrollArchiveRejectsUnknownFormat(t *testing.T) {
	if _, err := UnrollArchive([]byte("not an archive"), "x.rar"); err == nil {
		t.Error("expected unsupported archive format to error")
	}
}
