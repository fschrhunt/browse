#!/bin/sh
# Takes screenshots of browse in a real terminal, kitty on a virtual display,
# through the scenarios that matter to a person: the start screen, typing an
# address, a page loading and loaded, hovering the bar, the box over the page,
# a new tab, the light theme, remote mode, and tmux. They land in
# dist/screens/ with a sheet of them all, dist/screens/sheet.png, to look at.
# Nothing is compared automatically; the sheet is for eyes. CI makes one on
# every push.
#
# Needs Xvfb, kitty, xdotool, tmux, and ImageMagick (import, montage), and a
# built browse: scripts/screens.sh [path/to/browse]. It uses display :99 and
# its own browse state under dist/screens, so a running browse and your
# profile are left alone.
set -eu

SRC=$(cd "$(dirname "$0")/.." && pwd)
BROWSE=${1:-$SRC/browse}
OUT="$SRC/dist/screens"
export DISPLAY=:99 LIBGL_ALWAYS_SOFTWARE=1
[ -x "$BROWSE" ] || { echo "build browse first: go build -o browse ." >&2; exit 1; }
for tool in Xvfb kitty xdotool tmux import montage; do
	command -v "$tool" >/dev/null || { echo "needs $tool" >&2; exit 1; }
done
rm -rf "$OUT"
mkdir -p "$OUT"
export XDG_STATE_HOME="$OUT/state"

# A page that answers at once and finishes seconds later, to see loading.
python3 - <<'EOF' &
import http.server, time
class H(http.server.BaseHTTPRequestHandler):
    def do_GET(self):
        self.send_response(200); self.send_header("Content-Type", "text/html"); self.end_headers()
        self.wfile.write(b"<title>Slow</title>" + b" " * 1024); self.wfile.flush()
        time.sleep(6)
        self.wfile.write(b"<body style='background:#2b5;font:40px sans-serif'>loaded</body>")
    def log_message(self, *a): pass
http.server.ThreadingHTTPServer(("127.0.0.1", 8771), H).serve_forever()
EOF
SLOW=$!
Xvfb :99 -screen 0 1600x1000x24 >/dev/null 2>&1 &
XVFB=$!
trap 'kill $SLOW $XVFB 2>/dev/null; "$BROWSE" shutdown >/dev/null 2>&1 || true' EXIT
sleep 1

bg=000000 fg=e6e6e6
term() { # term COMMAND: runs a command in kitty on the display
	kitty -o font_size=13 -o "background=#$bg" -o "foreground=#$fg" -o remember_window_size=no \
		-o initial_window_width=1600 -o initial_window_height=1000 -o placement_strategy=top-left \
		-o window_padding_width=0 -o hide_window_decorations=yes -e sh -c "$1" >/dev/null 2>&1 &
	sleep 1
	xdotool search --sync --onlyvisible --class kitty | head -1 | xargs -I{} xdotool windowfocus --sync {}
}
shot() { sleep "${2:-0.8}"; import -window root "$OUT/$1.png"; }
type() { xdotool type --delay 30 "$1"; }
key() { xdotool key "$1"; }
quit() { key ctrl+q; sleep 1; pkill -x kitty || true; "$BROWSE" shutdown >/dev/null 2>&1 || true; sleep 1; }

term "$BROWSE"; sleep 8
shot 01-start 0
type example.com; shot 02-typed
key Return; shot 03-loaded 5
xdotool mousemove 800 30; shot 04-hover
xdotool mousemove 800 500; key ctrl+l; shot 05-box-over-page
key Escape; key ctrl+t; shot 06-new-tab 1.5
type 127.0.0.1:8771; key Return; shot 07-loading 2
shot 08-slow-loaded 7
quit

bg=fafafa fg=1a1a1a
term "$BROWSE example.com"; sleep 9
xdotool mousemove 800 30; shot 09-light-hover
key ctrl+l; shot 10-light-box
quit

bg=000000 fg=e6e6e6
term "SSH_CONNECTION=1 $BROWSE 'data:text/html,<body style=margin:0;background:%23fff><input autofocus style=position:absolute;left:100px;top:80px;width:300px;height:40px;font:28px/1.3+monospace>'"; sleep 9
type "hello world"; shot 11-remote-typed
quit

term "tmux -L screens -f /dev/null new -s screens 'tmux set -p allow-passthrough on; $BROWSE example.com'"; sleep 10
shot 12-tmux-loaded 0
key ctrl+l; shot 13-tmux-box
key ctrl+q; sleep 1; tmux -L screens kill-server 2>/dev/null || true; pkill -x kitty || true

montage -label '%f' "$OUT"/[0-9]*.png -tile 3x -geometry 640x400+4+4 -background '#555' "$OUT/sheet.png"
rm -rf "$OUT/state"
echo "$OUT/sheet.png"
