# pocket-advisor runs on the host, against the three stores in the local
# cluster. There are no container images to build any more.

BIN     := bin/pocket-advisor
PKG     := ./cmd/pocket-advisor
RELEASE := pocket-advisor
CHART   := charts/pocket-advisor

# The private per-workspace override: names and credentials for each
# workspace's database, bucket and NATS account. Gitignored, and the same file
# the binary reads (config.yaml's workspaces.values), so Helm and the CLI
# cannot disagree about a password. Overridable for a second environment:
#   make deploy-infra WS_VALUES=workspaces/other.yaml
WS_VALUES := workspaces/values.yaml
NS      := pocket-advisor

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

.PHONY: all build test race vet fmt lint deploy-infra destroy-infra destroy-state clean

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
	@# The NATS accounts block is rendered by the chart now, not patched by Go
	@# code, so the unit tests that covered addAccountBlock/removeAccountBlock
	@# went with it. This replaces them: render with a throwaway workspace and
	@# check it actually reaches nats-server.conf. Uses --set rather than a
	@# committed example file, so there is one source of truth for the shape
	@# (charts/pocket-advisor/values.yaml) and no fixture to drift from it.
	@helm template $(RELEASE) $(CHART) \
	  --set rustfs.credentials.rootUser=lint --set rustfs.credentials.rootPassword=lint \
	  --set postgres.credentials.user=lint --set postgres.credentials.password=lint \
	  --set workspaces[0].id=lintws \
	  --set workspaces[0].nats.credentials.password=lint \
	  --set workspaces[0].postgres.credentials.password=lint \
	  --set workspaces[0].rustfs.credentials.secretKey=lint \
	  | grep -q '"lintws": {' \
	  && echo "ok: chart renders workspace NATS accounts" \
	  || (echo "FAIL: workspaces from values did not reach nats-server.conf"; exit 1)

# The chart carries RustFS, PostgreSQL+pgvector and NATS. Nothing in it runs
# pipeline code.
deploy-infra:
	@test -f $(WS_VALUES) || (echo "missing $(WS_VALUES) — copy workspaces/values.yaml.example and fill it in"; exit 1)
	helm upgrade --install $(RELEASE) $(CHART) --namespace $(NS) --create-namespace \
	  -f $(WS_VALUES)
	@# Wait on the stores themselves. There used to be a rustfs-setup Job to
	@# wait for; it created a global bucket and two scoped identities that
	@# per-workspace isolation made redundant, and it was deleted
	@# (ingestion-design.md deviation 19). RustFS now carries a readiness
	@# probe, so rollout status is a real signal rather than a proxy one.
	kubectl rollout status statefulset/$(RELEASE)-rustfs -n $(NS) --timeout=5m
	kubectl rollout status statefulset/$(RELEASE)-postgres -n $(NS) --timeout=5m
	kubectl rollout status statefulset/$(RELEASE)-nats -n $(NS) --timeout=5m
	@echo
	@echo "stores ready, with a NATS account per workspace in $(WS_VALUES). next:"
	@echo "  make build"
	@echo "  ./$(BIN) --create-workspace --workspace-id <id>   # required first"
	@echo "  ./$(BIN) --ingest-all      --workspace-id <id>"
	@echo
	@echo "--create-workspace is the only step needing root credentials, and"
	@echo "the only one that points RustFS notifications at a workspace."
	@echo "see README.md"

# Keeps PVCs: Tier 1 is the corpus source of truth, and the JetStream volume is
# what makes an interrupted ingest resumable. Pair with destroy-state to also
# wipe them.
#
# The delete after uninstall is not belt-and-braces: the notify Secret is
# written by --create-workspace through the Kubernetes API rather than by the
# chart, so it is not a release resource and `helm uninstall` leaves it,
# stranding a deleted workspace's NATS password.
#
# --ignore-not-found on both, so a re-run — or a run after the release was
# removed some other way — still reaches the cleanup rather than aborting on
# the uninstall, which is how leftovers survived long enough to be noticed.
destroy-infra:
	helm uninstall $(RELEASE) --namespace $(NS) --ignore-not-found
	kubectl delete secret $(RELEASE)-rustfs-notify -n $(NS) --ignore-not-found
	@echo
	@echo "release removed. PVCs kept — 'make destroy-state' also wipes those."

# Deliberately separate from destroy-infra: this deletes Tier 1's corpus and
# the JetStream/Postgres state behind it, not just the workloads that mount
# them. Run destroy-infra first — a StatefulSet's volumeClaimTemplates
# outlives `helm uninstall` on purpose (see README.md), so nothing here
# depends on ordering, but there's nothing left to delete PVCs *from* once
# the release exists again.
destroy-state:
	kubectl delete pvc --all --namespace $(NS)

clean:
	rm -rf bin
