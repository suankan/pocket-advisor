#!/bin/sh
# pocket-advisor runs on the host, against the three stores in the local
# cluster. No operators, no CRDs (deviation 39): the chart renders plain
# Deployments/StatefulSets for the three shared stores, and everything
# workspace-specific — a database, a bucket, a set of streams — is
# provisioned by `./pocket-advisor.sh deploy-workspace <id>`, calling
# psql/rc/aws-cli/natscli directly, the same operation an operator would
# have made but run once by a human instead of continuously by a controller.
#
# Replaces the Makefile entirely, in plain POSIX sh — no GNU Make
# extensions, and no WORKSPACE_ID=<id> variable-override syntax:
# deploy-workspace/destroy-workspace take the workspace id as a plain
# positional argument instead. Make's own MAKECMDGOALS trick for that
# collides with any workspace id that also happens to name a real target —
# this repo's own "test" workspace, for instance, would silently also run
# the Go test suite (the make target that happens to be spelled the same).
# A plain positional argument to a shell script has no such collision.
#
# config.yaml and workspaces/pocket-advisor-infra.yaml are committed/
# gitignored *templates* holding ${VAR} placeholders, never literal values;
# every real value lives in .envrc (direnv-loaded, gitignored) and is
# expanded into a throwaway copy at the point of use, by envsubst_workspaces
# and resolve_infra below — one place defines a secret, everything else
# reads it from there.

set -u

BIN=./bin/pocket-advisor
PKG=./cmd/pocket-advisor
RELEASE=pocket-advisor
CHART=./charts/pocket-advisor-infra
APP_CHART=./charts/pocket-advisor-app

TMPFS=$(mktemp -d)
trap 'rm -rf "$TMPFS"' EXIT

# config.yaml and workspaces/pocket-advisor-infra.yaml are templates now,
# not literal values: every secret lives in .envrc (direnv-loaded into the
# environment), and both committed/gitignored files carry only ${VAR}
# placeholders naming which one. Neither is expanded here at the top level —
# only deploy-infra, deploy-workspace and destroy-workspace ever touch
# Postgres/RustFS/NATS, so nothing else (build, test, lint, …) should have
# to find envsubst, yq, a valid .envrc or a valid config.yaml just to run.
# envsubst_workspaces and resolve_infra below do this lazily, into $TMPFS,
# which the EXIT trap removes — there is nothing sensitive in the *template*
# files themselves to protect, only in the expanded copies, and those never
# outlive the process.

# metrics-server is not a dependency of $CHART — it never needed anything
# the removed operators provided, so its lifecycle stays independent,
# installed as its own release from its own upstream chart. kube-system,
# not $POCKET_ADVISOR_NAMESPACE: it's a cluster-wide add-on, not part of pocket-advisor, and the
# chart's own RoleBinding for the metrics API (extension-apiserver-
# authentication-reader) already lives in kube-system regardless — putting
# the rest of the release there too means everything metrics-server owns is
# in one namespace instead of split across two.
METRICS_SERVER_NAMESPACE="kube-system"

