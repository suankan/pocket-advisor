package cli

import (
	"bytes"
	"strings"
	"testing"
	"time"

	"github.com/suankan/pocket-advisor/internal/domain"
	"github.com/suankan/pocket-advisor/internal/storage/postgres"
)

// The summary says what the run did, what is still undone, and names what only
// the owner can resolve, with this run's items marked and the list capped.
func TestPrintRunSummary(t *testing.T) {
	s := postgres.RunSummary{
		Files: 120, NewOriginals: 3,
		Settled: map[domain.Status][]postgres.Tally{
			domain.StatusCompleted: {{Reason: "", Count: 7}},
			domain.StatusSkipped:   {{Reason: "IMAGE_UNREADABLE", Count: 2}, {Reason: "ARCHIVE_ENCRYPTED", Count: 1}},
			domain.StatusFailed:    {{Reason: "OCR_FAILED", Count: 1}},
		},
		FailedThisRun: []postgres.Tally{{Reason: "OCR_FAILED", Count: 1}},
		FailedEarlier: []postgres.Tally{{Reason: "UNKNOWN_ENCODING", Count: 2}},
		Locked:        4, LockedThisRun: 1,
		Attention: []postgres.AttentionDoc{
			{Status: domain.StatusSkipped, Reason: "ARCHIVE_ENCRYPTED", ThisRun: true, Location: "letters.zip in mail/a.eml"},
			{Status: domain.StatusFailed, Reason: "UNKNOWN_ENCODING", Location: "mail/b.eml"},
		},
		AttentionTotal: 3,
	}
	var b bytes.Buffer
	printRunSummary(&b, "synthetic", 75*time.Second, s)
	out := b.String()
	for _, want := range []string{
		"ingest summary: workspace synthetic, 1m15s",
		"sources    120 files observed, 3 new",
		"this run   11 documents",
		"  7 completed\n",
		"  3 skipped    IMAGE_UNREADABLE 2, ARCHIVE_ENCRYPTED 1",
		"  1 failed     OCR_FAILED 1",
		"not done   0 unfinished, 0 waiting for a source",
		"3 failed: 1 this run (OCR_FAILED 1), 2 earlier (UNKNOWN_ENCODING 2)",
		"4 password-protected, 1 of them found this run",
		"skipped  ARCHIVE_ENCRYPTED  letters.zip in mail/a.eml  [this run]",
		"failed   UNKNOWN_ENCODING   mail/b.eml\n",
		"... and 1 more",
		"--recover retries",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("missing %q in:\n%s", want, out)
		}
	}

	b.Reset()
	printRunSummary(&b, "synthetic", time.Second, postgres.RunSummary{Files: 120})
	if out := b.String(); !strings.Contains(out, "nothing new to process") || strings.Contains(out, "needs you") {
		t.Errorf("a quiet run:\n%s", out)
	}
}

// Unsupported files are listed by type, largest first, with one example each
// and the rest of the list capped.
func TestPrintRunSummaryListsUnsupportedTypes(t *testing.T) {
	groups := []postgres.TypeGroup{
		{MimeType: "application/x-ole-storage", Extension: ".doc", Count: 4, Locations: []string{"docs/old.doc"}},
		{MimeType: "application/octet-stream", Count: 2, Locations: []string{"ATT00001 in mail/c.eml"}},
	}
	for i := 0; i < maxTypes; i++ {
		groups = append(groups, postgres.TypeGroup{MimeType: "audio/mpeg", Extension: ".mp3", Count: 1})
	}
	var b bytes.Buffer
	printRunSummary(&b, "synthetic", time.Second, postgres.RunSummary{Files: 1, Unsupported: groups})
	out := b.String()
	for _, want := range []string{
		"unsupported 21 files skipped, by type, across all runs",
		"4  application/x-ole-storage (.doc)",
		"e.g. docs/old.doc",
		"2  application/octet-stream (no extension)",
		"e.g. ATT00001 in mail/c.eml",
		"... and 2 more types",
		"--doctor --json lists every unsupported file",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("missing %q in:\n%s", want, out)
		}
	}
}
