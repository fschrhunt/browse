// The viewer: the browser drawn in a terminal, with the bar (see bar.go) at
// the top and the active tab's page filling the rest. A blank tab shows the
// start screen instead, which is the address box on its own. The person's
// mouse, keyboard, and pastes go to the page, except for a few browser
// shortcuts. It runs fullscreen when a person types browse, and in a pane beside an
// agent's conversation through browse show.
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
	"syscall"
	"time"
	"unicode/utf8"

	"golang.org/x/sys/unix"
	"golang.org/x/term"
)

const (
	probeTimeout = time.Second
	doubleClick  = 400 * time.Millisecond
	moveGap      = 30 * time.Millisecond // at most about 30 mouse moves a second
	wheelStep    = 100                   // CSS pixels per wheel notch
	noteShown    = 4 * time.Second
)

// layout is the terminal's size and how the page maps onto it.
type layout struct {
	cols, rows   int
	cellW, cellH float64 // pixels per cell
	scale        float64 // device pixels per CSS pixel
}

// view is the page area below the bar, in CSS pixels.
func (l layout) view() View {
	return View{
		Width:  int(float64(l.cols) * l.cellW / l.scale),
		Height: int(float64(l.rows-barRows) * l.cellH / l.scale),
		Scale:  l.scale,
	}
}

// at turns a mouse position into a bar column (1-based), or a page point in
// CSS pixels when the position is below the bar.
func (l layout) at(ev mouseEvent, pixels bool) (col int, x, y float64, onPage bool) {
	if pixels {
		top := barRows * l.cellH
		col = int(float64(ev.X)/l.cellW) + 1
		if float64(ev.Y) <= top {
			return col, 0, 0, false
		}
		return col, float64(ev.X) / l.scale, (float64(ev.Y) - top) / l.scale, true
	}
	if ev.Y <= barRows {
		return ev.X, 0, 0, false
	}
	return ev.X, (float64(ev.X) - 0.5) * l.cellW / l.scale, (float64(ev.Y-barRows-1) + 0.5) * l.cellH / l.scale, true
}

// row is the terminal row of a mouse position, from 1.
func (l layout) row(ev mouseEvent, pixels bool) int {
	if pixels {
		return int(float64(ev.Y)/l.cellH) + 1
	}
	return ev.Y
}

// luminance is how light a color is, from 0 for black to 1 for white.
func luminance(c color.NRGBA) float64 {
	return (0.2126*float64(c.R) + 0.7152*float64(c.G) + 0.0722*float64(c.B)) / 255
}