# workspaces/pocket-advisor-infra.yaml is the private per-workspace
# template: names and credentials (as ${VAR} placeholders, expanded from
# .envrc) for each workspace's database, bucket and NATS account, plus the
# three admin credentials (Postgres, RustFS, NATS) deploy-workspace/
# destroy-workspace authenticate as. Gitignored, and the same file the
# binary reads (config.yaml's workspaces.values), so Helm and the CLI
# cannot disagree about a password.

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
  docker-build-app          build the application image used by authenticated MCP
  deploy-infra              helm install/upgrade the three shared stores
  destroy-infra             helm uninstall (PVCs retained)
  destroy-state             delete every PVC in $POCKET_ADVISOR_NAMESPACE (irreversible)
  deploy-metrics-server     helm install metrics-server into $METRICS_SERVER_NAMESPACE
  destroy-metrics-server    helm uninstall metrics-server
  deploy-workspace <id>     provision one workspace's database/bucket/streams
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
  helm lint "$APP_CHART" \
    --set workspace.id=synthetic \
    --set workspace.configurationSecret=synthetic-config \
    --set mcp.publicURI=https://mcp.example.test/mcp \
    --set mcp.allowedHosts=mcp.example.test \
    --set oauth.authorizationServer=https://auth.example.test/realms/pocket-advisor \
    --set oauth.introspectionEndpoint=https://auth.example.test/realms/pocket-advisor/protocol/openid-connect/token/introspect \
    --set oauth.introspectionClientID=resource-server \
    --set oauth.secretName=oauth-secret \
    --set tls.secretName=tls-secret || exit 1
  helm template synthetic "$APP_CHART" \
    --set workspace.id=synthetic \
    --set workspace.configurationSecret=synthetic-config \
    --set mcp.publicURI=https://mcp.example.test/mcp \
    --set mcp.allowedHosts=mcp.example.test \
    --set oauth.authorizationServer=https://auth.example.test/realms/pocket-advisor \
    --set oauth.introspectionEndpoint=https://auth.example.test/realms/pocket-advisor/protocol/openid-connect/token/introspect \
    --set oauth.introspectionClientID=resource-server \
    --set oauth.secretName=oauth-secret \
    --set tls.secretName=tls-secret \
    --set 'networkPolicy.egress[0].cidr=192.0.2.10/32' \
    --set 'networkPolicy.egress[0].ports[0]=5432' > "$TMPFS/app-lint.yaml" \
    && grep -q 'reverse_proxy 127.0.0.1:8080' "$TMPFS/app-lint.yaml" \
    && grep -q 'port: 5432' "$TMPFS/app-lint.yaml" \
    && [ "$(grep -c 'kind: Deployment' "$TMPFS/app-lint.yaml")" = "1" ] \
    && echo "ok: authenticated MCP chart renders one loopback-backed deployment" \
    || { echo "FAIL: rendered authenticated MCP chart is incomplete"; exit 1; }
  # No more per-workspace rendering to assert on — deviation 39 removed
  # the last workspace-scoped template (Streams). What is left to check is
  # that the chart still renders at all with zero dependencies and that
  # RustFS's per-workspace notify env vars — the one place a workspace id
  # still reaches a template — actually appear.
  helm template "$RELEASE" "$CHART" \
    --set rustfs.adminRustFSUser=lint \
    --set rustfs.adminRustFSPassword=lint \
    --set postgres.adminPostgresUser=lint \
    --set postgres.adminPostgresPassword=lint \
    --set workspaces[0].id=lintws \
    --set workspaces[0].rustfs.password=lint \
    --set workspaces[0].nats.password=lint \
    --set workspaces[0].postgres.password=lint > "$TMPFS/infra-lint.yaml" \
    && grep -q 'RUSTFS_NOTIFY_NATS_ENABLE_LINTWS' "$TMPFS/infra-lint.yaml" \
    && [ "$(grep -c 'kind: StatefulSet' "$TMPFS/infra-lint.yaml")" = "3" ] \
    && echo "ok: chart renders three StatefulSets and the workspace's notify block" \
    || { echo "FAIL: rendered chart is incomplete"; exit 1; }
}

cmd_docker_build_app() {
  docker build --platform linux/arm64 \
    -t "pocket-advisor:local" -f docker-images/app/Dockerfile .
}

cmd_install_hooks() {
  git config --local core.hooksPath .githooks
  echo "Git hooks enabled from .githooks"
}

