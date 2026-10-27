#!/bin/sh
# Builds the server image, runs it on a random port with throwaway credentials, and runs the
# Swift live tests against it.
set -eu
cd "$(dirname "$0")/.."
KEY=$(openssl rand -hex 24)
PW=$(openssl rand -hex 12)
PORT=${PORT:-18787}
docker build -q -t snag-server:test Server >/dev/null
docker rm -f snag-live-test >/dev/null 2>&1 || true
docker run -d --name snag-live-test -p "$PORT:8080" \
  -e SNAG_API_KEYS="live:$KEY" -e SNAG_ADMIN_PASSWORD="$PW" -e SNAG_PUBLIC_URL="http://127.0.0.1:$PORT" \
  snag-server:test >/dev/null
trap 'docker rm -f snag-live-test >/dev/null' EXIT
for _ in $(seq 1 30); do
  [ "$(docker inspect -f '{{.State.Health.Status}}' snag-live-test)" = healthy ] && break
  sleep 1
done
SNAG_SERVER_URL="http://127.0.0.1:$PORT" SNAG_API_KEY="$KEY" SNAG_ADMIN_PASSWORD="$PW" swift test --filter LiveServerTests