// measure reads the terminal's size. fallbackW and fallbackH are a cell's
// pixel size for terminals that do not report it. BROWSE_SCALE overrides the
// guess of the display's scale, which is 2 when cells are tall.
func measure(fallbackW, fallbackH float64) (layout, error) {
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
	if l.cellH >= 28 {
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

// viewer is the state of one viewer.
type viewer struct {
	p      painter
	pixels bool
	lay    layout
	frame  Frame // the newest, for the bar
	caps   caps

	hoverCol int                 // the bar column the mouse is over, or 0 when not over the bar
	drawn    [buttonCount]string // how each button was last drawn
	pills    map[uint32]string   // where each grey rectangle was last drawn
	icons    map[string][]byte   // icon PNGs for this layout

	editing bool
	edit    []rune
	caret   int
	replace bool // the next typed character replaces the whole address

	startShown bool   // the start screen is drawn in place of the page
	boxShown   bool   // the address box is drawn
	lastPNG    []byte // the newest picture of the page, to show on leaving it
	pngURL     string // the address the newest picture was of

	cursorRow, cursorCol int // where the caret is drawn, or row 0 for nowhere

	held      string // the mouse button held down
	lastPress time.Time
	pressX    float64
	pressY    float64
	clicks    int
	lastMove  time.Time

	note      string // a message for the bar, such as an error
	noteUntil time.Time
	notes     chan string
}

// openStream asks the daemon for frames of the active tab sized to view. It
// returns the frames, and a func that stops the stream.
func openStream(view View) (<-chan Frame, func(), error) {
	conn, err := net.Dial("unix", socketPath())
	if err != nil {
		return nil, nil, err
	}
	if err := json.NewEncoder(conn).Encode(Request{Cmd: "stream", View: &view, Person: true}); err != nil {
		conn.Close()
		return nil, nil, err
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
	return frames, func() { conn.Close() }, nil
}

// do sends a request for the person, in the background, so the screen keeps
// updating while a page loads. Errors show on the bar.
func (v *viewer) do(req Request) {
	req.Person = true
	go func() {
		if _, err := send(socketPath(), req); err != nil {
			v.notes <- err.Error()
		}
	}()
}

// sendInput delivers input to the page right away, in order.
func (v *viewer) sendInput(in Input) {
	if _, err := send(socketPath(), Request{Cmd: "input", Input: &in, Person: true}); err != nil {
		v.say(err.Error())
	}
}

func (v *viewer) say(msg string) {
	v.note, v.noteUntil = msg, time.Now().Add(noteShown)
}

// runViewer shows the active tab in this terminal until the person quits with
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

	v := &viewer{p: painter{tmux: inTmux}, notes: make(chan string, 4)}
	v.caps = probe(v.p, in)
	if !v.caps.graphics {
		return errors.New("this terminal cannot show images; browse needs one with kitty graphics, such as kitty, Ghostty, or WezTerm")
	}
	v.p.files, v.pixels = v.caps.files, v.caps.pixels

	// Alternate screen, hidden cursor, mouse with motion in SGR format (in
	// pixels when the terminal can), and bracketed paste.
	setup := "\x1b[?1049h\x1b[?25l\x1b[?1003h\x1b[?1006h\x1b[?2004h"
	if v.pixels {
		setup += "\x1b[?1016h"
	}
	fmt.Print(setup)
	defer func() {
		for id := pageImage; id <= hoverImage; id++ {
			fmt.Print(v.p.remove(id))
		}
		fmt.Print("\x1b[0 q\x1b[?2004l\x1b[?1016l\x1b[?1006l\x1b[?1003l\x1b[?25h\x1b[?1049l")
	}()

	resized := make(chan os.Signal, 1)
	signal.Notify(resized, syscall.SIGWINCH)
	defer signal.Stop(resized)

	var parser inputParser
	for {
		v.lay, err = measure(v.caps.cellW, v.caps.cellH)
		if err != nil {
			return err
		}
		view := v.lay.view()
		view.Dark = luminance(v.caps.bg) < 0.5
		frames, stop, err := openStream(view)
		if err != nil {
			return err
		}
		// Clearing the screen clears its images, so everything is drawn anew.
		v.drawn, v.pills, v.icons = [buttonCount]string{}, map[uint32]string{}, map[string][]byte{}
		v.startShown, v.boxShown = false, false
		fmt.Print("\x1b[2J" + v.screen())
		if v.p.tmux {
			fmt.Print(placeholders(pageImage, barRows+1, 1, v.lay.cols, v.lay.rows-barRows))
		}

		quit, err := v.loop(frames, in, resized, &parser)
		stop()
		if quit || err != nil {
			return err
		}
	}
}

// loop draws frames and handles input until the terminal is resized, which
// returns (false, nil), or the viewer should end.
func (v *viewer) loop(frames <-chan Frame, in <-chan []byte, resized <-chan os.Signal, parser *inputParser) (bool, error) {
	tick := time.NewTicker(time.Second)
	defer tick.Stop()
	animate := time.NewTicker(40 * time.Millisecond) // the loading band's frames
	defer animate.Stop()
	for {
		select {
		case f, ok := <-frames:
			if !ok {
				return true, errors.New("the browser stopped")
			}
			if f.Tab == "" {
				return true, nil
			}
			v.frame = f
			if f.PNG != nil {
				v.lastPNG, v.pngURL = f.PNG, f.URL
			}
			fmt.Print(v.page(f.PNG != nil) + v.screen())
		case b, ok := <-in:
			if !ok {
				return true, nil
			}
			for _, ev := range parser.feed(b) {
				if v.handle(ev) {
					return true, nil
				}
			}
			fmt.Print(v.screen())
		case msg := <-v.notes:
			v.say(msg)
			fmt.Print(v.screen())
		case <-tick.C:
			fmt.Print(v.screen()) // lets an old note expire
		case <-animate.C:
			if v.frame.Loading {
				fmt.Print(v.screen())
			}
		case <-resized:
			return false, nil
		}
	}
}

// handle acts on one input event, and reports whether the viewer should quit.
func (v *viewer) handle(ev any) bool {
	switch ev := ev.(type) {
	case keyPress:
		switch {
		case ev.Ctrl && ev.Key == "q":
			return true
		case v.shortcut(ev):
		case v.editing || v.onStart():
			if !v.editing {
				v.startEdit("")
			}
			v.editKey(ev)
		default:
			v.sendInput(Input{Kind: "key", Key: ev.Key, Modifiers: ev.modifiers()})
		}
	case pasteEvent:
		if !v.editing && v.onStart() {
			v.startEdit("")
		}
		if v.editing {
			v.insert(strings.ReplaceAll(ev.Text, "\n", " "))
		} else {
			v.sendInput(Input{Kind: "text", Text: ev.Text})
		}
	case mouseEvent:
		v.mouse(ev)
	}
	return false
}

// shortcut acts on a browser shortcut and reports whether k was one.
func (v *viewer) shortcut(k keyPress) bool {
	switch {
	case k.Ctrl && k.Key == "l":
		v.startEdit(v.frame.URL)
	case k.Ctrl && k.Key == "t":
		v.newTab()
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

// newTab opens a blank tab, whose start screen takes the address.
func (v *viewer) newTab() {
	v.do(Request{Cmd: "open", URL: "about:blank"})
	v.startEdit("")
}

// onStart reports whether the start screen shows: while the tab is blank, and
// after an address is entered there, until the new page's first picture.
func (v *viewer) onStart() bool {
	if v.frame.Tab == "" {
		return false
	}
	return v.frame.URL == "about:blank" || v.startShown && v.pngURL != v.frame.URL
}

// startEdit starts editing the address with text in it, all selected.
func (v *viewer) startEdit(text string) {
	if text == "about:blank" {
		text = ""
	}
	v.editing = true
	v.edit = []rune(text)
	v.caret = len(v.edit)
	v.replace = text != ""
}

func (v *viewer) insert(s string) {
	if v.replace {
		v.edit, v.caret, v.replace = nil, 0, false
	}
	r := []rune(s)
	v.edit = append(v.edit[:v.caret], append(r, v.edit[v.caret:]...)...)
	v.caret += len(r)
}

// editKey handles a key while the address has the keyboard.
func (v *viewer) editKey(k keyPress) {
	switch {
	case k.Key == "Enter":
		v.editing = false
		addr := strings.TrimSpace(string(v.edit))
		if addr != "" {
			v.do(Request{Cmd: "goto", URL: addr})
		}
	case k.Key == "Escape":
		v.editing = false
	case k.Key == "Backspace":
		if v.replace {
			v.edit, v.caret, v.replace = nil, 0, false
		} else if v.caret > 0 {
			v.edit = append(v.edit[:v.caret-1], v.edit[v.caret:]...)
			v.caret--
		}
	case k.Key == "Delete":
		if v.caret < len(v.edit) {
			v.edit = append(v.edit[:v.caret], v.edit[v.caret+1:]...)
		}
	case k.Key == "ArrowLeft":
		v.caret, v.replace = max(v.caret-1, 0), false
	case k.Key == "ArrowRight":
		v.caret, v.replace = min(v.caret+1, len(v.edit)), false
	case k.Key == "Home" || (k.Ctrl && k.Key == "a"):
		v.caret, v.replace = 0, false
	case k.Key == "End" || (k.Ctrl && k.Key == "e"):
		v.caret, v.replace = len(v.edit), false
	case k.Ctrl && k.Key == "u":
		v.edit, v.caret, v.replace = nil, 0, false
	case !k.Ctrl && !k.Alt && utf8.RuneCountInString(k.Key) == 1:
		v.insert(k.Key)
	}
}

// mouse handles one mouse event: on the bar it presses its buttons, on the
// page it goes to the page.
func (v *viewer) mouse(ev mouseEvent) {
	col, x, y, onPage := v.lay.at(ev, v.pixels)
	v.hoverCol = 0
	press := !ev.Release && !ev.Motion && ev.Wheel == 0
	switch {
	case !onPage:
		v.hoverCol = col
		if press {
			v.barClick(col)
		}
		return
	case v.onStart():
		// The start screen is all address box, so a click there types in it.
		if press {
			v.startEdit("")
		}
		return
	case v.editing:
		// A click outside the box puts the address away; inside it, nothing.
		if press && !v.inBox(col, v.lay.row(ev, v.pixels)) {
			v.editing = false
		}
		return
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
		in := Input{Kind: "mouse", Type: "mouseWheel", X: x, Y: y, DeltaY: float64(ev.Wheel * wheelStep), Modifiers: mods}
		if ev.Shift {
			in.DeltaX, in.DeltaY = in.DeltaY, 0
		}
		v.sendInput(in)
	case ev.Motion:
		if time.Since(v.lastMove) < moveGap {
			return
		}
		v.lastMove = time.Now()
		v.sendInput(Input{Kind: "mouse", Type: "mouseMoved", X: x, Y: y, Button: v.held, Modifiers: mods})
	case ev.Release:
		if v.held != "" {
			v.sendInput(Input{Kind: "mouse", Type: "mouseReleased", X: x, Y: y, Button: v.held, Clicks: v.clicks, Modifiers: mods})
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
		v.sendInput(Input{Kind: "mouse", Type: "mousePressed", X: x, Y: y, Button: button, Clicks: v.clicks, Modifiers: mods})
	}
}

func abs(f float64) float64 {
	if f < 0 {
		return -f
	}
	return f
}

// fit cuts s to at most width columns, ending in an ellipsis when cut.
func fit(s string, width int) string {
	r := []rune(s)
	if len(r) <= width {
		return s
	}
	if width < 1 {
		return ""
	}
	return string(r[:width-1]) + "…"
}

const (
	dim    = "\x1b[2m"
	accent = "\x1b[38;5;208m"
	reset  = "\x1b[0m"
)

// page draws the page area when it changes between the page and the start
// screen, and the page's newest picture when there is a new one or the page
// comes back.
func (v *viewer) page(newPNG bool) string {
	var out string
	start := v.onStart()
	switch {
	case start && !v.startShown:
		out = v.p.remove(pageImage)
		blank := strings.Repeat(" ", v.lay.cols)
		for row := barRows + 1; row <= v.lay.rows; row++ {
			out += fmt.Sprintf("\x1b[%d;1H%s", row, blank)
		}
		v.boxShown = false // the box's text was cleared with the page
	case !start && v.startShown:
		v.editing, v.edit = false, nil // what was typed in the box is done with
		if v.p.tmux {
			out = placeholders(pageImage, barRows+1, 1, v.lay.cols, v.lay.rows-barRows)
		}
		newPNG = v.lastPNG != nil
	}
	v.startShown = start
	if newPNG && !start {
		// Beneath the text, so the address box can sit on the page.
		out += fmt.Sprintf("\x1b[%d;1H", barRows+1) + v.p.layer(pageImage, v.lastPNG, v.lay.cols, v.lay.rows-barRows, -2)
	}
	return out
}

// editField draws the address being edited in width columns, scrolled so the
// caret stays in view, with the whole address underlined while it is
// selected. It returns the field and the caret's column within it.
func (v *viewer) editField(width int) (string, int) {
	start := max(v.caret-width+1, 0)
	end := min(start+width, len(v.edit))
	text := string(v.edit[start:end])
	if v.replace {
		text = "\x1b[4m" + text + reset
	}
	return text + strings.Repeat(" ", width-(end-start)), v.caret - start
}
