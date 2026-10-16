package cli

import (
	"context"
	"fmt"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/suankan/pocket-advisor/internal/doctor"
	"github.com/suankan/pocket-advisor/internal/domain"
	"github.com/suankan/pocket-advisor/internal/storage/postgres"
)

func TestParseAcceptsTheDocumentedInvocations(t *testing.T) {
	cases := map[string][]string{
		"ingest-all":  {"--ingest-all", "--workspace-id", "test"},
		"delete-data": {"--delete-data", "--workspace-id", "test"},
		"reconcile":   {"--reconcile", "--workspace-id", "test"},
		"doctor":      {"--doctor", "--workspace-id", "test"},
		"recover":     {"--recover", "--workspace-id", "test"},
	}
	for wantMode, args := range cases {
		o, err := Parse(args)
		if err != nil {
			t.Errorf("Parse(%v): %v", args, err)
			continue
		}
		if o.Mode() != wantMode {
			t.Errorf("Parse(%v).Mode() = %q, want %q", args, o.Mode(), wantMode)
		}
	}
}

// Selecting two modes at once is ambiguous in a way that could destroy data —
// --delete-data alongside --ingest-all must never be resolved by picking one.
func TestParseRejectsRemovedFixtureGenerationFlags(t *testing.T) {
	for _, flag := range []string{"--generate-fixtures", "--eval-generate"} {
		if _, err := Parse([]string{"--evaluate", flag, "--workspace-id", "test"}); err == nil {
			t.Errorf("Parse accepted removed flag %s", flag)
		}
	}
}

// --scan ran exactly what --reconcile runs and was folded into it.
func TestParseRejectsRemovedScanMode(t *testing.T) {
	if _, err := Parse([]string{"--scan", "--workspace-id", "test"}); err == nil {
		t.Error("Parse accepted removed mode --scan")
	}
}

func TestParseRejectsMultipleModes(t *testing.T) {
	_, err := Parse([]string{"--ingest-all", "--delete-data", "--workspace-id", "test"})
	if err == nil {
		t.Fatal("Parse accepted two modes at once")
	}
	if !strings.Contains(err.Error(), "mutually exclusive") {
		t.Errorf("error %q does not explain the conflict", err)
	}
}

func TestParseRequiresAMode(t *testing.T) {
	if _, err := Parse([]string{"--workspace-id", "test"}); err == nil {
		t.Fatal("Parse accepted an invocation with no mode")
	}
}

// A mode that defaulted its workspace could purge the wrong corpus. There is
// no shared database left to fall back to
// (workspace-isolation.md §13).
func TestParseRequiresWorkspace(t *testing.T) {
	for _, mode := range []string{
		"--ingest-all", "--delete-data", "--reconcile",
		"--doctor", "--recover",
	} {
		if _, err := Parse([]string{mode}); err == nil {
			t.Errorf("Parse(%s) accepted a missing --workspace-id", mode)
		}
	}
}

func TestParseValidatesForgetHash(t *testing.T) {
	valid := strings.Repeat("a", 64)
	if _, err := Parse([]string{"--forget", valid, "--workspace-id", "test"}); err != nil {
		t.Errorf("Parse rejected a valid sha256: %v", err)
	}
	if _, err := Parse([]string{"--forget", "abc123", "--workspace-id", "test"}); err == nil {
		t.Error("Parse accepted a truncated sha256")
	}
}

func TestDryRunRejectsModesWithoutAReadOnlyPreview(t *testing.T) {
	for _, args := range [][]string{
		{"--forget", strings.Repeat("a", 64)},
		{"--delete-data"}, {"--reconcile"}, {"--listen"},
	} {
		args = append(args, "--dry-run", "--workspace-id", "synthetic")
		if _, err := Parse(args); err == nil {
			t.Errorf("accepted unsupported preview: %v", args)
		}
	}
}

// Every mode that enqueues must also drain it: nothing else is listening, so a
// mode that enqueued without running the pools would publish into a void.
func TestNeedsPipelineCoversEveryEnqueueingMode(t *testing.T) {
	enqueues := map[string]bool{
		"--ingest-all": true, "--reconcile": true,
		"--delete-data": false, "--doctor": false, "--recover": false,
	}
	for mode, want := range enqueues {
		o, err := Parse([]string{mode, "--workspace-id", "test"})
		if err != nil {
			t.Fatalf("Parse(%s): %v", mode, err)
		}
		if got := o.NeedsPipeline(); got != want {
			t.Errorf("%s NeedsPipeline() = %v, want %v", mode, got, want)
		}
	}
}

func TestDoctorDoesNotNeedPipeline(t *testing.T) {
	o, err := Parse([]string{"--doctor", "--workspace-id", "test"})
	if err != nil {
		t.Fatal(err)
	}
	if o.NeedsPipeline() {
		t.Error("--doctor should not need pipeline")
	}
}

