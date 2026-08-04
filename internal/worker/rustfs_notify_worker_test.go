package worker

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net/url"
	"testing"
	"time"

	"github.com/nats-io/nats.go"
	"github.com/nats-io/nats.go/jetstream"

	"github.com/suankan/pocket-advisor/internal/domain"
)

const testSHA256 = "0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef"

type ingestCall struct {
	workspaceID string
	key         string
	mode        string
}

type fakeIngester struct {
	err   error
	calls []ingestCall
}

func (f *fakeIngester) Ingest(_ context.Context, workspaceID, key, mode string) error {
	f.calls = append(f.calls, ingestCall{workspaceID: workspaceID, key: key, mode: mode})
	return f.err
}

const testWorkspaceID = "matter-one"

func testNotifyWorker(svc Ingester) *RustFSNotifyWorker {
	return &RustFSNotifyWorker{
		Discovery:   svc,
		WorkspaceID: testWorkspaceID,
		Log:         slog.New(slog.NewTextHandler(io.Discard, nil)),
	}
}

// notificationMsg builds the payload RustFS's NATS target really sends, with
// the S3 event nested under "data".
//
// The earlier version of this helper wrote Records[].s3.object.key — the plain
// S3 shape from the pre-4.0.0 webhook — and the worker read the same wrong
// path, so these tests passed against a message no server produces. Every
// field below is copied from a live beta.12 message; the extra ones are kept
// precisely because they are what makes it recognisable as the real thing.
func notificationMsg(t *testing.T, encodedKey string) *notifyFakeMsg {
	t.Helper()
	body, err := json.Marshal(map[string]any{
		"EventName": "s3:ObjectCreated:Put",
		"Key":       "matter-one/" + encodedKey,
		"Records": []any{
			map[string]any{
				"object_name": encodedKey,
				"bucket_name": testWorkspaceID,
				"event_name":  "s3:ObjectCreated:Put",
				"data": map[string]any{
					"eventName": "s3:ObjectCreated:Put",
					"s3": map[string]any{
						"bucket": map[string]any{"name": testWorkspaceID},
						"object": map[string]any{"key": encodedKey},
					},
				},
			},
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	return &notifyFakeMsg{data: body}
}

// TestRustFSNotifyWorkerRejectsWebhookShapedPayload pins the regression
// directly: a record carrying only the old top-level "s3" path must not be
// read as a valid event. Without this, reverting the struct to the webhook
// shape would leave every other test in this file passing.
func TestRustFSNotifyWorkerRejectsWebhookShapedPayload(t *testing.T) {
	key := domain.RawObjectKey(testSHA256)
	body, err := json.Marshal(map[string]any{
		"Records": []any{
			map[string]any{
				"s3": map[string]any{
					"object": map[string]any{"key": url.QueryEscape(key)},
				},
			},
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	svc := &fakeIngester{}

	if err := testNotifyWorker(svc).Handle(context.Background(), &notifyFakeMsg{data: body}); err != nil {
		t.Fatalf("Handle: %v", err)
	}
	if len(svc.calls) != 0 {
		t.Fatalf("webhook-shaped payload reached ingest: %+v", svc.calls)
	}
}

// TestRustFSNotifyWorkerFallsBackToObjectName covers the other half of the
// key lookup, so the fallback cannot rot unnoticed behind the nested path.
func TestRustFSNotifyWorkerFallsBackToObjectName(t *testing.T) {
	key := domain.RawObjectKey(testSHA256)
	body, err := json.Marshal(map[string]any{
		"Records": []any{
			map[string]any{"object_name": url.QueryEscape(key)},
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	svc := &fakeIngester{}

	if err := testNotifyWorker(svc).Handle(context.Background(), &notifyFakeMsg{data: body}); err != nil {
		t.Fatalf("Handle: %v", err)
	}
	if len(svc.calls) != 1 {
		t.Fatalf("expected one ingest call, got %d", len(svc.calls))
	}
	if svc.calls[0].key != key {
		t.Fatalf("got key %q want %q", svc.calls[0].key, key)
	}
}

// TestRustFSNotifyWorkerReportsEmptyRecords guards the silent-drain failure:
// an unparsed payload must be visible, not acked as if it were handled.
func TestRustFSNotifyWorkerReportsEmptyRecords(t *testing.T) {
	svc := &fakeIngester{}

	if err := testNotifyWorker(svc).Handle(context.Background(),
		&notifyFakeMsg{data: []byte(`{"Records":[]}`)}); err != nil {
		t.Fatalf("Handle: %v", err)
	}
	if len(svc.calls) != 0 {
		t.Fatalf("empty records reached ingest: %+v", svc.calls)
	}
}

// RustFS form-URL-encodes the forward slashes in an object key (live-
// verified against beta.12, ingestion-design.md §5.2) — the worker must
// decode that back before validating/using the key. Workspace identity
// comes from the worker's own configured field now, not from the key (keys
// carry no workspace segment since each workspace has its own bucket), so
// this also confirms that field is what reaches Ingest, not anything parsed
// from the payload.
func TestRustFSNotifyWorkerDecodesS3ObjectKey(t *testing.T) {
	key := domain.RawObjectKey(testSHA256)
	svc := &fakeIngester{}

	err := testNotifyWorker(svc).Handle(context.Background(), notificationMsg(t, url.QueryEscape(key)))
	if err != nil {
		t.Fatalf("Handle: %v", err)
	}
	if len(svc.calls) != 1 {
		t.Fatalf("expected one ingest call, got %d", len(svc.calls))
	}
	want := ingestCall{workspaceID: testWorkspaceID, key: key, mode: "notify"}
	if svc.calls[0] != want {
		t.Fatalf("unexpected ingest call: got %+v want %+v", svc.calls[0], want)
	}
}

func TestRustFSNotifyWorkerIgnoresNonRawKeys(t *testing.T) {
	keys := []string{
		domain.ExtractedObjectKey(testSHA256),
		"other/prefix/object",
		"raw/ff/" + testSHA256, // shard doesn't match the hash
	}
	for _, key := range keys {
		t.Run(key, func(t *testing.T) {
			svc := &fakeIngester{}

			err := testNotifyWorker(svc).Handle(context.Background(), notificationMsg(t, url.QueryEscape(key)))
			if err != nil {
				t.Fatalf("Handle: %v", err)
			}
			if len(svc.calls) != 0 {
				t.Fatalf("non-raw key reached ingest: %+v", svc.calls)
			}
		})
	}
}

func TestRustFSNotifyWorkerRejectsMalformedObjectKeyEncoding(t *testing.T) {
	svc := &fakeIngester{}

	err := testNotifyWorker(svc).Handle(context.Background(), notificationMsg(t, "raw%2"))

	var terminal *Terminal
	if !errors.As(err, &terminal) {
		t.Fatalf("expected a Terminal (non-retryable) error, got: %v", err)
	}
	if len(svc.calls) != 0 {
		t.Fatalf("malformed key reached ingest: %+v", svc.calls)
	}
}

func TestRustFSNotifyWorkerRejectsMalformedPayload(t *testing.T) {
	svc := &fakeIngester{}

	err := testNotifyWorker(svc).Handle(context.Background(), &notifyFakeMsg{data: []byte("{")})

	var terminal *Terminal
	if !errors.As(err, &terminal) {
		t.Fatalf("expected a Terminal (non-retryable) error, got: %v", err)
	}
	if len(svc.calls) != 0 {
		t.Fatalf("malformed payload reached ingest: %+v", svc.calls)
	}
}

func TestRustFSNotifyWorkerReturnsRetryableErrorOnIngestFailure(t *testing.T) {
	key := domain.RawObjectKey(testSHA256)
	svc := &fakeIngester{err: errors.New("postgres unavailable")}

	err := testNotifyWorker(svc).Handle(context.Background(), notificationMsg(t, url.QueryEscape(key)))

	if err == nil {
		t.Fatal("expected an error")
	}
	var terminal *Terminal
	if errors.As(err, &terminal) {
		t.Fatalf("expected a retryable error, got a Terminal one: %v", err)
	}
	if len(svc.calls) != 1 {
		t.Fatalf("expected one ingest attempt, got %d", len(svc.calls))
	}
}

// notifyFakeMsg implements jetstream.Msg with only Data() meaningful — the
// only method RustFSNotifyWorker.Handle calls.
type notifyFakeMsg struct{ data []byte }

func (m *notifyFakeMsg) Data() []byte         { return m.data }
func (m *notifyFakeMsg) Subject() string      { return "rustfs.events.raw" }
func (m *notifyFakeMsg) Reply() string        { return "" }
func (m *notifyFakeMsg) Headers() nats.Header { return nats.Header{} }
func (m *notifyFakeMsg) Metadata() (*jetstream.MsgMetadata, error) {
	return &jetstream.MsgMetadata{NumDelivered: 1}, nil
}
func (m *notifyFakeMsg) Ack() error                       { return nil }
func (m *notifyFakeMsg) DoubleAck(context.Context) error  { return nil }
func (m *notifyFakeMsg) Nak() error                       { return nil }
func (m *notifyFakeMsg) NakWithDelay(time.Duration) error { return nil }
func (m *notifyFakeMsg) InProgress() error                { return nil }
func (m *notifyFakeMsg) Term() error                      { return nil }
func (m *notifyFakeMsg) TermWithReason(string) error      { return nil }
