#!/bin/sh
# Builds browse for every system it runs on and publishes them as a GitHub
# release of fschrhunt/browse, which scripts/install.sh downloads from:
#   scripts/release.sh v0.1.0
# Without a tag it only builds, into dist/. Needs go, and gh to publish.
set -eu

SRC=$(cd "$(dirname "$0")/.." && pwd)
cd "$SRC"
rm -rf dist
mkdir dist
for target in linux/amd64 darwin/arm64 darwin/amd64; do
	GOOS=${target%/*} GOARCH=${target#*/} CGO_ENABLED=0 \
		go build -trimpath -ldflags=-s -o "dist/browse-${target%/*}-${target#*/}" .
done
ls -l dist

if [ $# -gt 0 ]; then
	gh release create "$1" dist/* --repo fschrhunt/browse --title "$1" --notes ""
fi
