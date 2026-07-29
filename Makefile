# pocket-advisor runs on the host, against the three stores in the local
# cluster. There are no container images to build any more.

BIN     := bin/pocket-advisor
PKG     := ./cmd/pocket-advisor
RELEASE := pocket-advisor
CHART   := infra/charts/pocket-advisor
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

# The chart carries RustFS, PostgreSQL+pgvector and NATS. Nothing in it runs
# pipeline code.
deploy-infra:
	helm upgrade --install $(RELEASE) $(CHART) --namespace $(NS) --create-namespace
	kubectl wait --for=condition=complete job \
	  -l app.kubernetes.io/component=rustfs-setup -n $(NS) --timeout=5m
	@echo
	@echo "stores ready. next:"
	@echo "  make build"
	@echo "  ./$(BIN) --bootstrap-schema"
	@echo "  ./$(BIN) --ingest-all --workspace-id <id>"
	@echo "see README.md"

# Keeps PVCs: Tier 1 is the corpus source of truth, and the JetStream volume is
# what makes an interrupted ingest resumable. Pair with destroy-state to also
# wipe them.
destroy-infra:
	helm uninstall $(RELEASE) --namespace $(NS)

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
