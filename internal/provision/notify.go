package provision

import (
	"context"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"time"

	"github.com/minio/minio-go/v7"
	"github.com/minio/minio-go/v7/pkg/credentials"
	"github.com/minio/minio-go/v7/pkg/notification"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes"

	"github.com/suankan/pocket-advisor/internal/bus"
	"github.com/suankan/pocket-advisor/internal/config"
)

// notifySecretName holds the identity RustFS's notify target authenticates
// with. The chart references it optionally, so its absence simply means no
// notifications rather than a pod that will not start (§5.2).
const notifySecretName = "-rustfs-notify"

// notifyTargetARN is the target the chart declares as _PRIMARY. A bucket rule
// naming this ARN is what actually causes events to publish.
//
// Two things about this string are load-bearing and neither is guessable:
//
//   - The partition is "rustfs", not "minio". RustFS implements MinIO's admin
//     wire protocol almost everywhere, and this is one place the rebrand
//     leaks: it rejects "arn:minio:..." with a message about TargetID format
//     that never mentions the partition.
//   - The target id is lowercase "primary" even though the chart declares the
//     target as RUSTFS_NOTIFY_NATS_*_PRIMARY. RustFS lowercases the env suffix
//     when it registers the target, and the runtime lookup is case-sensitive.
//
// The second one fails silently in the worst way: SetBucketNotification
// validates ARN *shape* only, so an uppercase id is accepted and stored, and
// the mismatch surfaces only per-event, in the RustFS log, as "Matched notify
// target is missing from runtime". Nothing on the S3 side reports it.
const notifyTargetARN = "arn:rustfs:sqs::primary:nats"

// configureNotify points RustFS's bucket-notification target at one workspace
// and enables it on that workspace's bucket.
//
// Only one target exists, because RustFS has one server-wide notify config
// while NATS accounts are isolated per workspace — a target can authenticate
// as exactly one workspace's user. That is not a limitation in practice: every
// mode operates on one workspace, so two are never needed at once (§5.2).
//
// Isolation survives: RustFS connects to NATS *as the workspace user*, never
// as an administrator, so events land in that workspace's own account. The
// bucket rule is set on that workspace's bucket alone.
func configureNotify(ctx context.Context, cfg *config.Config, id string, log *slog.Logger) error {
	w, err := cfg.Workspace(id)
	if err != nil {
		return err
	}
	if w.NATSPassword == "" {
		return fmt.Errorf("workspace %q has no nats.credentials.password in %s", id, cfg.WorkspacesValuesPath)
	}

	// The streams must exist before RustFS is told to publish into them.
	// RustFS resolves its JetStream target at startup, and provisioning
	// creates the NATS *account* but nothing in it — so without this the
	// target names a stream that does not exist yet, and the first events
	// after a restart have nowhere to land. Creating them here also makes a
	// freshly provisioned workspace complete rather than half-ready.
	if err := ensureWorkspaceStreams(ctx, cfg, w.NATSUser, w.NATSPassword); err != nil {
		return fmt.Errorf("ensure streams: %w", err)
	}

	clientset, _, err := k8sClient()
	if err != nil {
		return fmt.Errorf("kubernetes client: %w", err)
	}
	ns := cfg.Kubernetes.Namespace
	secretName := releaseName(cfg) + notifySecretName

	changed, err := writeNotifySecret(ctx, clientset, ns, secretName, id, w.NATSPassword)
	if err != nil {
		return err
	}

	// Restarting is what makes a new identity take effect: RustFS reads the
	// target from its environment at startup, and reconfiguring at runtime is
	// not available — madmin's SetConfigKV crashes the server and loses the
	// change (§5.2). Skipped entirely when the target already names this
	// workspace, so repeated runs cost nothing.
	if changed {
		log.Info("rustfs notify target changed; restarting rustfs", "workspace_id", id)
		if err := restartRustFS(ctx, clientset, cfg, log); err != nil {
			return err
		}
	}

	if err := setBucketNotification(ctx, cfg, id); err != nil {
		return err
	}
	log.Info("rustfs notify target configured", "workspace_id", id, "restarted", changed)
	return nil
}

// ensureWorkspaceStreams creates the workspace's own JetStream streams, as
// that workspace's user. Idempotent — the pipeline calls the same routine at
// startup, and CreateOrUpdateStream is safe to repeat.
func ensureWorkspaceStreams(ctx context.Context, cfg *config.Config, user, password string) error {
	b, err := bus.Connect(ctx, cfg.NATS.URL, user, password)
	if err != nil {
		return err
	}
	defer b.Close()
	return b.EnsureStreams(ctx)
}

