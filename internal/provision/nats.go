package provision

import (
	"context"
	"fmt"
	"log/slog"
	"time"

	"github.com/nats-io/nats.go"
	"github.com/nats-io/nats.go/jetstream"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/rest"
	"k8s.io/client-go/tools/clientcmd"

	"github.com/suankan/pocket-advisor/internal/config"
)

// natsContainer is the StatefulSet's single container name
// (templates/nats.yaml) — exec targets it explicitly rather than relying on
// a pod's default container.
const natsContainer = "nats"

// createNATS verifies the workspace's NATS account is live, rather than
// creating it.
//
// The account itself is rendered by the chart from workspaces/values.yaml and
// applied by `make deploy-infra`. This used to patch the ConfigMap here and
// hot-reload the server, which made Helm and this binary two writers of one
// field: every `helm upgrade` then failed with a field-ownership conflict on
// .data.nats-server.conf, and force-applying Helm's copy — the documented
// recovery — silently discarded every workspace account. Deleting that write
// also deletes the 25-56s this step spent waiting for kubelet to propagate the
// patched file into the pod (ingestion-design.md deviation 18).
//
// What is left is a check with an actionable message, because "account
// missing" now means "values.yaml and the deployed release disagree", which no
// amount of retrying here will fix.
func createNATS(ctx context.Context, cfg *config.Config, id string, log *slog.Logger) error {
	w, err := cfg.Workspace(id)
	if err != nil {
		return err
	}
	if w.NATSPassword == "" {
		return fmt.Errorf("workspace %q has no nats.credentials.password in %s", id, cfg.WorkspacesValuesPath)
	}

	if err := waitForAccount(ctx, cfg, id); err != nil {
		return fmt.Errorf("nats account %q is not live: %w\n"+
			"  the chart renders accounts from %s — run `make deploy-infra` "+
			"after adding a workspace to it", id, err, cfg.WorkspacesValuesPath)
	}

	log.Info("nats account confirmed live", "workspace_id", id, "account", w.NATSAccount)
	return nil
}

// deleteNATS drops the workspace's JetStream assets. The account itself is
// the chart's to remove.
//
// Symmetric with createNATS: since the account block is rendered from
// workspaces/values.yaml, removing it here would only be undone by the next
// `helm upgrade`. Deleting the streams is still this binary's job, and still
// has to happen while the account exists — once it is gone there is no user
// left to authenticate as, and the streams become unreachable orphans that
// poison the next workspace of the same name (deviation 12).
func deleteNATS(ctx context.Context, cfg *config.Config, id string, log *slog.Logger) error {
	if err := deleteJetStreamAssets(ctx, cfg, id, log); err != nil {
		return err
	}
	log.Info("nats jetstream assets removed; account remains until the next helm upgrade",
		"workspace_id", id, "values", cfg.WorkspacesValuesPath)
	return nil
}

// deleteJetStreamAssets removes the workspace's streams through the JetStream
// API, as that workspace's own user, while its account is still in the config.
//
// This has to happen *before* the account is removed, and it has to go through
// the API. Removing the account alone orphans its JetStream assets — NATS does
// not clean them up — and on the next --create-workspace it finds the stale
// store, reports the old streams through /jsz as though they were healthy, and
// never initialises JetStream for the account: every publish then fails with
// "no response from stream" while the run reports a clean scan.
//
// Deleting the store directory instead does not work either, and fails in a
// worse way: JetStream holds account state in memory as well as on disk, so
// removing files under a running server desynchronises the two. The streams
// keep appearing in /jsz with no backing files, and the next consumer creation
// dies on `open .../obs/<consumer>/meta.inf.tmp: no such file or directory`.
// Letting JetStream delete its own streams keeps both halves in step
// (§12 deviation 12).
func deleteJetStreamAssets(ctx context.Context, cfg *config.Config, id string, log *slog.Logger) error {
	w, err := cfg.Workspace(id)
	if err != nil {
		return err
	}
	nc, err := nats.Connect(cfg.NATS.URL,
		nats.UserInfo(id, w.NATSPassword),
		nats.MaxReconnects(0),
		nats.Timeout(5*time.Second),
	)
	if err != nil {
		// The account may already be gone from a previous partial teardown.
		// That is the state this function wants to reach, so it is not fatal.
		log.Warn("nats connect for jetstream teardown failed; skipping",
			"workspace_id", id, "error", err)
		return nil
	}
	defer nc.Close()

	js, err := jetstream.New(nc)
	if err != nil {
		return fmt.Errorf("jetstream context: %w", err)
	}

	listCtx, cancel := context.WithTimeout(ctx, 15*time.Second)
	defer cancel()

	var deleted int
	names := js.StreamNames(listCtx)
	for name := range names.Name() {
		if err := js.DeleteStream(listCtx, name); err != nil {
			return fmt.Errorf("delete stream %s: %w", name, err)
		}
		deleted++
	}
	if err := names.Err(); err != nil {
		return fmt.Errorf("list streams: %w", err)
	}

	log.Info("nats jetstream streams deleted", "workspace_id", id, "streams", deleted)
	return nil
}

// k8sClient uses the operator's own ambient kubeconfig — the same context
// kubectl/helm already use throughout this project — rather than a
// dedicated ServiceAccount. This is a single-user local cluster; see
// workspace-isolation.md §8, §10 item 3. Returns the *rest.Config alongside
// the typed Clientset because reloadNATS's exec transport needs it directly
// — the typed Clientset alone can't build the SPDY upgrade.
func k8sClient() (*kubernetes.Clientset, *rest.Config, error) {
	loadingRules := clientcmd.NewDefaultClientConfigLoadingRules()
	overrides := &clientcmd.ConfigOverrides{}
	restConfig, err := clientcmd.NewNonInteractiveDeferredLoadingClientConfig(loadingRules, overrides).ClientConfig()
	if err != nil {
		return nil, nil, fmt.Errorf("load kubeconfig: %w", err)
	}
	cs, err := kubernetes.NewForConfig(restConfig)
	if err != nil {
		return nil, nil, err
	}
	return cs, restConfig, nil
}

func waitForAccount(ctx context.Context, cfg *config.Config, id string) error {
	w, err := cfg.Workspace(id)
	if err != nil {
		return err
	}

	deadline := time.Now().Add(2 * time.Minute)
	var lastErr error
	for {
		lastErr = probeJetStream(ctx, cfg.NATS.URL, id, w.NATSPassword)
		if lastErr == nil {
			return nil
		}
		if time.Now().After(deadline) {
			return fmt.Errorf("jetstream not usable for account %q after 2m: %w", id, lastErr)
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(2 * time.Second):
		}
	}
}

// probeJetStream connects as the workspace's user and asks JetStream for its
// account info — a real $JS.API round trip. A zombie account accepts the
// connection and then never answers, which is exactly what this catches.
func probeJetStream(ctx context.Context, url, user, password string) error {
	nc, err := nats.Connect(url,
		nats.UserInfo(user, password),
		nats.MaxReconnects(0),
		nats.Timeout(5*time.Second),
	)
	if err != nil {
		return fmt.Errorf("connect as %q: %w", user, err)
	}
	defer nc.Close()

	js, err := jetstream.New(nc)
	if err != nil {
		return fmt.Errorf("jetstream context: %w", err)
	}

	probeCtx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	if _, err := js.AccountInfo(probeCtx); err != nil {
		return fmt.Errorf("jetstream account info: %w", err)
	}
	return nil
}
