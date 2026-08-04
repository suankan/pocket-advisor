# pocket-advisor runs on the host, against the three stores in the local
# cluster. There are no container images to build any more.

BIN     := bin/pocket-advisor
PKG     := ./cmd/pocket-advisor
RELEASE := pocket-advisor
CHART   := charts/pocket-advisor-infra

# The private per-workspace override: names and credentials for each
# workspace's database, bucket and NATS account. Gitignored, and the same file
# the binary reads (config.yaml's workspaces.values), so Helm and the CLI
# cannot disagree about a password. Overridable for a second environment:
#   make deploy-infra WS_VALUES=workspaces/other.yaml
WS_VALUES := workspaces/pocket-advisor-infra.yaml

# One release manages every workspace. It lives in its own namespace; the
# workspaces' resources are rendered into namespaces of their own, named after
# each workspace id, which the chart creates.
NS := pocket-advisor

# OCR is a CGo build against Homebrew's tesseract and leptonica. Without the
# tag the binary still builds and runs, but scanned PDFs and images are
# recorded SKIPPED rather than indexed — so it is the default, not an extra.
TAGS := ocr

# The toolchain is pinned in mise.toml, which also carries CGO_ENABLED and the
# tesseract include/library paths. Route through mise rather than trusting the
# ambient PATH: Homebrew's mise only auto-activates for fish, so a zsh shell
# that never ran `mise activate` has no `go` at all — and one that has some
# other `go` would build without the pinned CGo settings, which fails at link
# time or, worse, silently drops OCR.
MISE := $(shell command -v mise 2>/dev/null)
ifeq ($(MISE),)
GO    := go
GOFMT := gofmt
else
GO    := mise exec -- go
GOFMT := mise exec -- gofmt
endif

.PHONY: all build test race vet fmt lint require-workspace deploy-operator \
        deploy-infra deploy-all destroy-infra destroy-state clean

# Every cluster-facing target acts on exactly one workspace, and getting it
# wrong would deploy into — or destroy — the wrong namespace. Fail before
# touching anything rather than defaulting to something plausible.

all: build

build:
	$(GO) build -tags '$(TAGS)' -o $(BIN) $(PKG)

test:
	$(GO) test -tags '$(TAGS)' ./...

race:
	$(GO) test -tags '$(TAGS)' -race -count=1 ./...

vet:
	$(GO) vet -tags '$(TAGS)' ./...

fmt:
	$(GOFMT) -l -w .

lint: fmt vet
	helm lint $(CHART)
	@# The NATS account and the JetStream streams are chart-rendered now, not
	@# created by Go, so the unit tests that covered addAccountBlock and
	@# EnsureStreams went with them. The account is keyed by workspace id —
	@# unlike the bucket and database, it shares one server (deviation 23). This replaces both: render a throwaway
	@# workspace and check its account reaches nats-server.conf and its streams
	@# reach Stream CRDs. --set rather than a committed fixture, so the chart's
	@# own values.yaml stays the single source of truth for the shape.
	@helm template $(RELEASE) $(CHART) \
	  --set rustfs.credentials.rootUser=lint \
	  --set rustfs.credentials.rootPassword=lint \
	  --set workspaces[0].id=lintws \
	  --set workspaces[0].rustfs.credentials.secretKey=lint \
	  --set workspaces[0].nats.credentials.password=lint \
	  --set workspaces[0].postgres.credentials.password=lint > /tmp/pa-lint.yaml \
	  && grep -q '"lintws": {' /tmp/pa-lint.yaml \
	  && test "$$(grep -c 'kind: Stream' /tmp/pa-lint.yaml)" = "3" \
	  && grep -q 'namespace: lintws' /tmp/pa-lint.yaml \
	  && echo "ok: chart renders the workspace account, 3 streams, namespaced" \
	  || (echo "FAIL: rendered workspace is incomplete"; exit 1)


