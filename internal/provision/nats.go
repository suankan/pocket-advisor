package provision

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"net/http"
	"strings"
	"time"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/tools/clientcmd"

	"github.com/suankan/pocket-advisor/internal/config"
)

const (
	natsConfigKey  = "nats-server.conf"
	accountsMarker = "accounts {"
	beginMarker    = "  # BEGIN-WORKSPACE-ACCOUNTS (managed by pocket-advisor --create-workspace)"
	endMarker      = "  # END-WORKSPACE-ACCOUNTS"
)

// createNATS adds the workspace's account and user to the NATS ConfigMap
// and restarts the server to pick it up (workspace-isolation.md §8, §6 step
// 3 — last, since it is the riskiest and slowest step: everything else has
// already succeeded by the time this runs).
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

	clientset, err := k8sClient()
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
	} else {
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
	}

	if err := restartNATS(ctx, clientset, cfg, log); err != nil {
		return err
	}
	if err := waitForAccount(ctx, cfg, id); err != nil {
		return fmt.Errorf("confirm nats account live: %w", err)
	}

	log.Info("nats account and user provisioned", "workspace_id", id)
	return nil
}

// deleteNATS removes the workspace's account and user and restarts the
// server (workspace-isolation.md §7, step 1 — first, to stop any new work
// from being enqueuable against this workspace before anything else is torn
// down).
func deleteNATS(ctx context.Context, cfg *config.Config, id string, log *slog.Logger) error {
	if err := cfg.RequireProvisioning(); err != nil {
		return err
	}

	clientset, err := k8sClient()
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

	if err := restartNATS(ctx, clientset, cfg, log); err != nil {
		return err
	}

	log.Info("nats account and user removed", "workspace_id", id)
	return nil
}

// k8sClient uses the operator's own ambient kubeconfig — the same context
// kubectl/helm already use throughout this project — rather than a
// dedicated ServiceAccount. This is a single-user local cluster; see
// workspace-isolation.md §8, §10 item 3.
func k8sClient() (*kubernetes.Clientset, error) {
	loadingRules := clientcmd.NewDefaultClientConfigLoadingRules()
	overrides := &clientcmd.ConfigOverrides{}
	restConfig, err := clientcmd.NewNonInteractiveDeferredLoadingClientConfig(loadingRules, overrides).ClientConfig()
	if err != nil {
		return nil, fmt.Errorf("load kubeconfig: %w", err)
	}
	return kubernetes.NewForConfig(restConfig)
}

// restartNATS deletes the single NATS pod so the StatefulSet recreates it,
// remounting the updated ConfigMap. Simpler and more deterministic than a
// signal-based hot reload, which is unverified for newly-added accounts
// (workspace-isolation.md §8). JetStream data on the PVC survives untouched.
func restartNATS(ctx context.Context, clientset *kubernetes.Clientset, cfg *config.Config, log *slog.Logger) error {
	podName := cfg.Kubernetes.NATSStatefulSet + "-0"
	if err := clientset.CoreV1().Pods(cfg.Kubernetes.Namespace).
		Delete(ctx, podName, metav1.DeleteOptions{}); err != nil && !apierrors.IsNotFound(err) {
		return fmt.Errorf("delete nats pod %s: %w", podName, err)
	}
	log.Info("nats pod restart triggered", "pod", podName)

	deadline := time.Now().Add(2 * time.Minute)
	for time.Now().Before(deadline) {
		pod, err := clientset.CoreV1().Pods(cfg.Kubernetes.Namespace).Get(ctx, podName, metav1.GetOptions{})
		if err == nil && podReady(pod) {
			return nil
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(2 * time.Second):
		}
	}
	return fmt.Errorf("nats pod %s did not become ready within 2m", podName)
}

func podReady(pod *corev1.Pod) bool {
	if pod.Status.Phase != corev1.PodRunning {
		return false
	}
	for _, c := range pod.Status.Conditions {
		if c.Type == corev1.PodReady {
			return c.Status == corev1.ConditionTrue
		}
	}
	return false
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