# Expands ${VAR} scalars in the private values template into a throwaway copy
# under $TMPFS. This deliberately uses yq rather than envsubst: passwords can
# contain quotes, colons or comment markers, which textual substitution can
# turn into invalid YAML (or a different value). yq parses the placeholders
# first, substitutes the environment value as a string, then serialises safe
# YAML. Needed by deploy-infra (passed to helm with -f) and by
# deploy-workspace/destroy-workspace; nothing else touches it.
envsubst_workspaces() {
  POCKET_ADVISOR_HELM_VALUES_ENVSUBSTED=$TMPFS/pocket-advisor-infra.yaml
  expression='.'
  for name in $(rg -o '\$\{[A-Za-z_][A-Za-z0-9_]*\}' workspaces/pocket-advisor-infra.yaml | sed 's/.*${//; s/}//' | sort -u); do
    if ! printenv "$name" >/dev/null; then
      echo "unset environment variable $name required by workspaces/pocket-advisor-infra.yaml" >&2
      exit 1
    fi
    expression="$expression | (.. | select(tag == \"!!str\" and . == \"\${$name}\")) = strenv($name)"
  done
  yq "$expression" workspaces/pocket-advisor-infra.yaml > "$POCKET_ADVISOR_HELM_VALUES_ENVSUBSTED"
}

