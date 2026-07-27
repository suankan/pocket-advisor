#!/bin/sh
# Build every container image. Run from the v3/ directory.
#
#   ./build/build-images.sh [tag]
set -e

TAG="${1:-latest}"
cd "$(dirname "$0")/.."

# Derived from cmd/, never hardcoded: an image name that does not match a
# cmd/ directory is unbuildable, and a chart referencing one fails to pull.
PURE_GO=$(ls cmd | grep -v '^document-extractor$' | tr '\n' ' ')

for bin in $PURE_GO; do
    echo "==> pocket-advisor/${bin}:${TAG}"
    docker build \
        -f build/Dockerfile \
        --build-arg BINARY="${bin}" \
        -t "pocket-advisor/${bin}:${TAG}" \
        .
done

echo "==> pocket-advisor/document-extractor:${TAG} (CGo + tesseract)"
docker build \
    -f build/Dockerfile.document-extractor \
    -t "pocket-advisor/document-extractor:${TAG}" \
    .

echo
echo "Built:"
for bin in $PURE_GO document-extractor; do
    docker image ls --format '  {{.Repository}}:{{.Tag}}  {{.Size}}' "pocket-advisor/${bin}:${TAG}"
done
