#!/bin/sh
# Installs browse for this user on Linux (x86-64) or macOS, replacing any
# earlier install:
#   ~/.local/bin/browse                the command: the binary given as $1,
#                                      else built from this checkout when go
#                                      is here, else the latest release
#   ~/.local/share/browse/             the Chromium headless shell it drives
#   <agent>/skills/browse/SKILL.md     the agent skill, through browse skill
# The shell is pinned to one version from Chrome for Testing, the Chromium
# team's own build pipeline. Needs curl, and unzip or python3; never go. To
# install without a checkout:
#   curl -fsSL https://fschrhunt.com/browse/install.sh | sh
# scripts/uninstall.sh removes all of it.
set -eu

VERSION=156.0.8078.47
# The checkout this script is in, unless it was piped in through curl.
SRC=
case "$0" in */install.sh) SRC=$(cd "$(dirname "$0")/.." && pwd) ;; esac
RELEASES=https://github.com/fschrhunt/browse/releases/latest/download
BIN="$HOME/.local/bin"
DATA="${XDG_DATA_HOME:-$HOME/.local/share}/browse"

case "$(uname -s)/$(uname -m)" in
Linux/x86_64) BUILD=linux64 BINARY=browse-linux-amd64 ;;
Darwin/arm64) BUILD=mac-arm64 BINARY=browse-darwin-arm64 ;;
Darwin/x86_64) BUILD=mac-x64 BINARY=browse-darwin-amd64 ;;
*)
	echo "browse runs on Linux x86-64 and macOS; Chrome for Testing has no headless shell for $(uname -s) $(uname -m)" >&2
	exit 1
	;;
esac
SHELL_DIR="chrome-headless-shell-$BUILD"
URL="https://storage.googleapis.com/chrome-for-testing-public/$VERSION/$BUILD/$SHELL_DIR.zip"

mkdir -p "$BIN"
TMP=$(mktemp -d)
trap 'rm -rf "$TMP"' EXIT
if [ $# -gt 0 ]; then
	cp "$1" "$TMP/browse"
elif [ -n "$SRC" ] && [ -f "$SRC/go.mod" ] && command -v go >/dev/null; then
	(cd "$SRC" && go build -o "$TMP/browse" .)
else
	curl -fsSL "$RELEASES/$BINARY" -o "$TMP/browse"
fi
chmod +x "$TMP/browse"
mv "$TMP/browse" "$BIN/browse"
echo "installed $BIN/browse"

mkdir -p "$DATA"
curl -fsSL "$URL" -o "$TMP/shell.zip"
rm -rf "$DATA/$SHELL_DIR"
if command -v unzip >/dev/null; then
	unzip -q "$TMP/shell.zip" -d "$DATA"
else
	python3 -m zipfile -e "$TMP/shell.zip" "$DATA"
	# zipfile does not keep the executable bit, so set it here.
	chmod +x "$DATA/$SHELL_DIR/chrome-headless-shell"
fi
echo "installed $DATA/$SHELL_DIR ($VERSION)"

"$BIN/browse" skill

case ":$PATH:" in
*":$BIN:"*) ;;
*) echo "add $BIN to your PATH to run browse" ;;
esac
