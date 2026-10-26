package main

import (
	"reflect"
	"strings"
	"testing"
	"unicode/utf8"
)

// The kitty protocol caps each payload at 4096 bytes. Only the first chunk
// carries the image parameters, and m=1 marks every chunk but the last.
func TestImageChunksPayload(t *testing.T) {
	png := make([]byte, kittyChunk*2) // base64 makes this three chunks

	out := painter{}.image(7, png, 80, 24)

	chunks := strings.Split(out, "\x1b\\")
	chunks = chunks[:len(chunks)-1]
	if len(chunks) != 3 {
		t.Fatalf("want 3 chunks, got %d", len(chunks))
	}
	if !strings.HasPrefix(chunks[0], "\x1b_Ga=T,f=100,i=7,") || !strings.Contains(chunks[0], ",c=80,r=24,q=2,m=1;") {
		t.Errorf("first chunk header: %q", chunks[0][:60])
	}
	if !strings.HasPrefix(chunks[1], "\x1b_Gm=1;") || !strings.HasPrefix(chunks[2], "\x1b_Gm=0;") {
		t.Errorf("later chunk headers: %q %q", chunks[1][:8], chunks[2][:8])
	}
}

// Inside tmux, images are placed virtually and every sequence goes through
// passthrough, with its escapes doubled.
func TestImageInsideTmux(t *testing.T) {
	out := painter{tmux: true}.image(7, []byte("png"), 10, 5)

	if !strings.HasPrefix(out, "\x1bPtmux;\x1b\x1b_Ga=T,") || !strings.HasSuffix(out, "\x1b\x1b\\\x1b\\") {
		t.Errorf("not wrapped for tmux: %q", out)
	}
	if !strings.Contains(out, ",U=1,c=10,r=5,") {
		t.Errorf("not a virtual placement: %q", out)
	}
}

// A placeholder cell carries the image id as its color and the image's row
// and column as diacritics.
func TestPlaceholderCell(t *testing.T) {
	out := placeholder(0x0a0b0c, 1, 2)

	if !strings.HasPrefix(out, "\x1b[38;2;10;11;12m") {
		t.Errorf("cell should be colored with the image id: %q", out)
	}
	rest := strings.TrimPrefix(out, "\x1b[38;2;10;11;12m")
	r, size := utf8.DecodeRuneInString(rest)
	row, size2 := utf8.DecodeRuneInString(rest[size:])
	col, _ := utf8.DecodeRuneInString(rest[size+size2:])
	if r != 0x10EEEE || row != rowColumnDiacritics[1] || col != rowColumnDiacritics[2] {
		t.Errorf("cell %U %U %U", r, row, col)
	}
}

func TestParserReadsKeysAndMouse(t *testing.T) {
	var p inputParser
	got := p.feed([]byte("a\r\x1b[1;3D\x1b[5;5~\x7f\x0c\x1b[<0;10;5M\x1b[<0;10;5m\x1b[<65;3;4M\x1b[<35;7;8M\x1b"))

	want := []any{
		keyPress{Key: "a"},
		keyPress{Key: "Enter"},
		keyPress{Key: "ArrowLeft", Alt: true},
		keyPress{Key: "PageUp", Ctrl: true},
		keyPress{Key: "Backspace"},
		keyPress{Key: "l", Ctrl: true},
		mouseEvent{X: 10, Y: 5},
		mouseEvent{X: 10, Y: 5, Release: true},
		mouseEvent{X: 3, Y: 4, Button: 1, Wheel: 1},
		mouseEvent{X: 7, Y: 8, Button: 3, Motion: true},
		keyPress{Key: "Escape"},
	}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("got\n%#v\nwant\n%#v", got, want)
	}
}

func TestParserJoinsAPasteAcrossReads(t *testing.T) {
	var p inputParser
	first := p.feed([]byte("\x1b[200~hello "))
	second := p.feed([]byte("world\x1b[201~x"))

	if len(first) != 0 {
		t.Errorf("events before the paste ended: %v", first)
	}
	want := []any{pasteEvent{Text: "hello world"}, keyPress{Key: "x"}}
	if !reflect.DeepEqual(second, want) {
		t.Errorf("got %#v", second)
	}
}

// testViewer is a viewer of a 100x40 terminal with 10x20 cells at scale 2,
// with pixel mouse reports, whose inputs are kept instead of sent.
func testViewer() (*viewer, *[]Input) {
	var sent []Input
	v := &viewer{lay: layout{cols: 100, rows: 40, cellW: 10, cellH: 20, scale: 2}, pixels: true}
	v.send = func(in Input) { sent = append(sent, in) }
	return v, &sent
}

// Mouse positions map to CSS pixels, whether the terminal reports pixels or
// cells, and the view the daemon gets describes the terminal.
func TestLayoutMapsMouse(t *testing.T) {
	l := layout{cols: 100, rows: 40, cellW: 10, cellH: 20, scale: 2}

	if x, y := l.at(mouseEvent{X: 400, Y: 260}, true); x != 200 || y != 130 {
		t.Errorf("pixels: %v,%v", x, y)
	}
	if x, y := l.at(mouseEvent{X: 1, Y: 4}, false); x != 2.5 || y != 35 {
		t.Errorf("cells: %v,%v", x, y)
	}
	view := l.view(caps{}, false)
	if w, h := view.screen(); w != 500 || h != 400 {
		t.Errorf("screen %dx%d", w, h)
	}
	if w, h := view.page(); w != 500 || h != 370 || view.barHeight() != 30 {
		t.Errorf("page %dx%d under a %v bar", w, h, view.barHeight())
	}
}