// writeNotifySecret reports whether the identity actually changed, so a
// restart happens only when it must.
func writeNotifySecret(ctx context.Context, cs *kubernetes.Clientset,
	ns, name, workspaceID, password string,
) (changed bool, err error) {
	api := cs.CoreV1().Secrets(ns)

	existing, err := api.Get(ctx, name, metav1.GetOptions{})
	switch {
	case apierrors.IsNotFound(err):
		_, err = api.Create(ctx, &corev1.Secret{
			ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: ns},
			StringData: map[string]string{
				"workspace-id":  workspaceID,
				"nats-password": password,
			},
		}, metav1.CreateOptions{})
		if err != nil {
			return false, fmt.Errorf("create notify secret: %w", err)
		}
		return true, nil
	case err != nil:
		return false, fmt.Errorf("read notify secret: %w", err)
	}

	if string(existing.Data["workspace-id"]) == workspaceID &&
		string(existing.Data["nats-password"]) == password {
		return false, nil
	}

	existing.StringData = map[string]string{
		"workspace-id":  workspaceID,
		"nats-password": password,
	}
	if _, err := api.Update(ctx, existing, metav1.UpdateOptions{}); err != nil {
		return false, fmt.Errorf("update notify secret: %w", err)
	}
	return true, nil
}

// restartRustFS deletes the pod and waits for its replacement to be ready.
//
// Deliberately unlike the NATS path, which was moved off restarts precisely
// because they drop every JetStream client mid-flight (deviation 10). RustFS
// is restarted during provisioning, before any upload begins, with nothing
// connected to lose.
func restartRustFS(ctx context.Context, cs *kubernetes.Clientset,
	cfg *config.Config, log *slog.Logger,
) error {
	ns := cfg.Kubernetes.Namespace
	pod := releaseName(cfg) + "-rustfs-0"

	if err := cs.CoreV1().Pods(ns).Delete(ctx, pod, metav1.DeleteOptions{}); err != nil &&
		!apierrors.IsNotFound(err) {
		return fmt.Errorf("restart rustfs: %w", err)
	}

	deadline := time.Now().Add(3 * time.Minute)
	for {
		p, err := cs.CoreV1().Pods(ns).Get(ctx, pod, metav1.GetOptions{})
		if err == nil && p.DeletionTimestamp == nil && podReady(p) {
			log.Info("rustfs ready", "pod", pod)
			return nil
		}
		if time.Now().After(deadline) {
			return fmt.Errorf("rustfs pod %s did not become ready within 3m", pod)
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(2 * time.Second):
		}
	}
}

func podReady(p *corev1.Pod) bool {
	if p.Status.Phase != corev1.PodRunning {
		return false
	}
	for _, c := range p.Status.Conditions {
		if c.Type == corev1.PodReady {
			return c.Status == corev1.ConditionTrue
		}
	}
	return false
}

// setBucketNotification is the runtime half, and needs no restart: it decides
// which bucket publishes to the target. Only this workspace's bucket carries a
// rule, which is what keeps one shared target from leaking another workspace's
// events.
//
// Scoped to raw/ deliberately — extracted/ children are written by the email
// worker itself, and re-ingesting them would loop.
func setBucketNotification(ctx context.Context, cfg *config.Config, id string) error {
	// Short per-attempt timeouts, because this runs immediately after a pod
	// restart and the first attempt is expected to fail. With minio-go's
	// default transport that attempt blocks ~30s on a TCP connect to the dead
	// pod's address, so the retry loop below never got to retry — the whole
	// step took a consistent 33s even though a real S3 call succeeds 7s after
	// the restart. Failing fast is what turns the loop into a loop.
	transport := &http.Transport{
		DialContext:           (&net.Dialer{Timeout: 3 * time.Second}).DialContext,
		ResponseHeaderTimeout: 5 * time.Second,
	}
	c, err := minio.New(cfg.RustFS.Endpoint, &minio.Options{
		Creds:     credentials.NewStaticV4(cfg.RustFS.RootAccessKey, cfg.RustFS.RootSecretKey, ""),
		Secure:    cfg.RustFS.UseSSL,
		Transport: transport,
	})
	if err != nil {
		return fmt.Errorf("rustfs admin client: %w", err)
	}

	arn, err := notification.NewArnFromString(notifyTargetARN)
	if err != nil {
		return fmt.Errorf("notify target arn %q: %w", notifyTargetARN, err)
	}
	queue := notification.NewConfig(arn)
	queue.AddEvents(notification.ObjectCreatedAll)
	queue.AddFilterPrefix("raw/")

	conf := notification.Configuration{}
	conf.AddQueue(queue)

	// Retried because this call routinely follows a pod restart. The pod is
	// Ready, but the Service's endpoint list and the host's route to the
	// ClusterIP converge a moment later, so the first attempts fail with an
	// i/o timeout against an address that is about to work.
	deadline := time.Now().Add(90 * time.Second)
	for attempt := 1; ; attempt++ {
		err := c.SetBucketNotification(ctx, id, conf)
		if err == nil {
			return nil
		}
		if time.Now().After(deadline) {
			return fmt.Errorf("set bucket notification on %s (after %d attempts): %w",
				id, attempt, err)
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(3 * time.Second):
		}
	}
}

