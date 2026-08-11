package worker

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"sync"

	"github.com/suankan/pocket-advisor/internal/domain"
	"github.com/suankan/pocket-advisor/internal/engine/email"
	"github.com/suankan/pocket-advisor/internal/storage/postgres"
	"github.com/suankan/pocket-advisor/internal/storage/rustfs"
)

// Metadata reprocessing: rebuilding the durable email browse metadata of
// documents that were ingested before those tables existed
// (ingestion-design.md §2.5).
//
// It is the same work the email worker does on a live message, minus the body:
// read the authoritative Tier 1 bytes, parse them with the one MIME parser,
// map the parsed headers with emailMessageOf, and write them through the one
// repository transaction. Nothing here re-derives a document, a chunk, a
// thread id or a processing status — a message that has already been extracted
// is not extracted again, and a message whose bytes cannot be read is counted
// and reported rather than rewritten from whatever Tier 2 happens to hold.

// EmailDocumentSelector lists the email message documents of one workspace.
type EmailDocumentSelector interface {
	EmailDocuments(ctx context.Context, q postgres.EmailDocumentQuery) ([]domain.Document, error)
}

// EmailMetadataWriter is the single write path for browse metadata: one
// message, one transaction, idempotent on doc_id.
type EmailMetadataWriter interface {
	SaveEmailMessage(ctx context.Context, m domain.EmailMessage) (domain.EmailConversation, error)
}

// EmailObjectReader reads Tier 1, the only authority for what a message says.
type EmailObjectReader interface {
	KeyFromURI(uri string) (string, error)
	Get(ctx context.Context, key string) ([]byte, rustfs.Provenance, error)
}

// EmailMetadataReprocessor rebuilds browse metadata for one workspace.
type EmailMetadataReprocessor struct {
	Docs   EmailDocumentSelector
	Vault  EmailObjectReader
	Emails EmailMetadataWriter
	Log    *slog.Logger
}

// log keeps a reprocessor usable without telemetry wiring; a maintenance
// command that panicked on a nil logger would be worse than a quiet one.
func (p *EmailMetadataReprocessor) log() *slog.Logger {
	if p.Log != nil {
		return p.Log
	}
	return slog.New(slog.NewTextHandler(io.Discard, nil))
}

// EmailReprocessOptions bounds one run.
type EmailReprocessOptions struct {
	// WorkspaceID is fixed for the whole run. Every read and every write is
	// scoped to it; there is no cross-workspace pass (workspace-isolation.md §13).
	WorkspaceID string
	// Limit caps the documents examined in this run; 0 means every email
	// message document in the workspace.
	Limit int
	// BatchSize is how many documents one keyset page fetches.
	BatchSize int
	// Concurrency bounds concurrent Tier 1 reads and metadata writes.
	Concurrency int
	// OnlyMissing narrows selection to documents with no metadata row yet.
	OnlyMissing bool
	// DryRun reads and parses but writes nothing, so a run can report what it
	// would rebuild — and which documents it cannot read — before changing a
	// single row.
	DryRun bool
	// Progress, when set, is called with the running totals after each batch.
	Progress func(EmailReprocessSummary)
}

// Defaults for an unconfigured run. The concurrency default is deliberately
// small: this is one object store and one database being walked by a
// maintenance command, not the ingest pipeline, and it must not crowd out a
// pipeline running beside it.
const (
	defaultReprocessBatch       = 200
	defaultReprocessConcurrency = 4
	maxReprocessConcurrency     = 32
)

func (o *EmailReprocessOptions) normalize() {
	if o.BatchSize <= 0 {
		o.BatchSize = defaultReprocessBatch
	}
	if o.Concurrency <= 0 {
		o.Concurrency = defaultReprocessConcurrency
	}
	if o.Concurrency > maxReprocessConcurrency {
		o.Concurrency = maxReprocessConcurrency
	}
	if o.Limit > 0 && o.BatchSize > o.Limit {
		o.BatchSize = o.Limit
	}
}