// A click goes to the bar, the box while it shows, or the page, each in its
// own coordinates. Leaving the bar tells it so, and a click on the page
// closes the box.
func TestMouseRoutesByPlace(t *testing.T) {
	v, sent := testViewer()
	v.frame = Frame{Tab: "t1", Editing: true}
	bx, by, _, _ := v.lay.view(caps{}, false).boxPx() // 100,110 CSS px: column 21, row 12

	v.mouse(mouseEvent{X: 400, Y: 20})                        // the bar
	v.mouse(mouseEvent{X: int(bx*2) + 40, Y: int(by*2) + 20}) // the box
	v.mouse(mouseEvent{X: 400, Y: 700})                       // the page
	var kinds []string
	for _, in := range *sent {
		kinds = append(kinds, in.Kind+":"+in.To)
	}
	want := []string{"mouse:bar", "mouse:bar", "mouse:box", "blur:", "mouse:page"}
	if !reflect.DeepEqual(kinds, want) {
		t.Fatalf("sent %v, want %v", kinds, want)
	}
	if bar := (*sent)[0]; bar.X != 200 || bar.Y != 10 {
		t.Errorf("bar click %+v", bar)
	}
	if leave := (*sent)[1]; leave.Type != "mouseMoved" || leave.Y != 29.5 {
		t.Errorf("leaving the bar %+v", leave)
	}
	if box := (*sent)[2]; box.X != 20 || box.Y != 10 {
		t.Errorf("box click %+v, want box coordinates", box)
	}
	if page := (*sent)[4]; page.X != 200 || page.Y != 320 {
		t.Errorf("page click %+v, want page coordinates under a 30px bar", page)
	}
}

// Keys go to the box while it has the keyboard, and Ctrl+L asks for the box
// without reaching either.
func TestKeysFollowEditing(t *testing.T) {
	v, sent := testViewer()
	v.frame = Frame{Tab: "t1"}
	v.handle(keyPress{Key: "a"})
	v.frame.Editing = true
	v.handle(keyPress{Key: "b"})
	v.handle(keyPress{Key: "l", Ctrl: true})

	want := []Input{{Kind: "key", To: "page", Key: "a"}, {Kind: "key", To: "box", Key: "b"}, {Kind: "edit"}}
	if !reflect.DeepEqual(*sent, want) {
		t.Errorf("sent %+v", *sent)
	}
}

// Inside tmux, the bar's rows show the bar, the box's cells the box while it
// shows, and the rest the page from its own first row.
func TestGridPicksImagePerCell(t *testing.T) {
	v, _ := testViewer()
	v.p.tmux = true
	grid := v.grid(Frame{Tab: "t1", Editing: true})

	if !strings.Contains(grid, placeholder(barImage, 0, 0)) {
		t.Error("the bar's first cell should show the bar")
	}
	if !strings.Contains(grid, placeholder(pageImage, 0, 0)) {
		t.Error("the first page row should show the page's first row")
	}
	col, row, _, _ := v.lay.view(caps{}, false).box()
	if !strings.Contains(grid, placeholder(boxImage, 0, 0)) || strings.Contains(grid, placeholder(pageImage, row-1-barRows, col-1)) {
		t.Error("the box's cells should show the box, not the page")
	}
	if strings.Contains(v.grid(Frame{Tab: "t1"}), placeholder(boxImage, 0, 0)) {
		t.Error("the box should not show while it is closed")
	}
}

// Closing the box takes its picture away and leaves the page; the start
// screen takes the last page's picture away.
func TestDrawRemovesWhatLeft(t *testing.T) {
	v, _ := testViewer()
	v.draw(Frame{Tab: "t1", Layer: "box", PNG: []byte("png"), Editing: true})
	out := v.draw(Frame{Tab: "t1"})
	if !strings.Contains(out, v.p.remove(boxImage)) || strings.Contains(out, v.p.remove(pageImage)) {
		t.Errorf("closing the box: %q", out)
	}
	out = v.draw(Frame{Tab: "t2", Start: true, Editing: true})
	if !strings.Contains(out, v.p.remove(pageImage)) {
		t.Errorf("the start screen should remove the page: %q", out)
	}
}

// Ctrl+C asks for the selection where the keyboard is, and the viewer puts
// what comes back on the clipboard through the terminal.
func TestCopyGoesToClipboard(t *testing.T) {
	v, sent := testViewer()
	v.frame = Frame{Tab: "t1"}
	v.handle(keyPress{Key: "c", Ctrl: true})
	if !reflect.DeepEqual(*sent, []Input{{Kind: "copy", To: "page"}}) {
		t.Errorf("sent %+v", *sent)
	}
	v.frame.Copy = "hi"
	if out := v.draw(v.frame); out != "" {
		t.Errorf("draw should not write the clipboard itself: %q", out)
	}
}
