#!/bin/sh
# pocket-advisor runs on the host, against the three stores in the local
# cluster. No operators, no CRDs (deviation 39): the chart renders plain
# Deployments/StatefulSets for the three shared stores, and everything
# workspace-specific — a database, a bucket, a set of streams — is
# provisioned by `./pocket-advisor.sh deploy-workspaces`, calling
# psql/rc/aws-cli/natscli directly once per workspace listed in
# workspaces/workspace-config.yaml, the same operation an operator would
# have made but run once by a human instead of continuously by a
# controller. `deploy-infra` runs it automatically after the shared stores
# come up, so a stock `deploy-infra` alone leaves nothing unprovisioned.
#
# Replaces the Makefile entirely, in plain POSIX sh — no GNU Make
# extensions. destroy-workspace still takes a single workspace id as a
# plain positional argument (it is deliberately scoped to one workspace,
# never "all" — see cmd_destroy_workspace). Make's own MAKECMDGOALS trick
# for that collides with any workspace id that also happens to name a real
# target — this repo's own "test" workspace, for instance, would silently
# also run the Go test suite (the make target that happens to be spelled
# the same). A plain positional argument to a shell script has no such
# collision.
#
# config.yaml is a committed *template* holding ${VAR} placeholders for the
# handful of infra fields that still use them (none of them credentials any
# more — see below), expanded into a throwaway copy at the point of use, by
# resolve_infra below.
#
# There is no credentials file and no direnv. This is a fully local,
# single-operator system: administrative credentials for Postgres/RustFS are
# the fixed literal admin/admin (Postgres also drops the password check
# entirely — trust auth, see charts/pocket-advisor-infra/templates/
# postgres.yaml), and every per-workspace name and credential — Postgres
# role, RustFS bucket, NATS subjects/streams — is simply the workspace id
# itself, computed here and in internal/config wherever it's needed, never
# stored anywhere.

set -u

BIN=./bin/pocket-advisor
PKG=./cmd/pocket-advisor
RELEASE=pocket-advisor
CHART=./charts/pocket-advisor-infra

TMPFS=$(mktemp -d)
trap 'rm -rf "$TMPFS"' EXIT

# config.yaml is a template only for the ${VAR} placeholders that remain
# (none are credentials any more). Not expanded here at the top level — only
# deploy-infra, deploy-workspaces and destroy-workspace ever touch Postgres/
# RustFS/NATS, so nothing else (build, test, lint, …) should have to find
# envsubst, yq, or a valid config.yaml just to run. resolve_infra below does
# this lazily, into $TMPFS, which the EXIT trap removes.

# metrics-server is not a dependency of $CHART — it never needed anything
# the removed operators provided, so its lifecycle stays independent,
# installed as its own release from its own upstream chart. kube-system,
# not $POCKET_ADVISOR_NAMESPACE: it's a cluster-wide add-on, not part of pocket-advisor, and the
# chart's own RoleBinding for the metrics API (extension-apiserver-
# authentication-reader) already lives in kube-system regardless — putting
# the rest of the release there too means everything metrics-server owns is
# in one namespace instead of split across two.
METRICS_SERVER_NAMESPACE="kube-system"

# One release, one namespace, holding all three shared stores and every
# workspace's data inside them. Nothing is namespaced per workspace any
# more — deviation 39 removed the last thing that was (JetStream Streams,
# via NACK).
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
  deploy-infra              helm install/upgrade the three shared stores, then deploy-workspaces
  destroy-infra             helm uninstall (PVCs retained)
  destroy-state             delete every PVC in $POCKET_ADVISOR_NAMESPACE (irreversible)
  deploy-metrics-server     helm install metrics-server into $METRICS_SERVER_NAMESPACE
  destroy-metrics-server    helm uninstall metrics-server
  deploy-workspaces         provision every workspace in workspaces/workspace-config.yaml
  destroy-workspace <id>    tear down one workspace's database/bucket/streams
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
  # No more per-workspace rendering to assert on — deviation 39 removed
  # the last workspace-scoped template (Streams). What is left to check is
  # that the chart still renders at all with zero dependencies and that
  # RustFS's per-workspace notify env vars — the one place a workspace id
  # still reaches a template — actually appear.
  helm template "$RELEASE" "$CHART" \
    --set workspaces[0].id=lintws > "$TMPFS/infra-lint.yaml" \
    && grep -q 'RUSTFS_NOTIFY_NATS_ENABLE_LINTWS' "$TMPFS/infra-lint.yaml" \
    && [ "$(grep -c 'kind: StatefulSet' "$TMPFS/infra-lint.yaml")" = "3" ] \
    && echo "ok: chart renders three StatefulSets and the workspace's notify block" \
    || { echo "FAIL: rendered chart is incomplete"; exit 1; }
}

