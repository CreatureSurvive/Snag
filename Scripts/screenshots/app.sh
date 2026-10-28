#!/bin/sh
# Captures the example app screenshots in docs/screenshots, against a server seeded with
# Scripts/screenshots/seed.py. Usage: Scripts/screenshots/app.sh demo.env [simulator name]
# (demo.env holds SNAG_PUBLIC_URL, SNAG_API_KEYS=name:key and SNAG_ADMIN_PASSWORD).
set -eu
cd "$(dirname "$0")/../.."
. "$1"
SIM=${2:-iPhone 17 Pro}
OUT=$PWD/docs/screenshots
mkdir -p "$OUT"
xcrun simctl boot "$SIM" 2>/dev/null || true
# A fresh install, so My Reports only lists the report sent here.
xcrun simctl uninstall "$SIM" com.creaturesurvive.SnagExample 2>/dev/null || true
xcrun simctl status_bar "$SIM" override --time 9:41 --batteryState charged --batteryLevel 100 --cellularBars 4 --wifiBars 3
(cd Example && xcodegen generate --quiet)
export TEST_RUNNER_SNAG_SERVER_URL=$SNAG_PUBLIC_URL
export TEST_RUNNER_SNAG_API_KEY=${SNAG_API_KEYS#*:}
export TEST_RUNNER_SNAG_ADMIN_PASSWORD=$SNAG_ADMIN_PASSWORD
export TEST_RUNNER_SNAG_SCREENSHOT_DIR=$OUT
rm -rf Example/build/Screenshots.xcresult
xcodebuild test -project Example/SnagExample.xcodeproj -scheme SnagExample -destination "platform=iOS Simulator,name=$SIM" \
  -only-testing:SnagExampleUITests/SnagExampleUITests/testReadmeScreenshots \
  -resultBundlePath Example/build/Screenshots.xcresult -quiet
xcrun simctl status_bar "$SIM" clear
