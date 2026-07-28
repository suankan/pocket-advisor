package main

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"

	"github.com/suankan/pocket-advisor/internal/domain"
)

const testSHA256 = "0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef"

type ingestCall struct {
	workspaceID string
	key         string
	mode        string
}

type fakeNotifyIngester struct {
	err   error
	calls []ingestCall
}

func (f *fakeNotifyIngester) Ingest(_ context.Context, workspaceID, key, mode string) error {
	f.calls = append(f.calls, ingestCall{workspaceID: workspaceID, key: key, mode: mode})
	return f.err
}

func testNotifyHandler(svc notifyIngester) http.Handler {
	return newNotifyHandler(slog.New(slog.NewTextHandler(io.Discard, nil)), svc)
}

func notificationRequest(t *testing.T, method, encodedKey string) *http.Request {
	t.Helper()
	body, err := json.Marshal(map[string]any{
		"Records": []any{
			map[string]any{
				"s3": map[string]any{
					"object": map[string]any{"key": encodedKey},
				},
			},
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	return httptest.NewRequest(method, "/v1/notify", strings.NewReader(string(body)))
}

func TestNotifyHandlerDecodesS3ObjectKey(t *testing.T) {
	workspaces := []string{"matter-one", "matter one", "matter+one"}
	for _, workspaceID := range workspaces {
		t.Run(workspaceID, func(t *testing.T) {
			key := domain.RawObjectKey(workspaceID, testSHA256)
			svc := &fakeNotifyIngester{}
			rec := httptest.NewRecorder()

			testNotifyHandler(svc).ServeHTTP(rec, notificationRequest(t, http.MethodPost, url.QueryEscape(key)))

			if rec.Code != http.StatusOK {
				t.Fatalf("expected 200, got %d: %s", rec.Code, rec.Body.String())
			}
			if len(svc.calls) != 1 {
				t.Fatalf("expected one ingest call, got %d", len(svc.calls))
			}
			want := ingestCall{workspaceID: workspaceID, key: key, mode: "notify"}
			if svc.calls[0] != want {
				t.Fatalf("unexpected ingest call: got %+v want %+v", svc.calls[0], want)
			}
		})
	}
}

func TestNotifyHandlerIgnoresNonRawKeys(t *testing.T) {
	keys := []string{
		domain.ExtractedObjectKey("matter-one", testSHA256),
		"other/prefix/object",
		"workspaces/matter-one/raw/ff/" + testSHA256,
	}
	for _, key := range keys {
		t.Run(key, func(t *testing.T) {
			svc := &fakeNotifyIngester{}
			rec := httptest.NewRecorder()

			testNotifyHandler(svc).ServeHTTP(rec, notificationRequest(t, http.MethodPost, url.QueryEscape(key)))

			if rec.Code != http.StatusOK {
				t.Fatalf("expected 200, got %d: %s", rec.Code, rec.Body.String())
			}
			if len(svc.calls) != 0 {
				t.Fatalf("non-raw key reached ingest: %+v", svc.calls)
			}
		})
	}
}

func TestNotifyHandlerRejectsMalformedObjectKeyEncoding(t *testing.T) {
	svc := &fakeNotifyIngester{}
	rec := httptest.NewRecorder()

	testNotifyHandler(svc).ServeHTTP(rec, notificationRequest(t, http.MethodPost, "workspaces%2"))

	if rec.Code != http.StatusBadRequest {
		t.Fatalf("expected 400, got %d: %s", rec.Code, rec.Body.String())
	}
	if len(svc.calls) != 0 {
		t.Fatalf("malformed key reached ingest: %+v", svc.calls)
	}
}

func TestNotifyHandlerReturnsServiceUnavailableOnIngestFailure(t *testing.T) {
	key := domain.RawObjectKey("matter-one", testSHA256)
	svc := &fakeNotifyIngester{err: errors.New("postgres unavailable")}
	rec := httptest.NewRecorder()

	testNotifyHandler(svc).ServeHTTP(rec, notificationRequest(t, http.MethodPost, url.QueryEscape(key)))

	if rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("expected 503, got %d: %s", rec.Code, rec.Body.String())
	}
	if len(svc.calls) != 1 {
		t.Fatalf("expected one ingest attempt, got %d", len(svc.calls))
	}
}

func TestNotifyHandlerRejectsMalformedPayload(t *testing.T) {
	svc := &fakeNotifyIngester{}
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/v1/notify", strings.NewReader("{"))

	testNotifyHandler(svc).ServeHTTP(rec, req)

	if rec.Code != http.StatusBadRequest {
		t.Fatalf("expected 400, got %d: %s", rec.Code, rec.Body.String())
	}
	if len(svc.calls) != 0 {
		t.Fatalf("malformed payload reached ingest: %+v", svc.calls)
	}
}

func TestNotifyHandlerAllowsOnlyPost(t *testing.T) {
	svc := &fakeNotifyIngester{}
	rec := httptest.NewRecorder()

	testNotifyHandler(svc).ServeHTTP(rec, notificationRequest(t, http.MethodGet, "ignored"))

	if rec.Code != http.StatusMethodNotAllowed {
		t.Fatalf("expected 405, got %d: %s", rec.Code, rec.Body.String())
	}
	if rec.Header().Get("Allow") != http.MethodPost {
		t.Fatalf("expected Allow: POST, got %q", rec.Header().Get("Allow"))
	}
	if len(svc.calls) != 0 {
		t.Fatalf("GET reached ingest: %+v", svc.calls)
	}
}
