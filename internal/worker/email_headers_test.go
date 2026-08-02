package worker

import (
	"strings"
	"testing"

	"github.com/suankan/pocket-advisor/internal/engine/email"
)

const headeredMessage = "From: Jane Doe <jane@example.com>\r\n" +
	"To: John Doe <john@example.com>\r\n" +
	"Subject: Re: =?utf-8?B?0J/RgNC+INC/0L7QtdC30LTQutGD?=\r\n" +
	"Date: Wed, 07 Jan 2026 08:12:30 +1100\r\n" +
	"Content-Type: text/plain; charset=utf-8\r\n" +
	"\r\n" +
	"Сегодня в 22.00\r\n"

func TestHeadersAreLiftedOutOfTheBody(t *testing.T) {
	parsed, err := email.ParseEmail([]byte(headeredMessage))
	if err != nil {
		t.Fatalf("parse: %v", err)
	}

	h := headersOf(parsed)
	if h.Subject != "Re: Про поездку" {
		t.Errorf("subject = %q", h.Subject)
	}
	if !strings.Contains(h.From, "jane@example.com") {
		t.Errorf("from = %q", h.From)
	}
	if !strings.Contains(h.To, "john@example.com") {
		t.Errorf("to = %q", h.To)
	}
	if h.Date.IsZero() {
		t.Error("date not parsed")
	}

	// The body that reaches normalized_text is prose only. This is the whole
	// point of the change: an identical header block on every message in a
	// thread made them look alike to the embedder (§5.3).
	body := strings.TrimSpace(parsed.BodyText)
	for _, header := range []string{"Subject:", "From:", "To:", "Date:"} {
		if strings.Contains(body, header) {
			t.Errorf("body still carries %q header: %q", header, body)
		}
	}
	if body != "Сегодня в 22.00" {
		t.Errorf("body = %q, want %q", body, "Сегодня в 22.00")
	}
}

func TestHeadersOfToleratesMissingHeaders(t *testing.T) {
	parsed, err := email.ParseEmail([]byte(
		"Content-Type: text/plain; charset=utf-8\r\n\r\nbody with no headers\r\n"))
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	h := headersOf(parsed)
	if h.Subject != "" || h.From != "" || h.To != "" || !h.Date.IsZero() {
		t.Fatalf("expected zero headers, got %+v", h)
	}
	// No subject means no context header, not a stray "Subject: " prefix on
	// every chunk of the document.
	if got := h.ContextHeader(); got != "" {
		t.Fatalf("context header = %q, want empty", got)
	}
}
