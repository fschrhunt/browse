// The viewer: the browser drawn in a terminal. It shows three pictures from
// the daemon, the page, the bar above it, and the address box over it while
// it is open, and sends the person's mouse, keyboard, and pastes to
// whichever they are for. A few shortcuts are the viewer's own. It runs fullscreen when a person types browse, and
// in a pane beside an agent's conversation through browse show.
package main

import (
	"bufio"
	"bytes"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"image"
	"image/color"
	"image/png"
	"net"
	"os"
	"os/exec"
	"os/signal"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"

	"golang.org/x/sys/unix"
	"golang.org/x/term"
)

const (
	probeTimeout = time.Second
	doubleClick  = 400 * time.Millisecond
	moveGap      = 30 * time.Millisecond // at most about 30 mouse moves a second
	wheelStep    = 100                   // CSS pixels per wheel notch
)

// layout is the terminal's size and how pictures map onto it.
type layout struct {
	cols, rows   int
	cellW, cellH float64 // device pixels per cell
	scale        float64 // device pixels per CSS pixel
}

// view describes the terminal to the daemon.
func (l layout) view(c caps, remote bool) View {
	hex := func(c color.NRGBA) string { return fmt.Sprintf("#%02x%02x%02x", c.R, c.G, c.B) }
	return View{Cols: l.cols, Rows: l.rows, CellW: l.cellW, CellH: l.cellH, Scale: l.scale, FG: hex(c.fg), BG: hex(c.bg), Remote: remote}
}

// at turns a mouse position into CSS pixels on the screen. Terminals report
// pixels when they can, else cells, whose center stands in.
func (l layout) at(ev mouseEvent, pixels bool) (x, y float64) {
	if pixels {
		return float64(ev.X) / l.scale, float64(ev.Y) / l.scale
	}
	return (float64(ev.X) - 0.5) * l.cellW / l.scale, (float64(ev.Y) - 0.5) * l.cellH / l.scale
}

// measure reads the terminal's size. fallbackW and fallbackH are a cell's
// pixel size for terminals that do not report it. BROWSE_SCALE overrides the
// guess of the display's scale, which is 2 when cells are tall; over a
// network it stays 1, since frames are then four times smaller.
func measure(fallbackW, fallbackH float64, remote bool) (layout, error) {
	ws, err := unix.IoctlGetWinsize(int(os.Stdout.Fd()), unix.TIOCGWINSZ)
	if err != nil {
		return layout{}, err
	}
	l := layout{cols: int(ws.Col), rows: int(ws.Row), cellW: fallbackW, cellH: fallbackH}
	if ws.Xpixel > 0 && ws.Ypixel > 0 {
		l.cellW = float64(ws.Xpixel) / float64(ws.Col)
		l.cellH = float64(ws.Ypixel) / float64(ws.Row)
	}
	l.scale = 1
	if l.cellH >= 28 && !remote {
		l.scale = 2
	}
	if v, err := strconv.ParseFloat(os.Getenv("BROWSE_SCALE"), 64); err == nil && v > 0 {
		l.scale = v
	}
	if l.rows < barRows+2 || l.cols < 30 {
		return l, errors.New("the terminal is too small")
	}
	return l, nil
}

// caps is what the terminal can do, and its text and background colors.
type caps struct {
	graphics     bool
	files        bool
	pixels       bool // mouse reports in pixels
	cellW, cellH float64
	fg, bg       color.NRGBA
}

var (
	graphicsOK = regexp.MustCompile(`\x1b_Gi=31;OK`)
	filesOK    = regexp.MustCompile(`\x1b_Gi=32;OK`)
	pixelsMode = regexp.MustCompile(`\x1b\[\?1016;([0-9])\$y`)
	cellSize   = regexp.MustCompile(`\x1b\[6;([0-9]+);([0-9]+)t`)
	deviceAttr = regexp.MustCompile(`\x1b\[\?[0-9;]*c`)
	termColor  = regexp.MustCompile(`\x1b\]1([01]);rgb:([0-9a-fA-F]+)/([0-9a-fA-F]+)/([0-9a-fA-F]+)`)
)