// EmailReprocessSummary is what a run reports. Counts only: no subject, no
// address, no identifier and no source path ever reaches this struct, because
// it is printed to a terminal and written to a log file.
type EmailReprocessSummary struct {
	WorkspaceID string `json:"workspace_id"`
	// Processed is every document examined, whatever the outcome.
	Processed int `json:"processed"`
	// Updated is metadata successfully rebuilt — or, under DryRun, metadata
	// that would have been rebuilt.
	Updated int `json:"updated"`
	// Unreadable is documents whose authoritative Tier 1 bytes could not be
	// read. Reported rather than skipped silently: their metadata is missing
	// and nothing in this run can supply it.
	Unreadable int `json:"unreadable"`
	// Failed is bytes that were read and could not be turned into metadata.
	Failed int `json:"failed"`
	// Reasons counts the redacted classification codes behind Unreadable and
	// Failed. The codes are a closed set, so nothing message-derived leaks
	// into a summary or a log line.
	Reasons map[string]int `json:"reasons,omitempty"`
	DryRun  bool           `json:"dry_run"`
}

func (s *EmailReprocessSummary) record(outcome EmailReprocessOutcome, code string) {
	switch outcome {
	case EmailReprocessCancelled:
		// Never examined, so never counted: an interrupted run reports what it
		// did, not what it was about to do.
		return
	case EmailReprocessUpdated:
		s.Processed++
		s.Updated++
		return
	case EmailReprocessUnreadable:
		s.Processed++
		s.Unreadable++
	default:
		s.Processed++
		s.Failed++
	}
	if s.Reasons == nil {
		s.Reasons = map[string]int{}
	}
	s.Reasons[code]++
}

// EmailReprocessOutcome is one document's result.
type EmailReprocessOutcome string

const (
	EmailReprocessUpdated    EmailReprocessOutcome = "updated"
	EmailReprocessUnreadable EmailReprocessOutcome = "unreadable"
	EmailReprocessFailed     EmailReprocessOutcome = "failed"
	// EmailReprocessCancelled is a document the run stopped before reaching.
	// It is not an outcome of the document, so it is not counted as one.
	EmailReprocessCancelled EmailReprocessOutcome = "cancelled"
)

// The reason codes. They are diagnostic labels for the summary and the log,
// not document statuses: reprocessing never rewrites processing_status, so a
// document that cannot be read here keeps whatever the pipeline recorded for
// it and stays exactly as recoverable as it was.
const (
	ReasonReprocessNoObject      = "no_tier1_object"
	ReasonReprocessBadObjectURI  = "bad_object_uri"
	ReasonReprocessUnreadable    = "tier1_unreadable"
	ReasonReprocessUnknownEncode = "unknown_encoding"
	ReasonReprocessParseFailed   = "email_parse_failed"
	ReasonReprocessWriteFailed   = "metadata_write_failed"
	ReasonReprocessUnclassified  = "unclassified"
)

// reprocessError carries the classification with the error rather than leaving
// it to be re-derived from an error string, which is both fragile and a way to
// end up logging message text.
type reprocessError struct {
	outcome EmailReprocessOutcome
	code    string
	err     error
}

func (e *reprocessError) Error() string { return e.code + ": " + e.err.Error() }
func (e *reprocessError) Unwrap() error { return e.err }

// classifyEmailReprocess maps one document's error onto its outcome and a
// redacted reason code.
//
// A cancelled context is neither an unreadable document nor a failed one — the
// run was stopped, and counting the document as broken would misreport an
// operator's Ctrl+C as corpus damage.
func classifyEmailReprocess(err error) (EmailReprocessOutcome, string) {
	switch {
	case err == nil:
		return EmailReprocessUpdated, ""
	case errors.Is(err, context.Canceled), errors.Is(err, context.DeadlineExceeded):
		return EmailReprocessCancelled, ""
	}
	var re *reprocessError
	if errors.As(err, &re) {
		return re.outcome, re.code
	}
	return EmailReprocessFailed, ReasonReprocessUnclassified
}