// releaseName derives the Helm release from the configured StatefulSet name,
// which the chart builds as "<release>-nats". Avoids a second config key that
// could drift from the one already required.
func releaseName(cfg *config.Config) string {
	name := cfg.Kubernetes.NATSStatefulSet
	if len(name) > len("-nats") && name[len(name)-len("-nats"):] == "-nats" {
		return name[:len(name)-len("-nats")]
	}
	return name
}

// deleteNotifySecret removes the notify identity when it names this workspace.
//
// Nothing else cleans it up. The secret is written through the Kubernetes API
// rather than by the chart, so it is not a release resource and `helm
// uninstall` leaves it — it was found surviving a full destroy-infra plus
// destroy-state, still holding a deleted workspace's NATS password. On a
// rebuilt cluster that stale identity is what RustFS would authenticate with.
//
// Left alone when it points at a different workspace: that one is still using
// it, and deleting another workspace must not silently disable its notify.
func deleteNotifySecret(ctx context.Context, cfg *config.Config, id string, log *slog.Logger) error {
	clientset, _, err := k8sClient()
	if err != nil {
		return fmt.Errorf("kubernetes client: %w", err)
	}
	api := clientset.CoreV1().Secrets(cfg.Kubernetes.Namespace)
	name := releaseName(cfg) + notifySecretName

	s, err := api.Get(ctx, name, metav1.GetOptions{})
	if apierrors.IsNotFound(err) {
		return nil
	}
	if err != nil {
		return fmt.Errorf("read notify secret: %w", err)
	}
	if active := string(s.Data["workspace-id"]); active != id {
		log.Info("leaving rustfs notify target in place; it points elsewhere",
			"workspace_id", id, "target_workspace_id", active)
		return nil
	}

	if err := api.Delete(ctx, name, metav1.DeleteOptions{}); err != nil &&
		!apierrors.IsNotFound(err) {
		return fmt.Errorf("delete notify secret: %w", err)
	}
	log.Info("rustfs notify target removed", "workspace_id", id)
	return nil
}

// ActiveNotifyWorkspace reports which workspace RustFS's notify target
// currently authenticates as, or "" if no target has been configured.
//
// Exists because the target is server-wide while the streams it feeds are
// per-account: pointing it at workspace A and then ingesting workspace B
// delivers B's events into A's account, leaving B's RUSTFS_EVENTS empty. The
// pipeline cannot tell that apart from "no uploads yet" — it simply waits, and
// a silent wait is exactly the failure this whole change set set out to remove.
//
// Read-only and best-effort: callers that merely want to warn should treat an
// error as unknown rather than fatal, so a missing kubeconfig degrades the
// check instead of blocking ingestion.
func ActiveNotifyWorkspace(ctx context.Context, cfg *config.Config) (string, error) {
	clientset, _, err := k8sClient()
	if err != nil {
		return "", fmt.Errorf("kubernetes client: %w", err)
	}
	s, err := clientset.CoreV1().Secrets(cfg.Kubernetes.Namespace).
		Get(ctx, releaseName(cfg)+notifySecretName, metav1.GetOptions{})
	if apierrors.IsNotFound(err) {
		return "", nil
	}
	if err != nil {
		return "", fmt.Errorf("read notify secret: %w", err)
	}
	return string(s.Data["workspace-id"]), nil
}
