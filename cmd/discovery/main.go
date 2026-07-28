// Command discovery is the sole entry point to the pipeline and the only
// component that mints root documents (ingestion-design.md §5.2).
//
//	discovery --mode=serve      HTTP + bucket notification consumer
//	discovery --mode=scan       exact reconciliation of raw/ against Tier 2
//	discovery --mode=reconcile  re-publish documents stuck PENDING
package main

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"log/slog"
	"net/http"
	"net/url"
	"os"
	"time"

	"github.com/suankan/pocket-advisor/internal/app"
	"github.com/suankan/pocket-advisor/internal/discovery"
	"github.com/suankan/pocket-advisor/internal/domain"
	"github.com/suankan/pocket-advisor/internal/telemetry"
)

func main() {
	var (
		mode        = flag.String("mode", "serve", "serve | scan | reconcile")
		workspace   = flag.String("workspace", "", "workspace id (required for scan)")
		highWater   = flag.Uint64("high-water", 10_000, "pause the scan above this many pending messages")
		lowWater    = flag.Uint64("low-water", 2_000, "resume the scan below this many pending messages")
		stalePeriod = flag.Duration("stale-after", 30*time.Minute, "PENDING age that counts as stalled")
		httpPort    = flag.Int("http-port", 8080, "ingest API port (serve mode)")
	)
	flag.Parse()

	a, err := app.New("discovery", app.Needs{MinIO: true, Postgres: true, NATS: true})
	if err != nil {
		fmt.Fprintf(os.Stderr, "discovery: %v\n", err)
		os.Exit(1)
	}
	defer a.Close()

	svc := &discovery.Service{Vault: a.Vault, Docs: a.Docs, Bus: a.Bus, Log: a.Log}

	switch *mode {
	case "scan":
		if *workspace == "" {
			fmt.Fprintln(os.Stderr, "discovery: --workspace is required for --mode=scan")
			os.Exit(1)
		}
		n, err := svc.Scan(a.Ctx, *workspace, *highWater, *lowWater)
		if err != nil {
			a.Log.Error("scan failed", "error", err, "ingested", n)
			os.Exit(1)
		}
		a.Log.Info("scan complete", "ingested", n)
		fmt.Printf("ingested=%d\n", n)

	case "reconcile":
		n, err := reconcile(a, svc, *stalePeriod)
		if err != nil {
			a.Log.Error("reconcile failed", "error", err)
			os.Exit(1)
		}
		fmt.Printf("republished=%d\n", n)

	case "serve":
		serve(a, svc, *httpPort)

	default:
		fmt.Fprintf(os.Stderr, "discovery: unknown mode %q\n", *mode)
		os.Exit(1)
	}
}

// reconcile re-publishes documents whose stub committed but whose command
// never reached NATS.
//
// Safe to run repeatedly because doc_id is deterministic and every worker is
// idempotent on it: a duplicate delivery redoes work, it does not create a
// second document (§2.2).
func reconcile(a *app.App, svc *discovery.Service, stale time.Duration) (int, error) {
	docs, err := a.Docs.ClaimStalePending(a.Ctx, stale, 500)
	if err != nil {
		return 0, err
	}
	telemetry.DiscoveryStalePending.Set(float64(len(docs)))
	a.Log.Info("stale pending found", "count", len(docs))

	n := 0
	for _, d := range docs {
		key, err := a.Vault.KeyFromURI(d.RawURI)
		if err != nil {
			a.Log.Error("bad raw uri", "doc_id", d.DocID, "uri", d.RawURI, "error", err)
			continue
		}
		if err := svc.Ingest(a.Ctx, d.WorkspaceID, key, "reconcile"); err != nil {
			a.Log.Error("republish failed", "doc_id", d.DocID, "error", err)
			continue
		}
		n++
	}
	remaining, _ := a.Docs.CountStalePending(a.Ctx, stale)
	telemetry.DiscoveryStalePending.Set(float64(remaining))
	a.Log.Info("reconcile complete", "republished", n, "remaining", remaining)
	return n, nil
}

// serve exposes the ingest API and the bucket-notification hook.
func serve(a *app.App, svc *discovery.Service, port int) {
	mux := http.NewServeMux()

	// RustFS bucket notification: the live path. A dropped event costs a delay
	// rather than a document, because the scan is an exact backstop (§5.2).
	mux.Handle("/v1/notify", newNotifyHandler(a.Log, svc))

	mux.HandleFunc("/healthz", func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
	})

	srv := &http.Server{
		Addr:              fmt.Sprintf(":%d", port),
		Handler:           mux,
		ReadHeaderTimeout: 5 * time.Second,
	}
	go func() {
		<-a.Ctx.Done()
		_ = srv.Close()
	}()

	a.Log.Info("discovery serving", "http_port", port)
	if err := srv.ListenAndServe(); err != nil && err != http.ErrServerClosed {
		a.Log.Error("http server stopped", "error", err)
	}
}

type notifyIngester interface {
	Ingest(context.Context, string, string, string) error
}

type bucketNotification struct {
	Records []struct {
		S3 struct {
			Object struct {
				Key string `json:"key"`
			} `json:"object"`
		} `json:"s3"`
	} `json:"Records"`
}

func newNotifyHandler(log *slog.Logger, svc notifyIngester) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			w.Header().Set("Allow", http.MethodPost)
			http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
			return
		}

		var event bucketNotification
		if err := json.NewDecoder(r.Body).Decode(&event); err != nil {
			telemetry.DiscoveryFiles.WithLabelValues("notify", "malformed").Inc()
			http.Error(w, "invalid notification payload", http.StatusBadRequest)
			return
		}

		for _, rec := range event.Records {
			key, err := url.QueryUnescape(rec.S3.Object.Key)
			if err != nil {
				telemetry.DiscoveryFiles.WithLabelValues("notify", "malformed").Inc()
				http.Error(w, "invalid notification object key", http.StatusBadRequest)
				return
			}
			workspaceID, _, err := domain.ParseRawObjectKey(key)
			if err != nil {
				// The bucket rule can only filter on workspaces/, so extracted/
				// child writes arrive here too. They are owned by the worker
				// that created them and must never mint root documents.
				telemetry.DiscoveryFiles.WithLabelValues("notify", "ignored").Inc()
				continue
			}
			if err := svc.Ingest(r.Context(), workspaceID, key, "notify"); err != nil {
				log.Error("notify ingest failed", "key", key, "error", err)
				http.Error(w, "notification ingestion failed", http.StatusServiceUnavailable)
				return
			}
		}
		w.WriteHeader(http.StatusOK)
	})
}
