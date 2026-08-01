package provision

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"net/http"
	"strings"
	"time"

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
func waitForAccount(ctx context.Context, cfg *config.Config, id string) error {
	host := hostFromNATSURL(cfg.NATS.URL)
	url := fmt.Sprintf("http://%s:%d/accountz", host, cfg.NATS.MonitorPort)

	deadline := time.Now().Add(2 * time.Minute)
	var lastErr error
	for time.Now().Before(deadline) {
		ok, err := accountPresent(ctx, url, id)
		if err == nil && ok {
			return nil
		}
		lastErr = err
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(2 * time.Second):
		}
	}
	return fmt.Errorf("account %q not visible at %s after 2m (last error: %v)", id, url, lastErr)
}

func accountPresent(ctx context.Context, url, id string) (bool, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return false, err
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return false, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return false, fmt.Errorf("accountz returned %d", resp.StatusCode)
	}

	// Confirmed against a live server (workspace-isolation.md §10 item 2):
	// /accountz's actual shape is {"accounts": [...]}, a plain string list —
	// not "account_list", which was an unverified guess that never matched.
	var doc struct {
		Accounts []string `json:"accounts"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&doc); err != nil {
		return false, err
	}
	for _, a := range doc.Accounts {
		if a == id {
			return true, nil
		}
	}
	return false, nil
}

func hostFromNATSURL(u string) string {
	s := strings.TrimPrefix(u, "nats://")
	if i := strings.LastIndex(s, ":"); i > 0 {
		s = s[:i]
	}
	return s
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
