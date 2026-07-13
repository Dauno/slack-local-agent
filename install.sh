#!/usr/bin/env bash
set -euo pipefail

VERSION="${VERSION:-dev}"
COMMIT="${COMMIT:-$(git rev-parse --short HEAD 2>/dev/null || echo unknown)}"
DATE="${DATE:-$(date -u +%Y-%m-%dT%H:%M:%SZ)}"

LDFLAGS="-s -w -X github.com/Dauno/slack-local-agent/internal/buildinfo.Version=${VERSION} -X github.com/Dauno/slack-local-agent/internal/buildinfo.Commit=${COMMIT} -X github.com/Dauno/slack-local-agent/internal/buildinfo.Date=${DATE}"

DEST_DIR="${PREFIX:-$HOME/.local-agent/bin}"
BIN="local-agent"

echo "Building ${BIN}..."
tmpdir="$(mktemp -d)"
trap 'rm -rf "$tmpdir"' EXIT
go build -trimpath -ldflags "${LDFLAGS}" -o "${tmpdir}/${BIN}" ./cmd/local-agent

mkdir -p "${DEST_DIR}"
install -m 0755 "${tmpdir}/${BIN}" "${DEST_DIR}/${BIN}"

echo "Installed ${DEST_DIR}/${BIN}"

if [[ ":$PATH:" != *":${DEST_DIR}:"* ]]; then
    echo
    echo "WARNING: ${DEST_DIR} is not in your PATH."
    echo "Add it with:  export PATH=\"\${PATH}:${DEST_DIR}\""
fi