// probe asks the terminal what it supports, and reads the answers from in
// until the device attributes reply, which every terminal sends last.
func probe(p painter, in <-chan []byte) caps {
	c := caps{cellW: 8, cellH: 16, fg: color.NRGBA{208, 208, 208, 255}, bg: color.NRGBA{0, 0, 0, 255}}
	// A 1x1 PNG in a file tests whether the terminal can read files.
	var buf bytes.Buffer
	png.Encode(&buf, image.NewGray(image.Rect(0, 0, 1, 1)))
	file, _ := tempImage(buf.Bytes())
	defer os.Remove(file)

	fmt.Print(
		p.wrap("\x1b_Gi=31,s=1,v=1,a=q,t=d,f=24;AAAA\x1b\\") +
			p.wrap(fmt.Sprintf("\x1b_Gi=32,a=q,t=t,f=100;%s\x1b\\", base64.StdEncoding.EncodeToString([]byte(file)))) +
			"\x1b[?1016$p\x1b[16t\x1b]10;?\x1b\\\x1b]11;?\x1b\\\x1b[c")

	var got []byte
	deadline := time.After(probeTimeout)
read:
	for !deviceAttr.Match(got) {
		select {
		case b := <-in:
			got = append(got, b...)
		case <-deadline:
			break read
		}
	}
	// tmux may not pass the terminal's answers back; images still work there,
	// through passthrough, so assume them.
	c.graphics = graphicsOK.Match(got) || p.tmux
	c.files = filesOK.Match(got)
	if m := pixelsMode.FindSubmatch(got); m != nil {
		c.pixels = m[1][0] == '1' || m[1][0] == '2'
	}
	if m := cellSize.FindSubmatch(got); m != nil {
		h, _ := strconv.ParseFloat(string(m[1]), 64)
		w, _ := strconv.ParseFloat(string(m[2]), 64)
		if w > 0 && h > 0 {
			c.cellW, c.cellH = w, h
		}
	}
	for _, m := range termColor.FindAllSubmatch(got, -1) {
		rgb := color.NRGBA{A: 255}
		for i, part := range []*uint8{&rgb.R, &rgb.G, &rgb.B} {
			v, _ := strconv.ParseUint(string(m[i+2]), 16, 64)
			*part = uint8(v * 255 / (1<<(4*len(m[i+2])) - 1))
		}
		if m[1][0] == '0' {
			c.fg = rgb
		} else {
			c.bg = rgb
		}
	}
	return c
}

// stream is the viewer's connection to the daemon: frames come in, inputs go out.
type stream struct {
	conn   net.Conn
	frames <-chan Frame
	mu     sync.Mutex
	enc    *json.Encoder
}

// openStream asks the daemon for frames for view.
func openStream(view View) (*stream, error) {
	conn, err := net.Dial("unix", socketPath())
	if err != nil {
		return nil, err
	}
	enc := json.NewEncoder(conn)
	if err := enc.Encode(Request{Cmd: "stream", View: &view, Person: true}); err != nil {
		conn.Close()
		return nil, err
	}
	frames := make(chan Frame, 2)
	go func() {
		defer close(frames)
		dec := json.NewDecoder(bufio.NewReader(conn))
		for {
			var f Frame
			if err := dec.Decode(&f); err != nil {
				return
			}
			frames <- f
		}
	}()
	return &stream{conn: conn, frames: frames, enc: enc}, nil
}

// send delivers an input to the daemon, in order.
func (st *stream) send(in Input) {
	st.mu.Lock()
	defer st.mu.Unlock()
	st.enc.Encode(in)
}

func (st *stream) close() { st.conn.Close() }

