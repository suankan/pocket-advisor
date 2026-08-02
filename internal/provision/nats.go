package provision

import (
	"bytes"
	"context"
	"fmt"
	"log/slog"
	"strings"
	"time"

	"github.com/nats-io/nats.go"
	"github.com/nats-io/nats.go/jetstream"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/kubernetes/scheme"
	"k8s.io/client-go/rest"
	"k8s.io/client-go/tools/clientcmd"
	"k8s.io/client-go/tools/remotecommand"

	"github.com/suankan/pocket-advisor/internal/config"
)

// natsContainer is the StatefulSet's single container name
// (templates/nats.yaml) — exec targets it explicitly rather than relying on
// a pod's default container.
const natsContainer = "nats"

const (
	natsConfigKey  = "nats-server.conf"
	accountsMarker = "accounts {"
	beginMarker    = "  # BEGIN-WORKSPACE-ACCOUNTS (managed by pocket-advisor --create-workspace)"
	endMarker      = "  # END-WORKSPACE-ACCOUNTS"
)

// createNATS adds the workspace's account and user to the NATS ConfigMap
// and hot-reloads the server to pick it up (workspace-isolation.md §8, §6
// step 3 — last, since it is the riskiest step: everything else has already
// succeeded by the time this runs).
func createNATS(ctx context.Context, cfg *config.Config, id string, log *slog.Logger) error {
	w, err := cfg.Workspace(id)
	if err != nil {
		return err
	}
	if w.NATSPassword == "" {
		return fmt.Errorf("workspace %q has no nats_password in config.yaml", id)
	}
	if err := cfg.RequireProvisioning(); err != nil {
		return err
	}

	clientset, restConfig, err := k8sClient()
	if err != nil {
		return fmt.Errorf("kubernetes client: %w", err)
	}

	cm, err := clientset.CoreV1().ConfigMaps(cfg.Kubernetes.Namespace).
		Get(ctx, cfg.Kubernetes.NATSConfigMap, metav1.GetOptions{})
	if err != nil {
		return fmt.Errorf("get nats configmap: %w", err)
	}

	conf := cm.Data[natsConfigKey]
	if hasAccountBlock(conf, id) {
		log.Info("nats account already present", "workspace_id", id)
		return nil
	}

	conf, err = addAccountBlock(conf, id, w.NATSPassword)
	if err != nil {
		return fmt.Errorf("edit nats config: %w", err)
	}
	if cm.Data == nil {
		cm.Data = map[string]string{}
	}
	cm.Data[natsConfigKey] = conf
	if _, err := clientset.CoreV1().ConfigMaps(cfg.Kubernetes.Namespace).
		Update(ctx, cm, metav1.UpdateOptions{}); err != nil {
		return fmt.Errorf("update nats configmap: %w", err)
	}

	if err := reloadNATS(ctx, clientset, restConfig, cfg, conf, log); err != nil {
		return err
	}
	if err := waitForAccount(ctx, cfg, id); err != nil {
		return fmt.Errorf("confirm nats account live: %w", err)
	}

	log.Info("nats account and user provisioned", "workspace_id", id)
	return nil
}

