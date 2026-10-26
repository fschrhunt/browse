#!/bin/sh
# Installs browse for this user on Linux (x86-64) or macOS, replacing any
# earlier install. Run it again to update.
#   ~/.local/bin/browse                the command, from a GitHub release checked
#                                      against its checksums; built from this
#                                      checkout instead when go is here
#   ~/.local/share/browse/             the Chromium headless shell it drives
#   <agent>/skills/browse/SKILL.md     the agent skill, through browse skill
#
#   curl -fsSL https://fschrhunt.com/browse/install.sh | sh
#   curl -fsSL https://fschrhunt.com/browse/install.sh | sh -s -- --version v0.2.0
#
# The shell is pinned to one version from Chrome for Testing, the Chromium
# team's own build pipeline. Needs curl, and unzip or python3; never go.
# scripts/uninstall.sh removes all of it.
set -eu

CHROME=156.0.8078.47
RELEASES=${BROWSE_RELEASES:-https://github.com/fschrhunt/browse/releases} # replaceable for testing
BIN="$HOME/.local/bin"
DATA="${XDG_DATA_HOME:-$HOME/.local/share}/browse"

fail() { echo "browse install: $*" >&2; exit 1; }

version=
while [ $# -gt 0 ]; do
	case $1 in
	--version)
		[ $# -ge 2 ] || fail "--version needs a version, like v0.2.0"
		version=$2
		shift 2
		;;
	*) fail "unknown option $1; the one option is --version vX.Y.Z" ;;
	esac
done

case "$(uname -s)/$(uname -m)" in
Linux/x86_64) os=linux arch=amd64 shell=linux64 ;;
Darwin/arm64) os=darwin arch=arm64 shell=mac-arm64 ;;
Darwin/x86_64) os=darwin arch=amd64 shell=mac-x64 ;;
*) fail "browse runs on Linux x86-64 and macOS; Chrome for Testing has no headless shell for $(uname -s) $(uname -m)" ;;
esac

# The checkout this script is in, unless it was piped in through curl.
src=
case "$0" in */install.sh) src=$(cd "$(dirname "$0")/.." && pwd) ;; esac

mkdir -p "$BIN"
tmp=$(mktemp -d)
trap 'rm -rf "$tmp"' EXIT
if [ -z "$version" ] && [ -n "$src" ] && [ -f "$src/go.mod" ] && command -v go >/dev/null; then
	(cd "$src" && go build -o "$tmp/browse" .)
else
	if [ -z "$version" ]; then
		# The latest release redirects to its tag; read the tag from where it lands.
		version=$(curl -fsSLI -o /dev/null -w '%{url_effective}' "$RELEASES/latest")
		version=${version##*/}
	fi
	archive="browse_${version}_${os}_${arch}.tar.gz"
	curl -fsSL "$RELEASES/download/$version/$archive" -o "$tmp/$archive"
	curl -fsSL "$RELEASES/download/$version/checksums.txt" -o "$tmp/checksums.txt"
	(
		cd "$tmp" && grep " $archive\$" checksums.txt >checksum.txt &&
			if command -v sha256sum >/dev/null; then sha256sum -c --status checksum.txt; else shasum -a 256 -c -s checksum.txt; fi
	) || fail "$archive does not match the release's checksums"
	tar -xzf "$tmp/$archive" -C "$tmp" browse
fi
chmod +x "$tmp/browse"
mv "$tmp/browse" "$BIN/browse"
echo "installed $BIN/browse ($("$BIN/browse" version))"

dir="chrome-headless-shell-$shell"
mkdir -p "$DATA"
curl -fsSL "https://storage.googleapis.com/chrome-for-testing-public/$CHROME/$shell/$dir.zip" -o "$tmp/shell.zip"
rm -rf "${DATA:?}/$dir"
if command -v unzip >/dev/null; then
	unzip -q "$tmp/shell.zip" -d "$DATA"
else
	python3 -m zipfile -e "$tmp/shell.zip" "$DATA"
	# zipfile does not keep the executable bit, so set it here.
	chmod +x "$DATA/$dir/chrome-headless-shell"
fi
echo "installed $DATA/$dir ($CHROME)"

"$BIN/browse" skill

case ":$PATH:" in
*":$BIN:"*) ;;
*) echo "add $BIN to your PATH to run browse" ;;
esac