// viewer is the state of one viewer.
type viewer struct {
	p      painter
	pixels bool
	lay    layout
	caps   caps
	st     *stream
	send   func(Input) // delivers an input to the daemon
	frame  Frame       // the newest state

	over      string // what the mouse was last over: "bar", "box", or "page"
	boxShown  bool   // the box is on screen
	onStart   bool   // the start screen is on
	held      string
	lastPress time.Time
	pressX    float64
	pressY    float64
	clicks    int
	lastMove  time.Time
	cells     string // the placeholder grid last drawn inside tmux
}

// do sends a command for the person, in the background, so the screen keeps
// updating while a page loads. Errors show under the bar, through the daemon.
func (v *viewer) do(req Request) {
	req.Person = true
	go send(socketPath(), req)
}

// runViewer shows the browser in this terminal until the person quits with
// Ctrl+Q, or every tab is closed.
func runViewer() error {
	if !term.IsTerminal(int(os.Stdin.Fd())) || !term.IsTerminal(int(os.Stdout.Fd())) {
		return errors.New("the viewer needs a terminal")
	}
	// A daemon left from before an update restarts here, before the stream opens.
	if _, err := callDaemon(Request{Cmd: "ls", Person: true}); err != nil {
		return err
	}
	inTmux := os.Getenv("TMUX") != ""
	if inTmux {
		// Let images through to the terminal outside tmux.
		args := []string{"set", "-p", "allow-passthrough", "on"}
		if pane := os.Getenv("TMUX_PANE"); pane != "" {
			args = []string{"set", "-p", "-t", pane, "allow-passthrough", "on"}
		}
		exec.Command("tmux", args...).Run()
	}
	remote := os.Getenv("SSH_CONNECTION") != "" || os.Getenv("SSH_TTY") != ""

	state, err := term.MakeRaw(int(os.Stdin.Fd()))
	if err != nil {
		return err
	}
	defer term.Restore(int(os.Stdin.Fd()), state)

	in := make(chan []byte, 16)
	go func() {
		for {
			buf := make([]byte, 4096)
			n, err := os.Stdin.Read(buf)
			if err != nil {
				close(in)
				return
			}
			in <- buf[:n]
		}
	}()

	v := &viewer{p: painter{tmux: inTmux}}
	v.caps = probe(v.p, in)
	if !v.caps.graphics {
		return errors.New("this terminal cannot show images; browse needs one with kitty graphics, such as kitty, Ghostty, or WezTerm")
	}
	v.p.files, v.pixels = v.caps.files && !remote, v.caps.pixels

	// Alternate screen, hidden cursor, mouse with motion in SGR format (in
	// pixels when the terminal can), and bracketed paste.
	setup := "\x1b[?1049h\x1b[?25l\x1b[?1003h\x1b[?1006h\x1b[?2004h"
	if v.pixels {
		setup += "\x1b[?1016h"
	}
	fmt.Print(setup)
	defer fmt.Print(v.p.remove(pageImage) + v.p.remove(barImage) + v.p.remove(boxImage) + "\x1b[?2004l\x1b[?1016l\x1b[?1006l\x1b[?1003l\x1b[?25h\x1b[?1049l")

	resized := make(chan os.Signal, 1)
	signal.Notify(resized, syscall.SIGWINCH)
	defer signal.Stop(resized)

	var parser inputParser
	for {
		v.lay, err = measure(v.caps.cellW, v.caps.cellH, remote)
		if err != nil {
			return err
		}
		v.st, err = openStream(v.lay.view(v.caps, remote))
		if err != nil {
			return err
		}
		v.send = v.st.send
		fmt.Print("\x1b[2J")
		v.cells = ""
		quit, err := v.loop(in, resized, &parser)
		v.st.close()
		if quit || err != nil {
			return err
		}
	}
}