// deleteNATS removes the workspace's account and user and hot-reloads the
// server (workspace-isolation.md §7, step 1 — first, to stop any new work
// from being enqueuable against this workspace before anything else is torn
// down).
func deleteNATS(ctx context.Context, cfg *config.Config, id string, log *slog.Logger) error {
	if err := cfg.RequireProvisioning(); err != nil {
		return err
	}

	clientset, restConfig, err := k8sClient()
	if err != nil {
		return fmt.Errorf("kubernetes client: %w", err)
	}

	cm, err := clientset.CoreV1().ConfigMaps(cfg.Kubernetes.Namespace).
		Get(ctx, cfg.Kubernetes.NATSConfigMap, metav1.GetOptions{})
	if err != nil {
		return fmt.Errorf("get nats configmap: %w", err)
	}

	conf := cm.Data[natsConfigKey]
	if !hasAccountBlock(conf, id) {
		log.Info("nats account already absent", "workspace_id", id)
		return nil
	}

	// Before the account leaves the config — once it is gone there is no user
	// left to authenticate as, and its streams become unreachable orphans.
	if err := deleteJetStreamAssets(ctx, cfg, id, log); err != nil {
		return err
	}

	conf, err = removeAccountBlock(conf, id)
	if err != nil {
		return fmt.Errorf("edit nats config: %w", err)
	}
	cm.Data[natsConfigKey] = conf
	if _, err := clientset.CoreV1().ConfigMaps(cfg.Kubernetes.Namespace).
		Update(ctx, cm, metav1.UpdateOptions{}); err != nil {
		return fmt.Errorf("update nats configmap: %w", err)
	}

	if err := reloadNATS(ctx, clientset, restConfig, cfg, conf, log); err != nil {
		return err
	}

	log.Info("nats account and user removed", "workspace_id", id)
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

// reloadNATS tells the running NATS process to re-read its config file —
// picking up an added or removed account without dropping any connected
// client's session, unlike deleting the pod. The ConfigMap is mounted as a
// full volume (not subPath, templates/nats.yaml), so Kubernetes already
// syncs the file into the pod on its own; this only has to wait for that
// sync to land — signaling reload against a stale file would silently
// reload nothing new — then tell the process to reload. want is the exact
// config just written to the ConfigMap: an exact byte match is the least
// assumption-laden way to confirm the mount actually caught up, and works
// identically for both an add and a remove.
func reloadNATS(ctx context.Context, clientset *kubernetes.Clientset, restConfig *rest.Config, cfg *config.Config, want string, log *slog.Logger) error {
	podName := cfg.Kubernetes.NATSStatefulSet + "-0"
	namespace := cfg.Kubernetes.Namespace

	deadline := time.Now().Add(2 * time.Minute)
	for {
		out, err := execInPod(ctx, clientset, restConfig, namespace, podName, natsContainer,
			[]string{"cat", "/etc/nats/nats-server.conf"})
		if err == nil && out == want {
			break
		}
		if time.Now().After(deadline) {
			if err != nil {
				return fmt.Errorf("nats-server.conf on pod %s did not sync within 2m: %w", podName, err)
			}
			return fmt.Errorf("nats-server.conf on pod %s did not sync within 2m", podName)
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(2 * time.Second):
		}
	}

	// PID 1 in this container is nats-server itself (its own startup log
	// confirms this: "[1] ... Starting nats-server") — targeting it
	// explicitly rather than relying on --signal reload's own process
	// discovery, which needs pgrep and may not find it in a minimal image.
	if _, err := execInPod(ctx, clientset, restConfig, namespace, podName, natsContainer,
		[]string{"nats-server", "--signal", "reload=1"}); err != nil {
		return fmt.Errorf("signal nats reload: %w", err)
	}
	log.Info("nats config reload signaled", "pod", podName)
	return nil
}

// execInPod runs command inside container of pod and returns its stdout.
func execInPod(ctx context.Context, clientset *kubernetes.Clientset, restConfig *rest.Config,
	namespace, pod, container string, command []string) (string, error) {
	req := clientset.CoreV1().RESTClient().Post().
		Resource("pods").
		Name(pod).
		Namespace(namespace).
		SubResource("exec").
		VersionedParams(&corev1.PodExecOptions{
			Container: container,
			Command:   command,
			Stdout:    true,
			Stderr:    true,
		}, scheme.ParameterCodec)

	exec, err := remotecommand.NewSPDYExecutor(restConfig, "POST", req.URL())
	if err != nil {
		return "", fmt.Errorf("build executor: %w", err)
	}

	var stdout, stderr bytes.Buffer
	if err := exec.StreamWithContext(ctx, remotecommand.StreamOptions{
		Stdout: &stdout,
		Stderr: &stderr,
	}); err != nil {
		return "", fmt.Errorf("exec %v: %w (stderr: %s)", command, err, stderr.String())
	}
	return stdout.String(), nil
}

// waitForAccount polls the monitoring port until the new account is visible,
// so --create-workspace does not report success before NATS actually has it
// (workspace-isolation.md §8, step 5).
// It proves the account is *usable*, not merely named. Polling /accountz for
// the name — what this did originally — is not sufficient and is precisely
// how a broken rebuild reported success: an account whose JetStream store
// survived a previous --delete-workspace reappears in that list immediately,
// while its JetStream never initialises. The name was present in 0.136s, a
// genuinely new account took 11.2s, and every publish afterwards failed
// (§12 deviation 12).
//
// Connecting as the workspace's own user and round-tripping the JetStream API
// tests the thing that actually has to work, over the same path the pipeline
// will use.
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

// hasAccountBlock reports whether id's account block is already present.
func hasAccountBlock(conf, id string) bool {
	return strings.Contains(conf, fmt.Sprintf("%q: {", id))
}

// addAccountBlock inserts id's account block just before the
// END-WORKSPACE-ACCOUNTS marker. Requires both markers already present
// (rendered by the chart even when there are no workspaces yet).
func addAccountBlock(conf, id, password string) (string, error) {
	if !strings.Contains(conf, beginMarker) || !strings.Contains(conf, endMarker) {
		return "", fmt.Errorf("nats-server.conf missing workspace-account markers; was it rendered by an older chart version?")
	}
	block := fmt.Sprintf(
		"  %q: {\n    jetstream: enabled\n    users: [\n      { user: %q, password: %q }\n    ]\n  }\n",
		id, id, password)
	return strings.Replace(conf, endMarker, block+endMarker, 1), nil
}

// removeAccountBlock deletes id's account block, using balanced-brace
// counting rather than line matching — a workspace's block itself contains
// nested braces (the users list), so a naive line-range delete would either
// under- or over-cut.
func removeAccountBlock(conf, id string) (string, error) {
	open := fmt.Sprintf("%q: {", id)
	start := strings.Index(conf, open)
	if start == -1 {
		return "", fmt.Errorf("account %q not found in nats-server.conf", id)
	}
	// Walk back to the start of the line so the leading indentation and any
	// blank line this block owns are removed with it.
	lineStart := strings.LastIndex(conf[:start], "\n") + 1

	depth := 0
	i := start + len(open) - 1 // position of the opening '{'
	end := -1
	for ; i < len(conf); i++ {
		switch conf[i] {
		case '{':
			depth++
		case '}':
			depth--
			if depth == 0 {
				end = i + 1
			}
		}
		if end != -1 {
			break
		}
	}
	if end == -1 {
		return "", fmt.Errorf("account %q: unbalanced braces in nats-server.conf", id)
	}
	// Consume the trailing newline after the closing brace, if present, so
	// removal doesn't leave a blank line behind.
	if end < len(conf) && conf[end] == '\n' {
		end++
	}
	return conf[:lineStart] + conf[end:], nil
}