# Resolves POSTGRES_HOST, POSTGRES_PORT, RUSTFS_ENDPOINT_URL, NATS_HOST and
# POSTGRES_ADMIN_USER — the first four from config.yaml's own infra:
# section, the same file internal/config reads, so this script and the Go
# binary read one address for each store, not two that could quietly
# disagree (deviation 41); POSTGRES_ADMIN_USER from
# $POCKET_ADVISOR_HELM_VALUES_ENVSUBSTED instead, deliberately not from
# charts/pocket-advisor-infra/values.yaml's own committed default — the
# values file's default is what applies only when workspaces/
# pocket-advisor-infra.yaml doesn't override it, and it does, so reading
# the chart's own default here could name a role that was never actually
# created if the two ever drifted. Called lazily, only by deploy-workspace/
# destroy-workspace, the only commands that touch Postgres/RustFS/NATS
# directly — callers must run envsubst_workspaces first, since this reads
# its output.
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
  # nats://<workspace>:<password>@host:port URLs below, so the scheme is
  # stripped once here rather than repeated at every call site.
  NUTS_URL=$(yq '.infra.nats.url' "$CONFIG_ENVSUBSTED")
  NATS_HOST=${NATS_HOST:-${NUTS_URL#nats://}}

  POSTGRES_ADMIN_USER=$(yq '.postgres.adminPostgresUser' "$POCKET_ADVISOR_HELM_VALUES_ENVSUBSTED")

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
  if [ -z "$POSTGRES_ADMIN_USER" ] || [ "$POSTGRES_ADMIN_USER" = null ]; then
    echo "missing postgres.adminPostgresUser in $POCKET_ADVISOR_HELM_VALUES_ENVSUBSTED" >&2; exit 1
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
  if [ ! -f workspaces/pocket-advisor-infra.yaml ]; then
    echo "missing workspaces/pocket-advisor-infra.yaml" >&2
    exit 1
  fi
  envsubst_workspaces
  helm upgrade --install "$RELEASE" "$CHART" --namespace "$POCKET_ADVISOR_NAMESPACE" --create-namespace \
    -f "$POCKET_ADVISOR_HELM_VALUES_ENVSUBSTED" || exit 1
  for sts in postgres rustfs nats; do
    kubectl rollout status statefulset/"$sts" -n "$POCKET_ADVISOR_NAMESPACE" --timeout=5m || exit 1
  done
  echo
  echo "shared stores ready. next:"
  echo "  ./pocket-advisor.sh deploy-workspace <id>"
  echo "  ./pocket-advisor.sh build && ./$BIN --ingest-all --workspace-id <id>"
  echo
  echo "deploy-infra only brings up Postgres, RustFS and NATS themselves —"
  echo "nothing workspace-specific exists until deploy-workspace runs."
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
# bucket/identity/policy, and three JetStream streams. Idempotent throughout
# — re-running against an already-provisioned workspace skips what exists
# rather than failing on it, since adding a workspace back after editing
# $POCKET_ADVISOR_HELM_VALUES_ENVSUBSTED should not require remembering what already ran.
#
# rc for identity and policy, aws-cli for the bucket and the notification
# binding — not an arbitrary split. Checked directly: `aws s3api
# put-bucket-policy` rejects the same policy document `rc admin policy
# create` accepts, because bucket resource policies (aws-cli's mechanism)
# and IAM canned policies (rc's, the same one the old Tenant CRD used) are
# different mechanisms with different required shapes. Bucket creation and
# notification binding are S3 data-plane operations aws-cli's own surface
# covers completely; identity and policy creation are RustFS's own
# MinIO-shaped admin API, which aws-cli cannot reach at all, on RustFS or
# real AWS either.
cmd_deploy_workspace() {
  id=${1:-}
  if [ -z "$id" ]; then
    echo "usage: ./pocket-advisor.sh deploy-workspace <workspace_id>" >&2
    exit 1
  fi
  if [ ! -f workspaces/pocket-advisor-infra.yaml ]; then
    echo "missing workspaces/pocket-advisor-infra.yaml" >&2
    exit 1
  fi
  envsubst_workspaces
  resolve_infra

  pg_pass=$(yq ".workspaces[] | select(.id == \"$id\") | .postgres.password" "$POCKET_ADVISOR_HELM_VALUES_ENVSUBSTED")
  rustfs_secret=$(yq ".workspaces[] | select(.id == \"$id\") | .rustfs.password" "$POCKET_ADVISOR_HELM_VALUES_ENVSUBSTED")
  nats_pass=$(yq ".workspaces[] | select(.id == \"$id\") | .nats.password" "$POCKET_ADVISOR_HELM_VALUES_ENVSUBSTED")
  admin_pass=$(yq '.postgres.adminPostgresPassword' "$POCKET_ADVISOR_HELM_VALUES_ENVSUBSTED")
  root_user=$(yq '.rustfs.adminRustFSUser' "$POCKET_ADVISOR_HELM_VALUES_ENVSUBSTED")
  root_pass=$(yq '.rustfs.adminRustFSPassword' "$POCKET_ADVISOR_HELM_VALUES_ENVSUBSTED")
  for value in "$pg_pass" "$rustfs_secret" "$nats_pass" "$admin_pass" "$root_user" "$root_pass"; do
    if [ -z "$value" ] || [ "$value" = "null" ]; then
      echo "workspace \"$id\" is missing a required credential in $POCKET_ADVISOR_HELM_VALUES_ENVSUBSTED" >&2
      exit 1
    fi
  done

  echo "--- postgres: role (idempotent via its own DO block) ---"
  PGPASSWORD=$admin_pass psql -h "$POSTGRES_HOST" -p "$POSTGRES_PORT" -U "$POSTGRES_ADMIN_USER" -d postgres -v ON_ERROR_STOP=1 \
    -c "DO \$\$ BEGIN CREATE ROLE \"${id}_user\" LOGIN PASSWORD '$pg_pass'; EXCEPTION WHEN duplicate_object THEN RAISE NOTICE 'role exists, skipping'; END \$\$;" \
    || exit 1

  echo "--- postgres: database (CREATE DATABASE has no IF NOT EXISTS; tolerated here the same way bucket/policy/user creation below is) ---"
  PGPASSWORD=$admin_pass psql -h "$POSTGRES_HOST" -p "$POSTGRES_PORT" -U "$POSTGRES_ADMIN_USER" -d postgres \
    -c "CREATE DATABASE \"$id\" OWNER \"${id}_user\";" \
    >/dev/null 2>&1 || echo "  database $id already exists, skipping"

  echo "--- postgres: extensions (needs the admin's superuser rights — the workspace's own role never has them, deviation 20/39) ---"
  PGPASSWORD=$admin_pass psql -h "$POSTGRES_HOST" -p "$POSTGRES_PORT" -U "$POSTGRES_ADMIN_USER" -d "$id" -v ON_ERROR_STOP=1 \
    -c "CREATE EXTENSION IF NOT EXISTS vector;" \
    -c "CREATE EXTENSION IF NOT EXISTS pg_textsearch;" \
    || exit 1

  echo "--- rustfs: bucket, identity, policy ---"
  rc alias set pa-admin "$RUSTFS_ENDPOINT_URL" "$root_user" "$root_pass" >/dev/null
  AWS_ACCESS_KEY_ID=$root_user AWS_SECRET_ACCESS_KEY=$root_pass \
    aws s3api create-bucket --bucket "$id" --endpoint-url "$RUSTFS_ENDPOINT_URL" --region us-east-1 >/dev/null 2>&1 || true
  policy_file=$(mktemp)
  printf '{"Version":"2012-10-17","Statement":[{"Effect":"Allow","Action":["s3:*"],"Resource":["arn:aws:s3:::%s","arn:aws:s3:::%s/*"]}]}' "$id" "$id" > "$policy_file"
  rc admin policy create pa-admin "$id" "$policy_file" >/dev/null 2>&1 || true
  rm -f "$policy_file"
  rc admin user add pa-admin "$id" "$rustfs_secret" >/dev/null 2>&1 || true
  rc admin policy attach pa-admin "$id" --user "$id" >/dev/null 2>&1 || true

  echo "--- rustfs: bucket notification binding (raw/ only — extracted/ children are worker output, re-ingesting them would loop) ---"
  arn="arn:rustfs:sqs::$(echo "$id" | tr '-' '_'):nats"
  AWS_ACCESS_KEY_ID=$id AWS_SECRET_ACCESS_KEY=$rustfs_secret \
    aws s3api put-bucket-notification-configuration \
      --endpoint-url "$RUSTFS_ENDPOINT_URL" --region us-east-1 --bucket "$id" \
      --notification-configuration \
        '{"QueueConfigurations":[{"QueueArn":"'"$arn"'","Events":["s3:ObjectCreated:*"],"Filter":{"Key":{"FilterRules":[{"Name":"prefix","Value":"raw/"}]}}}]}' \
    || exit 1

  echo "--- nats: streams ---"
  nats stream add INGESTION -s "nats://$NATS_HOST" --user "$id" --password "$nats_pass" \
    --subjects="ingest.emails.raw,ingest.pdfs.raw,ingest.docx.raw,ingest.images.raw,ingest.text.embed" \
    --retention=work --storage=file --discard=new --max-msgs=1000000 \
    --max-msgs-per-subject=-1 --max-bytes=-1 --max-age=-1 --max-msg-size=-1 \
    --dupe-window=2m --no-allow-rollup --deny-delete --no-deny-purge --replicas=1 \
    >/dev/null 2>&1 || echo "  INGESTION already exists, skipping"
  nats stream add INGESTION_DLQ -s "nats://$NATS_HOST" --user "$id" --password "$nats_pass" \
    --subjects="ingest.dlq" \
    --retention=limits --storage=file --discard=new --max-age=720h \
    --max-msgs=-1 --max-msgs-per-subject=-1 --max-bytes=-1 --max-msg-size=-1 \
    --dupe-window=2m --no-allow-rollup --deny-delete --no-deny-purge --replicas=1 \
    >/dev/null 2>&1 || echo "  INGESTION_DLQ already exists, skipping"
  nats stream add RUSTFS_EVENTS -s "nats://$NATS_HOST" --user "$id" --password "$nats_pass" \
    --subjects="rustfs.events.raw" \
    --retention=work --storage=file --discard=new --max-msgs=1000000 \
    --max-msgs-per-subject=-1 --max-bytes=-1 --max-age=-1 --max-msg-size=-1 \
    --dupe-window=10m --no-allow-rollup --deny-delete --no-deny-purge --replicas=1 \
    >/dev/null 2>&1 || echo "  RUSTFS_EVENTS already exists, skipping"

  echo
  echo "workspace \"$id\" provisioned."
}

# The inverse of deploy-workspace, and — unlike destroy-infra/destroy-state —
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
  if [ ! -f workspaces/pocket-advisor-infra.yaml ]; then
    echo "missing workspaces/pocket-advisor-infra.yaml" >&2
    exit 1
  fi
  envsubst_workspaces
  resolve_infra

  admin_pass=$(yq '.postgres.adminPostgresPassword' "$POCKET_ADVISOR_HELM_VALUES_ENVSUBSTED")
  root_user=$(yq '.rustfs.adminRustFSUser' "$POCKET_ADVISOR_HELM_VALUES_ENVSUBSTED")
  root_pass=$(yq '.rustfs.adminRustFSPassword' "$POCKET_ADVISOR_HELM_VALUES_ENVSUBSTED")

  echo "--- postgres: dropping database and role ---"
  PGPASSWORD=$admin_pass psql -h "$POSTGRES_HOST" -p "$POSTGRES_PORT" -U "$POSTGRES_ADMIN_USER" -d postgres -v ON_ERROR_STOP=1 \
    -c "SELECT pg_terminate_backend(pid) FROM pg_stat_activity WHERE datname = '$id';" \
    -c "DROP DATABASE IF EXISTS \"$id\";" \
    -c "DROP ROLE IF EXISTS \"${id}_user\";" \
    || exit 1

  echo "--- rustfs: removing identity, policy, bucket ---"
  rc alias set pa-admin "$RUSTFS_ENDPOINT_URL" "$root_user" "$root_pass" >/dev/null
  rc admin policy detach pa-admin "$id" --user "$id" >/dev/null 2>&1 || true
  rc admin user rm pa-admin "$id" >/dev/null 2>&1 || true
  rc admin policy rm pa-admin "$id" >/dev/null 2>&1 || true
  AWS_ACCESS_KEY_ID=$root_user AWS_SECRET_ACCESS_KEY=$root_pass \
    aws s3 rb "s3://$id" --force --endpoint-url "$RUSTFS_ENDPOINT_URL" --region us-east-1 >/dev/null 2>&1 || true

  echo "--- nats: removing streams (as the workspace's own account — it owns nothing to remove if the account itself is already gone) ---"
  nats_pass=$(yq ".workspaces[] | select(.id == \"$id\") | .nats.password" "$POCKET_ADVISOR_HELM_VALUES_ENVSUBSTED")
  if [ "$nats_pass" != "null" ]; then
    for s in INGESTION INGESTION_DLQ RUSTFS_EVENTS; do
      nats stream rm "$s" -s "nats://$NATS_HOST" --user "$id" --password "$nats_pass" --force >/dev/null 2>&1 || true
    done
  fi

  echo
  echo "workspace \"$id\" torn down. Remove its entry from $POCKET_ADVISOR_HELM_VALUES_ENVSUBSTED by hand."
}

cmd_clean() {
  rm -rf bin
}

# F1-b real-cluster end-to-end: bring up a test Keycloak (H2 dev, start-dev
# TLS) and the app chart, then run the gated TestClusterE2E against the
# deployed gateway. The app image must be built first (docker-build-app) and
# reachable by the cluster; the workspace config, TLS, and egress CIDRs are
# operator-provided so no private material enters version control.
E2E_NS="$POCKET_ADVISOR_NAMESPACE"
E2E_RELEASE="pocket-advisor-e2e"

cmd_e2e_keycloak_up() {
  kubectl apply -f test/e2e/keycloak/keycloak.yaml -n "$E2E_NS" || exit 1
  kubectl rollout status deployment/pocket-advisor-e2e-keycloak -n "$E2E_NS" --timeout=5m || exit 1
  echo "keycloak ready at https://keycloak.$E2E_NS.svc.cluster.local:8443/realms/pocket-advisor"
}

cmd_e2e_app_up() {
  local ws="${1:-e2e}"
  local pg_cidr="$2"
  local model_cidr="$3"
  cmd_docker_build_app || exit 1
  kubectl create secret generic "$E2E_RELEASE-config" \
    --from-file=config.yaml="workspaces/$ws-config.yaml" \
    --from-file=workspace-config.yaml="workspaces/$ws-workspace-config.yaml" \
    --from-file=workspace-values.yaml="workspaces/$ws-workspace-values.yaml" \
    -n "$E2E_NS" --dry-run=client -o yaml | kubectl apply -f - || exit 1
  kubectl create secret generic "$E2E_RELEASE-oauth" \
    --from-literal=introspection-client-secret='e2e-introspection-secret' \
    -n "$E2E_NS" --dry-run=client -o yaml | kubectl apply -f - || exit 1
  if ! kubectl get secret "$E2E_RELEASE-tls" -n "$E2E_NS" >/dev/null 2>&1; then
    echo "create a TLS secret $E2E_RELEASE-tls for mcp.example.test before continuing, e.g.:" >&2
    echo "  openssl req -x509 -newkey rsa:2048 -nodes -keyout key.pem -out cert.pem -days 1 -subj /CN=mcp.example.test" >&2
    echo "  kubectl create secret tls $E2E_RELEASE-tls --cert=cert.pem --key=key.pem -n $E2E_NS" >&2
    exit 1
  fi
  local kc
  kc="$(kubectl get svc keycloak -n "$E2E_NS" -o jsonpath='{.spec.clusterIP}')"
  helm upgrade --install "$E2E_RELEASE" "$APP_CHART" -n "$E2E_NS" --create-namespace \
    -f test/e2e/app-values.yaml \
    --set workspace.id="$ws" \
    --set oauth.authorizationServer="https://keycloak.$E2E_NS.svc.cluster.local:8443/realms/pocket-advisor" \
    --set oauth.introspectionEndpoint="https://keycloak.$E2E_NS.svc.cluster.local:8443/realms/pocket-advisor/protocol/openid-connect/token/introspect" \
    --set 'networkPolicy.egress[0].cidr='"$pg_cidr" --set 'networkPolicy.egress[0].ports[0]=5432' \
    --set 'networkPolicy.egress[1].cidr='"$model_cidr" --set 'networkPolicy.egress[1].ports[0]=8080' \
    --set 'networkPolicy.egress[2].cidr='"$kc/32" --set 'networkPolicy.egress[2].ports[0]=8443' \
    || exit 1
  kubectl rollout status deployment/"$E2E_RELEASE" -n "$E2E_NS" --timeout=5m || exit 1
}

cmd_e2e_mcp() {
  PA_K8S_E2E=1 \
  PA_E2E_MCP_URL="https://$E2E_RELEASE.$E2E_NS.svc.cluster.local/mcp" \
  PA_E2E_HOST="mcp.example.test" \
  PA_E2E_KEYCLOAK_URL="https://keycloak.$E2E_NS.svc.cluster.local:8443/realms/pocket-advisor" \
  PA_E2E_CLIENT_ID="pocket-advisor-opencode" \
  PA_E2E_REDIRECT_URI="http://127.0.0.1:19876/mcp/oauth/callback" \
  PA_E2E_USER="e2e-user" \
  PA_E2E_PASSWORD="e2e-password" \
  PA_E2E_INSECURE=1 \
  go test ./internal/mcp/ -run TestClusterE2E -v
}

cmd_e2e_down() {
  helm uninstall "$E2E_RELEASE" -n "$E2E_NS" 2>/dev/null || true
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
  docker-build-app) cmd_docker_build_app ;;
  e2e-keycloak-up) cmd_e2e_keycloak_up ;;
  e2e-app-up) cmd_e2e_app_up "${1:-}" "${2:-}" "${3:-}" ;;
  e2e-mcp) cmd_e2e_mcp ;;
  e2e-down) cmd_e2e_down ;;
  deploy-infra) cmd_deploy_infra ;;
  destroy-infra) cmd_destroy_infra ;;
  destroy-state) cmd_destroy_state ;;
  deploy-metrics-server) cmd_deploy_metrics_server ;;
  destroy-metrics-server) cmd_destroy_metrics_server ;;
  deploy-workspace) cmd_deploy_workspace "${1:-}" ;;
  destroy-workspace) cmd_destroy_workspace "${1:-}" ;;
  clean) cmd_clean ;;
  *)
    echo "unknown command: $cmd" >&2
    usage >&2
    exit 1
    ;;
esac
