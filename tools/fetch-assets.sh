#!/usr/bin/env bash
# Vendors terminal.css from terminal-to-html at the exact version pinned in
# go.mod. The CSS class names (term-fgx123, term-bgi4, ...) are an output
# contract of the library, so the stylesheet and the module must move together.
set -euo pipefail

cd "$(dirname "$0")/.."

# Robust against both the single-line and the require(...) block layouts that
# `go mod tidy` may produce: take the first v-prefixed token on the line.
VERSION="$(awk '/buildkite\/terminal-to-html/ {for (i=1;i<=NF;i++) if ($i ~ /^v[0-9]/) {print $i; exit}}' go.mod)"
[ -n "$VERSION" ] || { echo "could not read the terminal-to-html version from go.mod" >&2; exit 1; }

DEST="web/vendor/terminal.css"
URL="https://raw.githubusercontent.com/buildkite/terminal-to-html/${VERSION}/internal/assets/terminal.css"

echo "fetching terminal.css at ${VERSION}"
mkdir -p "$(dirname "$DEST")"
curl -fsSL "$URL" -o "$DEST"
echo "wrote ${DEST} ($(wc -l < "$DEST") lines)"
