package main

import (
	"fmt"
	"image/color"
	"reflect"
	"slices"
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

func TestPlaceholdersCoverTheImage(t *testing.T) {
	out := placeholders(0x0a0b0c, 2, 5, 3, 2)

	if !strings.HasPrefix(out, "\x1b[38;2;10;11;12m") {
		t.Errorf("cells should be colored with the image id: %q", out)
	}
	if got := strings.Count(out, string(rune(0x10EEEE))); got != 6 {
		t.Errorf("want 6 placeholder cells, got %d", got)
	}
	if !strings.Contains(out, "\x1b[2;5H") || !strings.Contains(out, "\x1b[3;5H") {
		t.Errorf("rows should start at row 2, column 5: %q", out)
	}
	// The second row's cells carry the diacritic for row 1.
	second := out[strings.Index(out, "\x1b[3;5H")+len("\x1b[3;5H"):]
	_, size := utf8.DecodeRuneInString(second)
	if r, _ := utf8.DecodeRuneInString(second[size:]); r != rowColumnDiacritics[1] {
		t.Errorf("row diacritic %U", r)
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

// Mouse positions below the bar map to CSS pixels in the page, whether the
// terminal reports pixels or cells.
func TestLayoutMapsMouseToPage(t *testing.T) {
	l := layout{cols: 100, rows: 40, cellW: 10, cellH: 20, scale: 2}

	if col, _, _, onPage := l.at(mouseEvent{X: 55, Y: 50}, true); onPage || col != 6 {
		t.Errorf("a pixel in the bar: col %d, on page %t", col, onPage)
	}
	if _, x, y, onPage := l.at(mouseEvent{X: 400, Y: 260}, true); !onPage || x != 200 || y != 100 {
		t.Errorf("pixels: %v,%v on page %t", x, y, onPage)
	}
	if _, _, _, onPage := l.at(mouseEvent{X: 1, Y: 3}, false); onPage {
		t.Error("the bar's last row is on the page")
	}
	if _, x, y, onPage := l.at(mouseEvent{X: 1, Y: 4}, false); !onPage || x != 2.5 || y != 5 {
		t.Errorf("cells: %v,%v on page %t", x, y, onPage)
	}
	if v := l.view(); v != (View{Width: 500, Height: 370, Scale: 2}) {
		t.Errorf("view %+v", v)
	}
}

// testViewer is a viewer of an 80x24 terminal, drawing nothing anywhere.
func testViewer() *viewer {
	return &viewer{
		lay:   layout{cols: 80, rows: 24, cellW: 10, cellH: 20, scale: 1},
		icons: map[string][]byte{}, pills: map[uint32]string{},
	}
}

// Tabs sit in the middle of the bar, labeled by site, with new tab right
// after the last one.
func TestTabsCenteredWithNewTabBeside(t *testing.T) {
	v := testViewer()
	v.frame.Tabs = []TabInfo{{Tab: "t1", URL: "https://www.example.com/a", Active: true}, {Tab: "t2", URL: "about:blank"}}

	items := v.items()
	first, second, plus := items[3], items[4], items[5]
	if first.label != "example.com" || second.label != "New tab" {
		t.Errorf("labels %q %q", first.label, second.label)
	}
	// The two tabs are 15 and 11 columns with a gap, 27 in all, centered in 80.
	if first.left != 27 || second.left != 43 {
		t.Errorf("tabs at columns %d and %d", first.left, second.left)
	}
	if plus.button != newTabButton || plus.left != 55 {
		t.Errorf("new tab at column %d", plus.left)
	}
}

// Back and forward are dimmed when the history does not go that way, reload
// turns into stop while the page loads, and new tab only shows while the
// mouse is over the bar.
func TestButtonsFollowHistoryAndHover(t *testing.T) {
	v := testViewer()
	v.frame = Frame{Back: true, Tabs: []TabInfo{{Tab: "t1", URL: "https://example.com/", Active: true}}}
	looks := func() []string {
		var out []string
		for _, d := range v.drawn {
			out = append(out, strings.TrimRight(strings.TrimRight(d, "0123456789"), " "))
		}
		return out
	}
	v.buttons(v.items())
	if got := looks(); !slices.Equal(got, []string{"back", "forward dim", "reload", ""}) {
		t.Errorf("drawn %q", got)
	}

	v.mouse(mouseEvent{X: 40, Y: 2, Button: 3, Motion: true})
	v.frame.Back, v.frame.Forward, v.frame.Loading = false, true, true
	v.buttons(v.items())
	if got := looks(); !slices.Equal(got, []string{"back dim", "forward", "stop", "new-tab"}) {
		t.Errorf("after hovering and going back: drawn %q", got)
	}
	if out := v.buttons(v.items()); out != "" {
		t.Errorf("unchanged buttons were drawn again: %q", out)
	}
}

// Clicking the active tab opens its address in the box, selected.
func TestClickActiveTabEditsAddress(t *testing.T) {
	v := testViewer()
	v.frame.Tabs = []TabInfo{{Tab: "t1", URL: "https://example.com/", Active: true}}
	tab := v.items()[3]

	v.mouse(mouseEvent{X: tab.left + 3, Y: 2})
	if !v.editing || string(v.edit) != "https://example.com/" || !v.replace {
		t.Errorf("editing %t %q, selected %t", v.editing, string(v.edit), v.replace)
	}
}

// Every icon is drawn from its SVG without error.
func TestIconsDraw(t *testing.T) {
	for _, name := range append(buttonIcons[:], "stop") {
		png, err := drawIcon(name, 20, 20, color.NRGBA{255, 255, 255, 255})
		if err != nil || len(png) == 0 {
			t.Errorf("%s: %v", name, err)
		}
	}
}

// On a blank tab, typing goes into the start screen's box with the cursor
// after it, and leaving the blank tab ends the editing.
func TestStartScreenTakesTyping(t *testing.T) {
	v := testViewer()
	v.frame = Frame{Tab: "t1", URL: "about:blank"}
	v.page(false)

	v.handle(keyPress{Key: "x"})
	if !v.editing || string(v.edit) != "x" {
		t.Errorf("editing %t, text %q", v.editing, string(v.edit))
	}
	top, left, _ := v.boxRect()
	if out := v.screen(); !strings.Contains(out, "x") || !strings.HasSuffix(out, fmt.Sprintf("\x1b[%d;%dH\x1b[6 q\x1b[?25h", top+1, left+3)) {
		t.Errorf("the caret should be a bar cursor after the x: %q", out)
	}

	v.frame.URL = "https://example.com/"
	v.page(false)
	if v.editing {
		t.Error("still editing after leaving the blank tab")
	}
}