func TestRecoverDoesNotNeedPipeline(t *testing.T) {
	o, err := Parse([]string{"--recover", "--workspace-id", "test"})
	if err != nil {
		t.Fatal(err)
	}
	if o.NeedsPipeline() {
		t.Error("--recover should not need pipeline")
	}
}

// --reason takes reasons a document can fail with, comma-separated and in any
// case, or "all" on its own, and means nothing outside recovery.
func TestParseRecoverReasons(t *testing.T) {
	o, err := Parse([]string{"--recover", "--reason", "unclassified, UNKNOWN_ENCODING,unclassified", "--workspace-id", "test"})
	if err != nil || !slices.Equal(o.RecoverReasons, []domain.FailureReason{domain.ReasonUnclassified, domain.ReasonUnknownEncoding}) || o.RecoverAll {
		t.Fatalf("reasons %v, all %v, err %v", o.RecoverReasons, o.RecoverAll, err)
	}
	o, err = Parse([]string{"--recover", "--reason", "all", "--workspace-id", "test"})
	if err != nil || !o.RecoverAll || len(o.RecoverReasons) != 0 {
		t.Fatalf("reasons %v, all %v, err %v", o.RecoverReasons, o.RecoverAll, err)
	}
	for _, args := range [][]string{
		{"--doctor", "--reason", "UNCLASSIFIED", "--workspace-id", "test"},
		{"--recover", "--reason", "NOT_A_REASON", "--workspace-id", "test"},
		{"--recover", "--reason", "UNCLASSIFIED,SOURCE_UNAVAILABLE", "--workspace-id", "test"},
		{"--recover", "--reason", "all,UNCLASSIFIED", "--workspace-id", "test"},
	} {
		if _, err := Parse(args); err == nil {
			t.Errorf("%v parsed", args)
		}
	}
}

// A bare word ends flag parsing, so the flags after it would go unread: a
// mistyped "--recover all --yes" must fail instead of running as a dry run.
func TestParseRejectsBareWords(t *testing.T) {
	_, err := Parse([]string{"--recover", "all", "--yes", "--workspace-id", "test"})
	if err == nil || !strings.Contains(err.Error(), `"all"`) {
		t.Fatalf("err = %v", err)
	}
}

// A message is named by date, sender and subject, leaving out what it lacks.
func TestEmailLineNamesAMessage(t *testing.T) {
	for want, e := range map[string]*doctor.EmailRef{
		"2018-11-05, from a@example.com: Policy": {Date: "2018-11-05", From: "a@example.com", Subject: "Policy "},
		"from a@example.com":                     {From: "a@example.com"},
		"Policy":                                 {Subject: "Policy"},
	} {
		if got := emailLine(e); got != want {
			t.Errorf("emailLine(%+v) = %q, want %q", e, got, want)
		}
	}
}

// An item names its file, and its email from what the store holds or, for an
// email whose header was never stored, from its original; a member of an
// email is named by the message holding it.
func TestApplyDescriptionsNamesFilesAndMessages(t *testing.T) {
	plan := &doctor.RecoveryPlan{Items: []doctor.RecoveryItem{{DocID: "stored"}, {DocID: "unread"}, {DocID: "member"}, {DocID: "file"}, {DocID: "gone"}}}
	described := map[string]postgres.Description{
		"stored": {Location: "mail/1.eml", DocType: "email", From: "a@example.com", Subject: "Hello", Date: time.Date(2018, 11, 5, 0, 0, 0, 0, time.UTC)},
		"unread": {Location: "mail/2.eml", DocType: "email", SHA256: "sha"},
		"member": {Location: "bill.pdf in mail/3.eml", DocType: "pdf", From: "b@example.com", Subject: "Bill", InEmail: true},
		"file":   {Location: "docs/report.pdf", DocType: "pdf"},
	}
	read := func(_ context.Context, ref, sha string) ([]byte, error) {
		if ref != "source:unread" || sha != "sha" {
			return nil, fmt.Errorf("unexpected read of %s", ref)
		}
		return []byte("From: c@example.com\r\nSubject: Unreadable body\r\nDate: Tue, 10 Mar 2011 11:09:24 -0800\r\n\r\nbody\r\n"), nil
	}
	applyDescriptions(context.Background(), plan, described, read)
	want := map[string]struct {
		location, line string
		container      bool
	}{
		"stored": {"mail/1.eml", "2018-11-05, from a@example.com: Hello", false},
		"unread": {"mail/2.eml", "2011-03-10, from c@example.com: Unreadable body", false},
		"member": {"bill.pdf in mail/3.eml", "from b@example.com: Bill", true},
		"file":   {"docs/report.pdf", "", false},
		"gone":   {"", "", false},
	}
	for _, item := range plan.Items {
		w := want[item.DocID]
		line, container := "", false
		if item.Email != nil {
			line, container = emailLine(item.Email), item.Email.Container
		}
		if item.Location != w.location || line != w.line || container != w.container {
			t.Errorf("%s: %q / %q (in email %v), want %q / %q (%v)", item.DocID, item.Location, line, container, w.location, w.line, w.container)
		}
	}
}