// loop draws frames and handles input until the terminal is resized, which
// returns (false, nil), or the viewer should end.
func (v *viewer) loop(in <-chan []byte, resized <-chan os.Signal, parser *inputParser) (bool, error) {
	for {
		select {
		case f, ok := <-v.st.frames:
			if !ok {
				return true, errors.New("the browser stopped")
			}
			if f.Tab == "" {
				return true, nil
			}
			v.frame = f
			fmt.Print(v.draw(f))
		case b, ok := <-in:
			if !ok {
				return true, nil
			}
			for _, ev := range parser.feed(b) {
				if v.handle(ev) {
					return true, nil
				}
			}
		case <-resized:
			return false, nil
		}
	}
}

// draw places a frame's picture: the page under the bar, the bar at the
// top, the box at its place while it shows. On the start screen the page
// area is bare. Inside tmux, where a cell shows one image, the cells are
// given out the same way.
func (v *viewer) draw(f Frame) string {
	var out string
	view := v.lay.view(v.caps, false)
	col, row, cols, rows := view.box()
	showBox := f.Editing || f.Start
	switch f.Layer {
	case "page":
		if !f.Start {
			out = fmt.Sprintf("\x1b[%d;1H", barRows+1) + v.p.layer(pageImage, f.PNG, v.lay.cols, v.lay.rows-barRows, 0)
		}
	case "bar":
		out = "\x1b[1;1H" + v.p.layer(barImage, f.PNG, v.lay.cols, barRows, 1)
	case "box":
		if showBox {
			out = fmt.Sprintf("\x1b[%d;%dH", row, col) + v.p.layer(boxImage, f.PNG, cols, rows, 2)
		}
	}
	if f.Start && !v.onStart {
		out += v.p.remove(pageImage) // the last page's picture goes
	}
	if !showBox && v.boxShown {
		out += v.p.remove(boxImage)
	}
	v.boxShown, v.onStart = showBox, f.Start
	if v.p.tmux {
		if cells := v.grid(f); cells != v.cells {
			v.cells = cells
			out += cells
		}
	}
	return out
}

// grid is tmux's placeholder cells for a frame: the bar's rows show the bar,
// the box's cells the box while it shows, the rest the page from its own
// first row, or nothing on the start screen.
func (v *viewer) grid(f Frame) string {
	col, row, cols, rows := v.lay.view(v.caps, false).box()
	showBox := f.Editing || f.Start
	var b strings.Builder
	for r := 1; r <= v.lay.rows; r++ {
		fmt.Fprintf(&b, "\x1b[%d;1H", r)
		for c := 1; c <= v.lay.cols; c++ {
			switch {
			case r <= barRows:
				b.WriteString(placeholder(barImage, r-1, c-1))
			case showBox && r >= row && r < row+rows && c >= col && c < col+cols:
				b.WriteString(placeholder(boxImage, r-row, c-col))
			case f.Start:
				b.WriteString(" ")
			default:
				b.WriteString(placeholder(pageImage, r-1-barRows, c-1))
			}
		}
	}
	return b.String() + "\x1b[0m"
}

// handle acts on one input event, and reports whether the viewer should quit.
func (v *viewer) handle(ev any) bool {
	switch ev := ev.(type) {
	case keyPress:
		switch {
		case ev.Ctrl && ev.Key == "q":
			return true
		case v.shortcut(ev):
		default:
			v.send(Input{Kind: "key", To: v.keyTarget(), Key: ev.Key, Modifiers: ev.modifiers()})
		}
	case pasteEvent:
		text := ev.Text
		if v.frame.Editing {
			text = strings.ReplaceAll(text, "\n", " ")
		}
		v.send(Input{Kind: "text", To: v.keyTarget(), Text: text})
	case mouseEvent:
		v.mouse(ev)
	}
	return false
}

// keyTarget is where keys go: the box while it has the keyboard, else the page.
func (v *viewer) keyTarget() string {
	if v.frame.Editing {
		return "box"
	}
	return "page"
}