// Run walks the workspace's email message documents and rebuilds their
// metadata.
//
// Deterministic order, bounded concurrency, and a keyset cursor: the batch is
// selected in doc_id order, processed by at most Concurrency goroutines, and
// tallied back in selection order so the summary does not depend on which
// goroutine finished first. Cancellation stops the walk and returns the
// summary accumulated so far along with the context's error, because a partial
// run is still a real result — every document it did write is complete.
func (p *EmailMetadataReprocessor) Run(ctx context.Context, o EmailReprocessOptions) (EmailReprocessSummary, error) {
	o.normalize()
	summary := EmailReprocessSummary{WorkspaceID: o.WorkspaceID, DryRun: o.DryRun}
	if o.WorkspaceID == "" {
		return summary, errors.New("email metadata reprocessing requires a workspace id")
	}

	cursor := ""
	for {
		if err := ctx.Err(); err != nil {
			return summary, err
		}
		size := o.BatchSize
		if o.Limit > 0 {
			remaining := o.Limit - summary.Processed
			if remaining <= 0 {
				break
			}
			if size > remaining {
				size = remaining
			}
		}

		batch, err := p.Docs.EmailDocuments(ctx, postgres.EmailDocumentQuery{
			WorkspaceID: o.WorkspaceID,
			After:       cursor,
			Limit:       size,
			OnlyMissing: o.OnlyMissing,
		})
		if err != nil {
			return summary, fmt.Errorf("select email documents: %w", err)
		}
		if len(batch) == 0 {
			break
		}

		results := p.processBatch(ctx, batch, o)
		for _, err := range results {
			outcome, code := classifyEmailReprocess(err)
			if outcome == EmailReprocessCancelled {
				continue
			}
			summary.record(outcome, code)
			if outcome != EmailReprocessUpdated {
				// Closed-set outcome and reason only. The underlying error can
				// quote a header line or an object key, and even a document id is
				// an identifier that has no place in an operator log.
				p.log().Warn("email metadata not rebuilt",
					"outcome", string(outcome), "reason", code)
			}
		}

		cursor = batch[len(batch)-1].DocID
		if o.Progress != nil {
			o.Progress(summary)
		}
		if err := ctx.Err(); err != nil {
			return summary, err
		}
		if len(batch) < size {
			break
		}
	}
	return summary, nil
}

// processBatch runs one page through at most Concurrency workers and returns
// each document's error in selection order.
func (p *EmailMetadataReprocessor) processBatch(ctx context.Context, batch []domain.Document, o EmailReprocessOptions) []error {
	results := make([]error, len(batch))
	slots := make(chan struct{}, o.Concurrency)
	var wg sync.WaitGroup

	for i := range batch {
		if ctx.Err() != nil {
			results[i] = ctx.Err()
			continue
		}
		wg.Add(1)
		slots <- struct{}{}
		go func(i int) {
			defer wg.Done()
			defer func() { <-slots }()
			results[i] = p.reprocessOne(ctx, batch[i], o)
		}(i)
	}
	wg.Wait()
	return results
}

// reprocessOne rebuilds one document's metadata from its Tier 1 bytes.
func (p *EmailMetadataReprocessor) reprocessOne(ctx context.Context, doc domain.Document, o EmailReprocessOptions) error {
	if doc.RawURI == "" {
		return &reprocessError{EmailReprocessUnreadable, ReasonReprocessNoObject,
			errors.New("document has no tier 1 object")}
	}
	key, err := p.Vault.KeyFromURI(doc.RawURI)
	if err != nil {
		return &reprocessError{EmailReprocessUnreadable, ReasonReprocessBadObjectURI, err}
	}
	data, _, err := p.Vault.Get(ctx, key)
	if err != nil {
		if ctx.Err() != nil {
			return ctx.Err()
		}
		return &reprocessError{EmailReprocessUnreadable, ReasonReprocessUnreadable, err}
	}

	parsed, err := email.ParseEmail(data)
	if err != nil {
		code := ReasonReprocessParseFailed
		if errors.Is(err, email.ErrUnknownCharset) {
			code = ReasonReprocessUnknownEncode
		}
		return &reprocessError{EmailReprocessFailed, code, err}
	}

	if o.DryRun {
		return nil
	}

	// The workspace comes from the run, not the row: the scope of this
	// operation is fixed at the command line and nothing read from a table
	// widens it.
	conv, err := p.Emails.SaveEmailMessage(ctx, emailMessageOf(doc.DocID, o.WorkspaceID, parsed))
	if err != nil {
		if ctx.Err() != nil {
			return ctx.Err()
		}
		return &reprocessError{EmailReprocessFailed, ReasonReprocessWriteFailed, err}
	}
	if conv.DuplicateOf != "" {
		// The repository retains the precise warning with the metadata row.
		// Logging either message or document identifier would leak it.
		p.log().Warn("duplicate message id while rebuilding metadata")
	}
	return nil
}
