#!/usr/bin/env bash
# Runs ON THE SERVER as root, from the source directory. Builds in throwaway containers,
# so the host needs neither Go nor Node, then installs the binary and restarts the panel.
set -euo pipefail
cd "$(dirname "$0")"

docker run --rm -v "$PWD/web:/web" -v hypanel-npm:/root/.npm -w /web node:22-alpine \
  sh -c 'npm install --no-audit --no-fund --loglevel=error && npm run build'

docker run --rm -v "$PWD:/src" -v hypanel-gomod:/go/pkg/mod -v hypanel-gocache:/root/.cache/go-build \
  -w /src -e CGO_ENABLED=0 golang:1.25-alpine \
  sh -c 'go mod tidy && go vet ./... && go test ./... && go build -trimpath -ldflags="-s -w" -o hypanel ./cmd/hypanel'

install -m 0755 hypanel /usr/local/bin/hypanel.new
mv /usr/local/bin/hypanel.new /usr/local/bin/hypanel
ls -la /usr/local/bin/hypanel
if systemctl is-enabled --quiet hypanel 2>/dev/null; then
  systemctl restart hypanel
  systemctl is-active hypanel
fi
echo BUILD_DONE
