#!/bin/sh

set -u

BIN=./bin/pocket-advisor
PKG=./cmd/pocket-advisor
RELEASE=pocket-advisor
CHART=./charts/pocket-advisor-infra

TMPFS=$(mktemp -d)
trap 'rm -rf "$TMPFS"' EXIT


METRICS_SERVER_NAMESPACE="kube-system"

POCKET_ADVISOR_NAMESPACE=pocket-advisor

# OCR is a CGo build against Homebrew's tesseract and leptonica. Without the
# tag the binary still builds and runs, but scanned PDFs and images are
# recorded SKIPPED rather than indexed — so it is the default, not an extra.
TAGS=${TAGS:-ocr}

# The toolchain is pinned in mise.toml, which also carries CGO_ENABLED and the
# tesseract include/library paths. Route through mise rather than trusting the
# ambient PATH: Homebrew's mise only auto-activates for fish, so a zsh shell
# that never ran `mise activate` has no `go` at all — and one that has some
# other `go` would build without the pinned CGo settings, which fails at link
# time or, worse, silently drops OCR.
if command -v mise >/dev/null 2>&1; then
  GO="mise exec -- go"
  GOFMT="mise exec -- gofmt"
else
  GO="go"
  GOFMT="gofmt"
fi

usage() {
  cat <<EOF
usage: ./pocket-advisor.sh <command> [args]

  build                     go build -> $BIN
  test                      go test ./...
  race                      go test -race ./...
  vet                       go vet ./...
  fmt                       gofmt -l -w .
  lint                      fmt, vet, helm lint, chart-render assertion
  install-hooks             enable this clone's versioned Git hooks
  docker-build-postgres     build the local Postgres image
  deploy-infra              helm install/upgrade the shared store, then deploy-workspaces
  destroy-infra             helm uninstall (PVCs retained)
  destroy-state             delete every PVC in $POCKET_ADVISOR_NAMESPACE (irreversible)
  deploy-metrics-server     helm install metrics-server into $METRICS_SERVER_NAMESPACE
  destroy-metrics-server    helm uninstall metrics-server
  deploy-workspaces         provision every workspace in workspaces/workspaces.yaml
  destroy-workspace <id>    tear down one workspace's database and role
  clean                     rm -rf bin
EOF
}

cmd_build() {
  $GO build -tags "$TAGS" -o "$BIN" "$PKG"
}

cmd_test() {
  $GO test -tags "$TAGS" ./...
}

cmd_race() {
  $GO test -tags "$TAGS" -race -count=1 ./...
}

cmd_vet() {
  $GO vet -tags "$TAGS" ./...
}

cmd_fmt() {
  $GOFMT -l -w .
}

cmd_lint() {
  cmd_fmt || exit 1
  cmd_vet || exit 1
  helm lint "$CHART" || exit 1
  # PostgreSQL is the only chart workload; the job queue lives inside it.
  helm template "$RELEASE" "$CHART" \
     > "$TMPFS/infra-lint.yaml" \
    && [ "$(grep -c 'kind: StatefulSet' "$TMPFS/infra-lint.yaml")" = "1" ] \
    && echo "ok: chart renders one StatefulSet" \
    || { echo "FAIL: rendered chart is incomplete"; exit 1; }
}

cmd_install_hooks() {
  git config --local core.hooksPath .githooks
  echo "Git hooks enabled from .githooks"
}

resolve_infra() {
  CONFIG_ENVSUBSTED=$TMPFS/config.yaml
  envsubst < config.yaml > "$CONFIG_ENVSUBSTED"

  POSTGRES_HOST=$(yq '.infra.postgres.host' "$CONFIG_ENVSUBSTED")
  POSTGRES_PORT=$(yq '.infra.postgres.port' "$CONFIG_ENVSUBSTED")
  POSTGRES_ADMIN_USER=postgres

  if [ -z "$POSTGRES_HOST" ] || [ "$POSTGRES_HOST" = null ]; then
    echo "missing infra.postgres.host in $CONFIG_ENVSUBSTED" >&2; exit 1
  fi
  if [ -z "$POSTGRES_PORT" ] || [ "$POSTGRES_PORT" = null ]; then
    echo "missing infra.postgres.port in $CONFIG_ENVSUBSTED" >&2; exit 1
  fi
}