// shortcut acts on a browser shortcut and reports whether k was one.
func (v *viewer) shortcut(k keyPress) bool {
	switch {
	case k.Ctrl && k.Key == "l":
		v.send(Input{Kind: "edit"})
	case k.Ctrl && k.Key == "t":
		v.do(Request{Cmd: "open", URL: "about:blank"})
	case k.Ctrl && k.Key == "w":
		v.do(Request{Cmd: "close"})
	case k.Ctrl && k.Key == "r":
		v.do(Request{Cmd: "reload"})
	case k.Alt && k.Key == "ArrowLeft":
		v.do(Request{Cmd: "back"})
	case k.Alt && k.Key == "ArrowRight":
		v.do(Request{Cmd: "forward"})
	case k.Ctrl && k.Key == "PageUp":
		v.do(Request{Cmd: "cycle", Step: -1})
	case k.Ctrl && k.Key == "PageDown":
		v.do(Request{Cmd: "cycle", Step: 1})
	default:
		return false
	}
	return true
}

// mouse sends one mouse event to the bar, the box while it shows, or the
// page, in that one's own coordinates. Leaving the bar tells it so, so hover
// states clear. A click on the page while the box is open closes the box.
func (v *viewer) mouse(ev mouseEvent) {
	x, y := v.lay.at(ev, v.pixels)
	view := v.lay.view(v.caps, false)
	bx, by, bw, bh := view.boxPx()
	to := "page"
	switch {
	case y < view.barHeight():
		to = "bar"
	case (v.frame.Editing || v.frame.Start) && x >= bx && x < bx+bw && y >= by && y < by+bh:
		to = "box"
		x, y = x-bx, y-by
	case v.frame.Start:
		return // the start screen is the box; nothing else takes the mouse
	default:
		y -= view.barHeight()
	}
	press := !ev.Release && !ev.Motion && ev.Wheel == 0
	if v.over == "bar" && to != "bar" {
		v.send(Input{Kind: "mouse", To: "bar", Type: "mouseMoved", X: 0, Y: view.barHeight() - 0.5})
	}
	v.over = to
	if to == "page" && press && v.frame.Editing {
		v.send(Input{Kind: "blur"})
	}

	var mods []string
	for _, m := range []struct {
		on   bool
		name string
	}{{ev.Ctrl, "Control"}, {ev.Alt, "Alt"}, {ev.Shift, "Shift"}} {
		if m.on {
			mods = append(mods, m.name)
		}
	}
	button := map[int]string{0: "left", 1: "middle", 2: "right"}[ev.Button]

	switch {
	case ev.Wheel != 0:
		if to != "page" {
			return
		}
		in := Input{Kind: "mouse", Type: "mouseWheel", X: x, Y: y, DeltaY: float64(ev.Wheel * wheelStep), Modifiers: mods}
		if ev.Shift {
			in.DeltaX, in.DeltaY = in.DeltaY, 0
		}
		v.send(in)
	case ev.Motion:
		if time.Since(v.lastMove) < moveGap {
			return
		}
		v.lastMove = time.Now()
		v.send(Input{Kind: "mouse", To: to, Type: "mouseMoved", X: x, Y: y, Button: v.held, Modifiers: mods})
	case ev.Release:
		if v.held != "" {
			v.send(Input{Kind: "mouse", To: to, Type: "mouseReleased", X: x, Y: y, Button: v.held, Clicks: v.clicks, Modifiers: mods})
		}
		v.held = ""
	case button != "":
		near := abs(x-v.pressX) < 5 && abs(y-v.pressY) < 5
		if time.Since(v.lastPress) < doubleClick && near {
			v.clicks = min(v.clicks+1, 3)
		} else {
			v.clicks = 1
		}
		v.lastPress, v.pressX, v.pressY, v.held = time.Now(), x, y, button
		v.send(Input{Kind: "mouse", To: to, Type: "mousePressed", X: x, Y: y, Button: button, Clicks: v.clicks, Modifiers: mods})
	}
}

func abs(f float64) float64 {
	if f < 0 {
		return -f
	}
	return f
}
