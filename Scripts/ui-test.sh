#!/bin/sh
# Runs the example app's UI tests against the snag-server container described by
# Server/.env.local (SNAG_API_KEYS=name:key, SNAG_ADMIN_PASSWORD), published on localhost:8787.
# Usage: Scripts/ui-test.sh [destination]   e.g. 'platform=iOS Simulator,name=iPhone 17 Pro'
set -eu
cd "$(dirname "$0")/.."
. Server/.env.local
DEST=${1:-"platform=iOS Simulator,name=iPhone 17 Pro"}
(cd Example && xcodegen generate --quiet)
export TEST_RUNNER_SNAG_SERVER_URL=${SNAG_SERVER_URL:-http://localhost:8787}
export TEST_RUNNER_SNAG_API_KEY=${SNAG_API_KEYS#*:}
export TEST_RUNNER_SNAG_ADMIN_PASSWORD=$SNAG_ADMIN_PASSWORD
rm -rf Example/build/UITests.xcresult
xcodebuild test -project Example/SnagExample.xcodeproj -scheme SnagExample -destination "$DEST" \
  -resultBundlePath Example/build/UITests.xcresult -allowProvisioningUpdates -quiet