cmd_install_hooks() {
  git config --local core.hooksPath .githooks
  echo "Git hooks enabled from .githooks"
}

# Resolves POSTGRES_HOST, POSTGRES_PORT, RUSTFS_ENDPOINT_URL and NATS_HOST
# from config.yaml's own infra: section, the same file internal/config
# reads, so this script and the Go binary read one address for each store,
# not two that could quietly disagree (deviation 41). POSTGRES_ADMIN_USER
# and the RustFS root credential are the fixed convention (postgres/admin/
# admin — see charts/pocket-advisor-infra/values.yaml), not read from
# anywhere. Called lazily, only by deploy-workspaces/destroy-workspace, the
# only commands that touch Postgres/RustFS/NATS directly.
resolve_infra() {
  CONFIG_ENVSUBSTED=$TMPFS/config.yaml
  envsubst < config.yaml > "$CONFIG_ENVSUBSTED"

  POSTGRES_HOST=$(yq '.infra.postgres.host' "$CONFIG_ENVSUBSTED")
  POSTGRES_PORT=$(yq '.infra.postgres.port' "$CONFIG_ENVSUBSTED")
  RUSTFS_ENDPOINT=$(yq '.infra.rustfs.endpoint' "$CONFIG_ENVSUBSTED")
  rustfs_use_ssl=$(yq '.infra.rustfs.use_ssl' "$CONFIG_ENVSUBSTED")
  if [ "$rustfs_use_ssl" = "true" ]; then
    RUSTFS_ENDPOINT_URL=https://$RUSTFS_ENDPOINT
  else
    RUSTFS_ENDPOINT_URL=http://$RUSTFS_ENDPOINT
  fi
  # infra.nats.url carries the nats:// scheme (internal/config uses it
  # whole); this script only ever needs host:port to build its own
  # nats://host:port URLs below, so the scheme is stripped once here rather
  # than repeated at every call site.
  NUTS_URL=$(yq '.infra.nats.url' "$CONFIG_ENVSUBSTED")
  NATS_HOST=${NATS_HOST:-${NUTS_URL#nats://}}

  POSTGRES_ADMIN_USER=postgres

  if [ -z "$POSTGRES_HOST" ] || [ "$POSTGRES_HOST" = null ]; then
    echo "missing infra.postgres.host in $CONFIG_ENVSUBSTED" >&2; exit 1
  fi
  if [ -z "$POSTGRES_PORT" ] || [ "$POSTGRES_PORT" = null ]; then
    echo "missing infra.postgres.port in $CONFIG_ENVSUBSTED" >&2; exit 1
  fi
  if [ -z "$RUSTFS_ENDPOINT" ] || [ "$RUSTFS_ENDPOINT" = null ]; then
    echo "missing infra.rustfs.endpoint in $CONFIG_ENVSUBSTED" >&2; exit 1
  fi
  if [ -z "$NUTS_URL" ] || [ "$NUTS_URL" = null ]; then
    echo "missing infra.nats.url in $CONFIG_ENVSUBSTED" >&2; exit 1
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

# No operators, no CRDs, so no webhook race to retry past and nothing to wait
# on but the three StatefulSets' own rollout — the whole reason deploy-infra
# used to need a retry loop and two separate `kubectl wait`s per operator is
# gone with the operators (deviation 39).
cmd_deploy_infra() {
  cmd_docker_build_postgres || exit 1
  if [ ! -f workspaces/workspace-config.yaml ]; then
    echo "missing workspaces/workspace-config.yaml" >&2
    exit 1
  fi
  # The chart's workspaces: list only needs an id per entry now (no
  # credentials survive anywhere) — derived straight from the one
  # authoritative registry, not a second credentials-only file.
  set --
  i=0
  for id in $(yq '.workspaces[].id' workspaces/workspace-config.yaml); do
    set -- "$@" --set "workspaces[$i].id=$id"
    i=$((i + 1))
  done
  helm upgrade --install "$RELEASE" "$CHART" --namespace "$POCKET_ADVISOR_NAMESPACE" --create-namespace \
    "$@" || exit 1
  for sts in postgres rustfs nats; do
    kubectl rollout status statefulset/"$sts" -n "$POCKET_ADVISOR_NAMESPACE" --timeout=5m || exit 1
  done
  echo
  echo "shared stores ready. provisioning every workspace in workspaces/workspace-config.yaml..."
  cmd_deploy_workspaces
  echo
  echo "deploy-infra brought up Postgres, RustFS, NATS, and every registered workspace. next:"
  echo "  ./pocket-advisor.sh build && ./$BIN --ingest-all --workspace-id <id>"
}

# No PVC workaround needed any more, unlike the CloudNativePG era this
# replaced: a plain StatefulSet's PVCs survive helm uninstall by Kubernetes'
# own ordinary default, the same as NATS's always did. CloudNativePG's
# operator was the one thing that actively deleted them regardless of that
# default (deviation 38) — removing the operator removed the problem, not
# just this script's workaround for it.
cmd_destroy_infra() {
  helm uninstall "$RELEASE" --namespace "$POCKET_ADVISOR_NAMESPACE" --ignore-not-found
  echo
  echo "release removed. PVCs kept — './pocket-advisor.sh destroy-state' also wipes those."
}

# Deliberately separate from destroy-infra, same as before: this deletes the
# corpus and the JetStream/Postgres state behind it, not just the workloads
# that mount them. One namespace now, not a namespace per workspace plus the
# release namespace — deviation 39 removed the per-workspace ones entirely,
# so there is nothing left to enumerate by label and nothing to wait on
# terminating.
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

# The whole of what an operator used to reconcile for one workspace, run
# once instead of continuously: a Postgres database and role, a RustFS
# bucket and its public policy, and three JetStream streams. Idempotent
# throughout — re-running against an already-provisioned workspace skips or
# safely repeats what exists rather than failing on it (the Postgres role
# step is a single DO block covering fresh, already-migrated, and
# pre-convention workspaces alike; put-bucket-policy overwrites cleanly on a
# second run). Takes one workspace id and assumes resolve_infra has already
# run — cmd_deploy_workspaces below is the only caller, looping this over
# every id in the registry.
#
# aws-cli for everything RustFS-side now (bucket, its public policy, the
# notification binding) — the per-workspace identity/IAM-policy layer `rc
# admin user`/`rc admin policy` used to own is gone entirely.
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

  echo "--- postgres: database (CREATE DATABASE has no IF NOT EXISTS; tolerated here the same way bucket/policy creation below is) ---"
  psql -h "$POSTGRES_HOST" -p "$POSTGRES_PORT" -U "$POSTGRES_ADMIN_USER" -d postgres \
    -c "CREATE DATABASE \"$id\" OWNER \"$id\";" \
    >/dev/null 2>&1 || echo "  database $id already exists, skipping"

  echo "--- postgres: extensions (needs the admin's superuser rights — the workspace's own role never has them, deviation 20/39) ---"
  psql -h "$POSTGRES_HOST" -p "$POSTGRES_PORT" -U "$POSTGRES_ADMIN_USER" -d "$id" -v ON_ERROR_STOP=1 \
    -c "CREATE EXTENSION IF NOT EXISTS vector;" \
    -c "CREATE EXTENSION IF NOT EXISTS pg_textsearch;" \
    || exit 1

  echo "--- rustfs: bucket with a public policy (no per-workspace identity any more — isolation is the bucket name, verified live: an unpolicied bucket still refuses anonymous access) ---"
  rc alias set pa-admin "$RUSTFS_ENDPOINT_URL" admin admin >/dev/null
  AWS_ACCESS_KEY_ID=admin AWS_SECRET_ACCESS_KEY=admin \
    aws s3api create-bucket --bucket "$id" --endpoint-url "$RUSTFS_ENDPOINT_URL" --region us-east-1 >/dev/null 2>&1 || true
  policy_file=$(mktemp)
  printf '{"Version":"2012-10-17","Statement":[{"Effect":"Allow","Principal":"*","Action":["s3:GetObject","s3:PutObject","s3:DeleteObject","s3:ListBucket","s3:ListBucketMultipartUploads","s3:AbortMultipartUpload"],"Resource":["arn:aws:s3:::%s","arn:aws:s3:::%s/*"]}]}' "$id" "$id" > "$policy_file"
  AWS_ACCESS_KEY_ID=admin AWS_SECRET_ACCESS_KEY=admin \
    aws s3api put-bucket-policy --bucket "$id" --policy "file://$policy_file" --endpoint-url "$RUSTFS_ENDPOINT_URL" --region us-east-1 \
    || exit 1
  rm -f "$policy_file"

  echo "--- rustfs: bucket notification binding (raw/ only — extracted/ children are worker output, re-ingesting them would loop; admin-authenticated, bucket administration is deliberately not part of the public policy above) ---"
  arn="arn:rustfs:sqs::$(echo "$id" | tr '-' '_'):nats"
  AWS_ACCESS_KEY_ID=admin AWS_SECRET_ACCESS_KEY=admin \
    aws s3api put-bucket-notification-configuration \
      --endpoint-url "$RUSTFS_ENDPOINT_URL" --region us-east-1 --bucket "$id" \
      --notification-configuration \
        '{"QueueConfigurations":[{"QueueArn":"'"$arn"'","Events":["s3:ObjectCreated:*"],"Filter":{"Key":{"FilterRules":[{"Name":"prefix","Value":"raw/"}]}}}]}' \
    || exit 1

  echo "--- nats: streams (anonymous — no NATS credential exists any more; subjects and stream names are namespaced by workspace id, matching internal/bus/bus.go's identical transform exactly) ---"
  suffix=$(echo "$id" | tr '-' '_' | tr '[:lower:]' '[:upper:]')
  nats stream add "INGESTION_$suffix" -s "nats://$NATS_HOST" \
    --subjects="$id.ingest.emails.raw,$id.ingest.pdfs.raw,$id.ingest.docx.raw,$id.ingest.images.raw,$id.ingest.text.embed" \
    --retention=work --storage=file --discard=new --max-msgs=1000000 \
    --max-msgs-per-subject=-1 --max-bytes=-1 --max-age=-1 --max-msg-size=-1 \
    --dupe-window=2m --no-allow-rollup --deny-delete --no-deny-purge --replicas=1 \
    >/dev/null 2>&1 || echo "  INGESTION_$suffix already exists, skipping"
  nats stream add "INGESTION_DLQ_$suffix" -s "nats://$NATS_HOST" \
    --subjects="$id.ingest.dlq" \
    --retention=limits --storage=file --discard=new --max-age=720h \
    --max-msgs=-1 --max-msgs-per-subject=-1 --max-bytes=-1 --max-msg-size=-1 \
    --dupe-window=2m --no-allow-rollup --deny-delete --no-deny-purge --replicas=1 \
    >/dev/null 2>&1 || echo "  INGESTION_DLQ_$suffix already exists, skipping"
  nats stream add "RUSTFS_EVENTS_$suffix" -s "nats://$NATS_HOST" \
    --subjects="$id.rustfs.events.raw" \
    --retention=work --storage=file --discard=new --max-msgs=1000000 \
    --max-msgs-per-subject=-1 --max-bytes=-1 --max-age=-1 --max-msg-size=-1 \
    --dupe-window=10m --no-allow-rollup --deny-delete --no-deny-purge --replicas=1 \
    >/dev/null 2>&1 || echo "  RUSTFS_EVENTS_$suffix already exists, skipping"

  echo
  echo "workspace \"$id\" provisioned."
}

# Provisions every workspace listed in workspaces/workspace-config.yaml —
# the one authoritative registry, same list deploy-infra already reads to
# populate the chart's workspaces: values. Run standalone to (re-)provision
# after editing the registry, or automatically at the end of deploy-infra.
cmd_deploy_workspaces() {
  if [ ! -f workspaces/workspace-config.yaml ]; then
    echo "missing workspaces/workspace-config.yaml" >&2
    exit 1
  fi
  resolve_infra
  for id in $(yq '.workspaces[].id' workspaces/workspace-config.yaml); do
    echo
    echo "=== workspace: $id ==="
    provision_workspace "$id"
  done
}

# The inverse of provision_workspace, scoped to one workspace rather than
# every registered one, and — unlike destroy-infra/destroy-state —
# a genuinely data-destroying operation scoped to one workspace: drops its
# Postgres database (and everything in it), its RustFS bucket (and every
# object in it), its identity and policy, and its NATS streams. This is the
# scoped cleanup workspace-isolation.md §10 previously listed as missing —
# deviation 39 building it directly, rather than as a follow-up, is what
# closes that gap.
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

  echo "--- rustfs: removing bucket (no per-workspace identity or policy object to remove separately any more — the policy lives on the bucket itself) ---"
  rc alias set pa-admin "$RUSTFS_ENDPOINT_URL" admin admin >/dev/null
  AWS_ACCESS_KEY_ID=admin AWS_SECRET_ACCESS_KEY=admin \
    aws s3 rb "s3://$id" --force --endpoint-url "$RUSTFS_ENDPOINT_URL" --region us-east-1 >/dev/null 2>&1 || true

  echo "--- nats: removing streams (anonymous, namespaced names matching provision_workspace) ---"
  suffix=$(echo "$id" | tr '-' '_' | tr '[:lower:]' '[:upper:]')
  for s in INGESTION INGESTION_DLQ RUSTFS_EVENTS; do
    nats stream rm "${s}_$suffix" -s "nats://$NATS_HOST" --force >/dev/null 2>&1 || true
  done

  echo
  echo "workspace \"$id\" torn down. Remove its entry from workspaces/workspace-config.yaml by hand."
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
