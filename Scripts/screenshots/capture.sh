#!/bin/sh
# Regenerates docs/screenshots: starts a throwaway server on :8788 with sample data for a fictional
# app, sends a report from the example app in the simulator, then captures the dashboard.
# Needs Docker, Xcode, XcodeGen, Node and Google Chrome. Usage: Scripts/screenshots/capture.sh [image]
set -eu
cd "$(dirname "$0")/../.."
IMAGE=${1:-ghcr.io/creaturesurvive/snag-server:latest}
WORK=$(mktemp -d)
trap 'docker rm -f snag-screenshots >/dev/null 2>&1; rm -rf "$WORK"' EXIT
umask 077
cat > "$WORK/demo.env" <<ENV
SNAG_ADMIN_PASSWORD=$(openssl rand -hex 16)
SNAG_API_KEYS=reelhouse:$(openssl rand -hex 24)
SNAG_PUBLIC_URL=http://localhost:8788
SNAG_RATE_LIMIT=10000
ENV
mkdir "$WORK/data"
docker run -d --name snag-screenshots -p 8788:8080 --env-file "$WORK/demo.env" -v "$WORK/data:/data" "$IMAGE" >/dev/null
for _ in 1 2 3 4 5 6 7 8 9 10; do
  python3 -c "import urllib.request; urllib.request.urlopen('http://localhost:8788/api/v1/health')" 2>/dev/null && break
  sleep 1
done
python3 -I Scripts/screenshots/seed.py "$WORK/demo.env"
Scripts/screenshots/app.sh "$WORK/demo.env"
ISSUE=$(python3 -I Scripts/screenshots/seed.py "$WORK/demo.env" finish)
node Scripts/screenshots/dashboard.mjs "$WORK/demo.env" "$ISSUE"
