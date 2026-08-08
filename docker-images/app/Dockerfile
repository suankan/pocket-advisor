FROM golang:1.25-bookworm AS build

RUN apt-get update \
    && apt-get install -y --no-install-recommends libtesseract-dev libleptonica-dev \
    && rm -rf /var/lib/apt/lists/*

WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY . .
RUN CGO_ENABLED=1 go build -tags ocr -trimpath -ldflags='-s -w' -o /out/pocket-advisor ./cmd/pocket-advisor

FROM debian:bookworm-slim

RUN apt-get update \
    && apt-get install -y --no-install-recommends ca-certificates curl libtesseract5 liblept5 \
    && rm -rf /var/lib/apt/lists/* \
    && groupadd --system --gid 65532 pocket-advisor \
    && useradd --system --uid 65532 --gid 65532 --home-dir /nonexistent --shell /usr/sbin/nologin pocket-advisor

COPY --from=build /out/pocket-advisor /usr/local/bin/pocket-advisor

USER 65532:65532
ENTRYPOINT ["/usr/local/bin/pocket-advisor"]