# Built fresh on every deploy rather than cached and rebuilt by hand: the
# Dockerfile pins pg_textsearch's version, so a stale local image can only
# mean stock ghcr.io/cloudnative-pg/postgresql was pulled since, not that the
# extension changed underneath anyone.
cmd_docker_build_postgres() {
  docker build --platform linux/arm64 \
    -t "$(yq '.postgres.image' "${CHART}/values.yaml")" docker-images/postgres
}

cmd_deploy_infra() {
  cmd_docker_build_postgres || exit 1
  if [ ! -f workspaces/workspaces.yaml ]; then
    echo "missing workspaces/workspaces.yaml" >&2
    exit 1
  fi
  helm upgrade --install "$RELEASE" "$CHART" --namespace "$POCKET_ADVISOR_NAMESPACE" --create-namespace \
    || exit 1
  kubectl rollout status statefulset/postgres -n "$POCKET_ADVISOR_NAMESPACE" --timeout=5m || exit 1
  report_retired_storage
  echo
  echo "shared stores ready. provisioning every workspace in workspaces/workspaces.yaml..."
  cmd_deploy_workspaces
  echo
  echo "deploy-infra brought up Postgres and every registered workspace. next:"
  echo "  ./pocket-advisor.sh build && ./$BIN --ingest-all --workspace-id <id>"
}

# The job queue used to live in a separate NATS StatefulSet. Helm removes that
# workload when the chart no longer declares it, but never the PersistentVolumeClaim
# a StatefulSet created for itself. Report what is left rather than delete it:
# whether to reclaim that storage is the operator's decision.
report_retired_storage() {
  retired=$(kubectl get pvc -n "$POCKET_ADVISOR_NAMESPACE" -o name 2>/dev/null | grep 'nats-data' || true)
  if [ -n "$retired" ]; then
    echo
    echo "retired NATS storage is still present (nothing deletes it automatically):"
    echo "$retired" | sed 's/^/  /'
    echo "once you no longer need it: kubectl delete pvc -n $POCKET_ADVISOR_NAMESPACE <name>"
  fi
}

cmd_destroy_infra() {
  helm uninstall "$RELEASE" --namespace "$POCKET_ADVISOR_NAMESPACE" --ignore-not-found
  echo
  echo "release removed. PVCs kept — './pocket-advisor.sh destroy-state' also wipes those."
}

cmd_destroy_state() {
  kubectl delete pvc --all --namespace "$POCKET_ADVISOR_NAMESPACE" --ignore-not-found
}

# metrics-server was never a dependency of $CHART even when it was still
# declared inside it — nothing here needs it, only kubectl top does — so its
# lifecycle is independent: its own release, its own upstream chart, added
# as a plain `helm repo` rather than a Chart.yaml dependency since nothing
# else in this repository shares a release with it.
cmd_deploy_metrics_server() {
  helm repo add metrics-server https://kubernetes-sigs.github.io/metrics-server/ >/dev/null
  helm repo update metrics-server >/dev/null
  helm upgrade --install metrics-server metrics-server/metrics-server \
    --version 3.13.1 \
    --namespace "$METRICS_SERVER_NAMESPACE" \
    --set args={--kubelet-insecure-tls} || exit 1
  echo "metrics-server installed. kubectl top nodes / kubectl top pods"
}

cmd_destroy_metrics_server() {
  helm uninstall metrics-server --namespace "$METRICS_SERVER_NAMESPACE" --ignore-not-found
}

