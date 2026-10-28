#!/bin/sh
# Uploads dSYMs to a Snag server so crash and hang reports show function names and lines.
#
#   SNAG_URL=https://snag.example.com SNAG_SYMBOLS_TOKEN=snag_... Scripts/upload-symbols.sh [path ...]
#
# Paths can be .dSYM bundles, .xcarchive bundles, or folders containing dSYMs. With no paths it
# uses Xcode's $DWARF_DSYM_FOLDER_PATH, so it also works as a Run Script build phase (Release
# configuration; turn off "User Script Sandboxing" or list the dSYM as an input file).
# Create the token in the project's settings under "Crash symbols"; it can't send reports.
set -eu

: "${SNAG_URL:?set SNAG_URL to your Snag server}"
: "${SNAG_SYMBOLS_TOKEN:?set SNAG_SYMBOLS_TOKEN to a symbol upload token}"

if [ "$#" -eq 0 ]; then
	: "${DWARF_DSYM_FOLDER_PATH:?pass dSYM paths, or run from an Xcode build phase}"
	set -- "$DWARF_DSYM_FOLDER_PATH"
fi

work=$(mktemp -d)
trap 'rm -rf "$work"' EXIT
for root in "$@"; do
	case "$root" in
	*.xcarchive) root="$root/dSYMs" ;;
	esac
	# find prints the bundle itself when root is a .dSYM.
	find "$root" -name '*.dSYM' -type d -prune | while IFS= read -r dsym; do
		name=$(basename "$dsym")
		zip="$work/$name.zip"
		if command -v ditto >/dev/null 2>&1; then
			ditto -c -k --keepParent "$dsym" "$zip"
		else
			(cd "$(dirname "$dsym")" && zip -qr "$zip" "$name")
		fi
		printf 'Uploading %s... ' "$name"
		curl --silent --show-error --fail-with-body --retry 3 \
			-H "Authorization: Bearer $SNAG_SYMBOLS_TOKEN" \
			-H "Content-Type: application/zip" \
			--data-binary "@$zip" \
			"${SNAG_URL%/}/api/v1/symbols"
		echo
	done
done
