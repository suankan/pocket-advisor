# Pure-Go binaries: uploader, discovery, email-processor, office-extractor,
# embed-indexer, schema-bootstrap.
#
# CGO_ENABLED=0 for all of them. Only document-extractor needs a C toolchain,
# and keeping that split explicit prevents it from silently becoming a
# dependency of the whole system (ingestion-design.md §8.3).
#
#   docker build -f build/Dockerfile --build-arg BINARY=uploader -t pocket-advisor/uploader:latest .

ARG GO_VERSION=1.25

FROM golang:${GO_VERSION}-alpine AS build
ARG BINARY
WORKDIR /src

# Dependency layer first so source edits do not re-download the module graph.
COPY go.mod go.sum ./
RUN go mod download

COPY . .
RUN test -n "$BINARY" || (echo "build arg BINARY is required" && exit 1)
RUN CGO_ENABLED=0 GOOS=linux go build \
        -trimpath -ldflags="-s -w" \
        -o /out/app ./cmd/${BINARY}

FROM alpine:3.20
ARG BINARY
RUN apk add --no-cache ca-certificates tzdata && \
    adduser -D -u 10001 pocket
COPY --from=build /out/app /usr/local/bin/app
USER pocket
ENV METRICS_PORT=9090
EXPOSE 9090
ENTRYPOINT ["/usr/local/bin/app"]