provision_workspace() {
  id=$1

  echo "--- postgres: role (idempotent — handles a fresh workspace, one already under this convention, and one still under the pre-convention <id>_user name alike) ---"
  psql -h "$POSTGRES_HOST" -p "$POSTGRES_PORT" -U "$POSTGRES_ADMIN_USER" -d postgres -v ON_ERROR_STOP=1 \
    -c "DO \$\$ BEGIN
          IF NOT EXISTS (SELECT FROM pg_roles WHERE rolname = '$id') THEN
            IF EXISTS (SELECT FROM pg_roles WHERE rolname = '${id}_user') THEN
              ALTER ROLE \"${id}_user\" RENAME TO \"$id\";
            ELSE
              CREATE ROLE \"$id\" LOGIN;
            END IF;
          END IF;
        END \$\$;" \
    || exit 1

  echo "--- postgres: database (CREATE DATABASE has no IF NOT EXISTS; tolerated here on repeat provisioning) ---"
  psql -h "$POSTGRES_HOST" -p "$POSTGRES_PORT" -U "$POSTGRES_ADMIN_USER" -d postgres \
    -c "CREATE DATABASE \"$id\" OWNER \"$id\";" \
    >/dev/null 2>&1 || echo "  database $id already exists, skipping"

  echo "--- postgres: extensions (needs the admin's superuser rights — the workspace's own role never has them, deviation 20/39) ---"
  psql -h "$POSTGRES_HOST" -p "$POSTGRES_PORT" -U "$POSTGRES_ADMIN_USER" -d "$id" -v ON_ERROR_STOP=1 \
    -c "CREATE EXTENSION IF NOT EXISTS vector;" \
    -c "CREATE EXTENSION IF NOT EXISTS pg_textsearch;" \
    || exit 1

  echo
  echo "workspace \"$id\" provisioned."
}

# Provisions every workspace listed in workspaces/workspaces.yaml —
# the one authoritative registry, same list deploy-infra already reads to
# populate the chart's workspaces: values. Run standalone to (re-)provision
# after editing the registry, or automatically at the end of deploy-infra.
cmd_deploy_workspaces() {
  if [ ! -f workspaces/workspaces.yaml ]; then
    echo "missing workspaces/workspaces.yaml" >&2
    exit 1
  fi
  resolve_infra
  for id in $(yq '.workspaces[].id' workspaces/workspaces.yaml); do
    echo
    echo "=== workspace: $id ==="
    provision_workspace "$id"
  done
}

cmd_destroy_workspace() {
  id=${1:-}
  if [ -z "$id" ]; then
    echo "usage: ./pocket-advisor.sh destroy-workspace <workspace_id>" >&2
    exit 1
  fi
  resolve_infra

  echo "--- postgres: dropping database and role (both possible role names — \"\$id\" under the current convention and the pre-convention \"\${id}_user\" — so this cleans up regardless of which one a workspace happens to be under) ---"
  psql -h "$POSTGRES_HOST" -p "$POSTGRES_PORT" -U "$POSTGRES_ADMIN_USER" -d postgres -v ON_ERROR_STOP=1 \
    -c "SELECT pg_terminate_backend(pid) FROM pg_stat_activity WHERE datname = '$id';" \
    -c "DROP DATABASE IF EXISTS \"$id\";" \
    -c "DROP ROLE IF EXISTS \"$id\";" \
    -c "DROP ROLE IF EXISTS \"${id}_user\";" \
    || exit 1

  echo
  echo "workspace \"$id\" torn down. Remove its entry from workspaces/workspaces.yaml by hand."
}

cmd_clean() {
  rm -rf bin
}

cmd=${1:-}
if [ $# -gt 0 ]; then
  shift
fi

case "$cmd" in
  "") cmd_build ;;
  help|-h|--help) usage ;;
  all) cmd_build ;;
  build) cmd_build ;;
  test) cmd_test ;;
  race) cmd_race ;;
  vet) cmd_vet ;;
  fmt) cmd_fmt ;;
  lint) cmd_lint ;;
  install-hooks) cmd_install_hooks ;;
  docker-build-postgres) cmd_docker_build_postgres ;;
  deploy-infra) cmd_deploy_infra ;;
  destroy-infra) cmd_destroy_infra ;;
  destroy-state) cmd_destroy_state ;;
  deploy-metrics-server) cmd_deploy_metrics_server ;;
  destroy-metrics-server) cmd_destroy_metrics_server ;;
  deploy-workspaces) cmd_deploy_workspaces ;;
  destroy-workspace) cmd_destroy_workspace "${1:-}" ;;
  clean) cmd_clean ;;
  *)
    echo "unknown command: $cmd" >&2
    usage >&2
    exit 1
    ;;
esac