# The chart carries RustFS, NATS, and one CloudNativePG Cluster per workspace.
# Nothing in it runs pipeline code.
deploy-infra:
	@test -f $(WS_VALUES) || (echo "missing $(WS_VALUES)"; exit 1)
	@# Two attempts, and the first is expected to fail on a fresh cluster.
	@# CloudNativePG installs a mutating webhook, and applying a Cluster
	@# requires that webhook to be serving — but one release applies the
	@# operator Deployment and our CRs in the same pass, so on a first install
	@# the webhook Service has no endpoints yet. The operators come up
	@# regardless, so the retry succeeds. This is the price of one chart
	@# instead of two; Helm cannot express "wait for a subchart to be ready"
	@# mid-apply (ingestion-design.md deviation 24).
	helm upgrade --install $(RELEASE) $(CHART) --namespace $(NS) --create-namespace \
	  -f $(WS_VALUES) || ( \
	    echo "first apply failed; waiting for operator webhooks, then retrying" && \
	    kubectl wait --for=condition=Available deploy --all -n $(NS) --timeout=5m && \
	    helm upgrade --install $(RELEASE) $(CHART) --namespace $(NS) --create-namespace \
	      -f $(WS_VALUES) )
	@# Wait on the stores themselves, per workspace namespace. By label rather
	@# than by name: a namespace mid-reconcile may not have every workload yet,
	@# and "not found" should mean "keep waiting", not "fail".
	@for ns in $$(kubectl get ns -l app.kubernetes.io/part-of=rag-ingestion-engine -o name 2>/dev/null | cut -d/ -f2); do \
	  for sts in $$(kubectl get sts -n $$ns -o name 2>/dev/null); do \
	    kubectl rollout status $$sts -n $$ns --timeout=5m || exit 1; \
	  done; \
	done
	@# The RustFS operator (0.0.5) gives up on provisioning after its first
	@# attempt fails, and the first attempt races RustFS's own storage init:
	@# tenants sit at "failed to list RustFS canned policies" indefinitely —
	@# observed still stuck after 3 minutes — while their pods run healthily.
	@# A no-op annotation forces the reconcile it should retry itself. Remove
	@# this once the operator backs off and retries provisioning on its own.
	@for ns in $$(kubectl get ns -l app.kubernetes.io/part-of=rag-ingestion-engine -o name 2>/dev/null | cut -d/ -f2); do \
	  kubectl annotate tenant --all -n $$ns \
	    pocket-advisor/reconcile-ts="$$(date +%s)" --overwrite >/dev/null 2>&1 || true; \
	done
	@kubectl wait --for=condition=Ready tenant.rustfs.com --all \
	  --all-namespaces --timeout=5m 2>/dev/null || true
	@# Each workspace's Postgres is its own CNPG cluster; the operator reports
	@# readiness on the Cluster resource rather than on a StatefulSet.
	@kubectl wait --for=condition=Ready cluster.postgresql.cnpg.io --all \
	  --all-namespaces --timeout=10m 2>/dev/null || true
	@echo
	@echo "all workspaces ready. next:"
	@echo "  make build"
	@echo "  ./$(BIN) --ingest-all --workspace-id <id>"
	@echo
	@echo "There is no provisioning step: everything a workspace needs is"
	@echo "declared by this chart, and --ingest-all applies the schema and the"
	@echo "bucket notification rule itself. see README.md"


# Keeps PVCs: Tier 1 is the corpus source of truth, and the JetStream volume is
# what makes an interrupted ingest resumable. Pair with destroy-state to also
# wipe them.
#
# Nothing needs deleting by hand any more. Every object is chart-rendered, so
# uninstall removes it — the notify Secret used to be written by the CLI
# through the Kubernetes API and had to be cleaned up separately, until the
# chart took it over (deviation 24).
#
# --ignore-not-found on both, so a re-run — or a run after the release was
# removed some other way — still reaches the cleanup rather than aborting on
# the uninstall, which is how leftovers survived long enough to be noticed.
destroy-infra:
	helm uninstall $(RELEASE) --namespace $(NS) --ignore-not-found
	@echo
	@echo "release removed. PVCs kept — 'make destroy-state' also wipes those."

# Deliberately separate from destroy-infra: this deletes Tier 1's corpus and
# the JetStream/Postgres state behind it, not just the workloads that mount
# them. Run destroy-infra first — a StatefulSet's volumeClaimTemplates
# outlives `helm uninstall` on purpose (see README.md), so nothing here
# depends on ordering, but there's nothing left to delete PVCs *from* once
# the release exists again.
destroy-state:
	@# The release namespace as well as the workspace ones: the shared NATS
	@# lives there, and its JetStream volume is state like any other. Only the
	@# workspace namespaces carry the part-of label, so listing them alone
	@# silently left that volume behind.
	@for ns in $(NS) $$(kubectl get ns -l app.kubernetes.io/part-of=rag-ingestion-engine -o name 2>/dev/null | cut -d/ -f2); do \
	  kubectl delete pvc --all --namespace $$ns --ignore-not-found; \
	done

clean:
	rm -rf bin
