#!/bin/sh
# Removes everything browse installed or made, after stopping it: the command,
# the Chromium headless shell, the agent skills, the socket, and the state,
# which is the browser profile with its logins and the daemon's log. Files
# you downloaded stay in ~/Downloads.
# Without a checkout:
#   curl -fsSL https://raw.githubusercontent.com/fschrhunt/browse/main/scripts/uninstall.sh | sh
set -eu

BIN="$HOME/.local/bin/browse"
DATA="${XDG_DATA_HOME:-$HOME/.local/share}/browse"
STATE="${XDG_STATE_HOME:-$HOME/.local/state}/browse"

# Stop the daemon and its Chrome. Their logins are about to be deleted, so
# there is no need to wait for Chrome to save them.
pkill -f "browse serve\$" || true
pkill -f -- "--user-data-dir=$STATE/profile" || true

# The skill folders browse skill added, one per line.
if [ -f "$DATA/skills" ]; then
	while IFS= read -r dir; do
		case "$dir" in
		*/browse) rm -rf "$dir" && rmdir "${dir%/browse}" 2>/dev/null || true ;; # also the skills folder if now empty
		esac
	done <"$DATA/skills"
fi
rm -f "$BIN"
rm -rf "$DATA" "$STATE"
for sock in "${BROWSE_SOCKET:-}" "${XDG_RUNTIME_DIR:-/nonexistent}/browse.sock" "${TMPDIR:-/tmp}/browse-$(id -u).sock"; do
	if [ -n "$sock" ]; then rm -f "$sock"; fi
done
# Frames the terminal had not yet read and deleted.
rm -f /dev/shm/tty-graphics-protocol-browse-* "${TMPDIR:-/tmp}"/tty-graphics-protocol-browse-*
echo "browse is uninstalled"
