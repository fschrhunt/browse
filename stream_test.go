package main

import (
	"bytes"
	"context"
	"image"
	"image/png"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

// testView is a 50x23 cell terminal with 8x15 cells: a 400x345 screen with
// a 400x300 page area under the bar.
var testView = View{Cols: 50, Rows: 23, CellW: 8, CellH: 15, Scale: 1, FG: "#e6e6e6", BG: "#000000"}

// nextFrame waits for a frame with a picture of layer from the stream.
func nextFrame(t *testing.T, frames <-chan Frame, layer string) Frame {
	t.Helper()
	return pictures(t, frames, layer)[layer]
}

// pictures waits for a picture of each of the layers, keeping the newest.
func pictures(t *testing.T, frames <-chan Frame, layers ...string) map[string]Frame {
	t.Helper()
	got := map[string]Frame{}
	timeout := time.After(10 * time.Second)
	for len(got) < len(layers) {
		select {
		case f := <-frames:
			for _, l := range layers {
				if f.Layer == l {
					got[l] = f
				}
			}
		case <-timeout:
			t.Fatalf("no picture of every layer %v within 10s, got %d", layers, len(got))
		}
	}
	return got
}

// startStream streams frames of the session into a channel until the test
// ends, and returns a channel to send the viewer's inputs on.
func startStream(t *testing.T, s *Session, view View) (<-chan Frame, chan<- Input) {
	ctx, cancel := context.WithCancel(context.Background())
	frames := make(chan Frame, 64)
	inputs := make(chan Input, 16)
	done := make(chan struct{})
	go func() {
		defer close(done)
		s.Stream(ctx, view, func(f Frame) error {
			select {
			case frames <- f:
			default: // a slow test does not stall the stream
			}
			return nil
		}, inputs)
	}()
	t.Cleanup(func() { cancel(); <-done })
	return frames, inputs
}

// siteURL is the test server's address, read from the first tab's page.
func siteURL(t *testing.T, s *Session, tab string) string {
	t.Helper()
	info, err := s.info(s.tabs[tab])
	if err != nil {
		t.Fatal(err)
	}
	return info.URL
}

// A viewer gets pictures of the page, the bar, and the box at once, each
// sized to its area, and the stream follows the active tab.
func TestStreamSendsAllLayers(t *testing.T) {
	s, first := newTestSession(t, map[string]string{
		"/":      `<title>One</title>`,
		"/other": `<body style="background:#0f0">`,
	})
	frames, _ := startStream(t, s, testView)

	got := pictures(t, frames, "page", "bar", "box")
	if f := got["page"]; f.Tab != first {
		t.Errorf("first frame is of tab %q, want %q", f.Tab, first)
	}
	if w, h := pngSize(got["page"].PNG); w != 400 || h != 300 {
		t.Errorf("page picture is %dx%d, want 400x300", w, h)
	}
	if w, h := pngSize(got["bar"].PNG); w != 400 || h != 45 {
		t.Errorf("bar picture is %dx%d, want 400x45", w, h)
	}
	box := got["box"]
	if w, h := pngSize(box.PNG); w != 240 || h != 45 {
		t.Errorf("box picture is %dx%d, want 240x45", w, h)
	}
	// The box's corners are rounded, so the page shows past them.
	if _, _, _, a := decodePNG(t, box.PNG).At(0, 0).RGBA(); a != 0 {
		t.Errorf("box corner has alpha %d, want 0", a)
	}

	info, _ := s.Open(siteURL(t, s, first) + "other")
	timeout := time.After(10 * time.Second)
	for {
		select {
		case f := <-frames:
			if f.Layer == "page" && f.Tab == info.Tab {
				if r, g := centerColor(f.PNG); !(g > 0xc000 && r < 0x4000) {
					t.Errorf("the new tab's picture is not its green page")
				}
				return
			}
		case <-timeout:
			t.Fatal("no picture of the new tab")
		}
	}
}

// Typing an address in the box and pressing Enter goes there, through the
// interface's own key handling.
func TestAddressBoxGoes(t *testing.T) {
	s, tab := newTestSession(t, map[string]string{"/": `<title>One</title>`, "/two": `<title>Two</title>`})
	frames, inputs := startStream(t, s, testView)
	nextFrame(t, frames, "bar")

	inputs <- Input{Kind: "edit"}
	timeout := time.After(5 * time.Second)
	for f := (Frame{}); !f.Editing; {
		select {
		case f = <-frames:
		case <-timeout:
			t.Fatal("the box never took the keyboard")
		}
	}
	inputs <- Input{Kind: "text", To: "box", Text: siteURL(t, s, tab) + "two"}
	inputs <- Input{Kind: "key", To: "box", Key: "Enter"}
	for deadline := time.Now().Add(10 * time.Second); time.Now().Before(deadline); time.Sleep(100 * time.Millisecond) {
		if info, _ := s.info(s.tabs[tab]); info.Title == "Two" {
			return
		}
	}
	t.Fatal("the tab did not go to the typed address")
}

// The box sits at eye level on whole cells, three fifths of the width, and
// its pixel place follows from the cells.
func TestBoxGeometry(t *testing.T) {
	col, row, cols, rows := testView.box()
	if col != 11 || row != 9 || cols != 30 || rows != 3 {
		t.Errorf("box at column %d row %d, %dx%d cells", col, row, cols, rows)
	}
	if x, y, w, h := testView.boxPx(); x != 80 || y != 120 || w != 240 || h != 45 {
		t.Errorf("box at %v,%v %vx%v px", x, y, w, h)
	}
}

// A blank tab shows the start screen: the box takes the keyboard on its own.
func TestStartScreenTakesKeyboard(t *testing.T) {
	s, tab := newTestSession(t, map[string]string{"/": ``})
	s.Goto(tab, "about:blank")
	frames, _ := startStream(t, s, testView)

	timeout := time.After(5 * time.Second)
	for f := (Frame{}); !f.Start || !f.Editing; {
		select {
		case f = <-frames:
		case <-timeout:
			t.Fatal("the start screen never gave the box the keyboard")
		}
	}
}

// The interface's state knows where the tab's history goes and when it loads.
func TestInterfaceStateKnowsHistoryAndLoading(t *testing.T) {
	s, id := newTestSession(t, map[string]string{"/": `<title>One</title>`, "/two": `<title>Two</title>`})
	tab := s.tabs[id]

	if st, start := s.uiState(tab); st.Back || st.Forward || start {
		t.Errorf("fresh tab: %+v start %t", st, start)
	}
	s.Goto(id, siteURL(t, s, id)+"two")
	s.Back(id)
	if st, _ := s.uiState(tab); st.Back || !st.Forward {
		t.Errorf("after going back to the first page: back %t, forward %t", st.Back, st.Forward)
	}
	if st, _ := s.uiState(tab); len(st.Tabs) != 1 || st.Tabs[0].Label != "127.0.0.1" || !st.Tabs[0].Active {
		t.Errorf("tabs %+v", st.Tabs)
	}

	// 10.255.255.1 is unroutable, so the load hangs until stopped.
	go s.Goto(id, "http://10.255.255.1/")
	for deadline := time.Now().Add(5 * time.Second); ; time.Sleep(50 * time.Millisecond) {
		if st, _ := s.uiState(tab); st.Loading {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("the state never said the page was loading")
		}
	}
	s.Stop(id)
}

// A person's click and typing reach the page.
func TestInputReachesPage(t *testing.T) {
	s, tab := newTestSession(t, map[string]string{"/": `
		<input id="q" style="position:fixed; left:0; top:0; width:200px; height:40px"
		       oninput="document.title = this.value">`})

	for _, in := range []Input{
		{Kind: "mouse", Type: "mousePressed", X: 50, Y: 20, Button: "left", Clicks: 1},
		{Kind: "mouse", Type: "mouseReleased", X: 50, Y: 20, Button: "left", Clicks: 1},
		{Kind: "key", Key: "h"},
		{Kind: "key", Key: "i"},
	} {
		if err := s.SendInput(in); err != nil {
			t.Fatal(err)
		}
	}
	if title := evalString(t, s, tab, "document.title"); title != "hi" {
		t.Errorf("title %q", title)
	}
}

func TestWheelScrollsPage(t *testing.T) {
	s, tab := newTestSession(t, map[string]string{"/": `<div style="height:5000px">long</div>`})

	if err := s.SendInput(Input{Kind: "mouse", Type: "mouseWheel", X: 100, Y: 100, DeltaY: 300}); err != nil {
		t.Fatal(err)
	}
	time.Sleep(300 * time.Millisecond) // the scroll lands after the event
	if y := evalString(t, s, tab, "String(scrollY)"); y == "0" {
		t.Error("the page did not scroll")
	}
}

// When the page changes after the viewer opened, a new picture follows.
func TestStreamSendsChanges(t *testing.T) {
	s, tab := newTestSession(t, map[string]string{"/": `<body style="background:#fff">`})
	frames, _ := startStream(t, s, testView)
	nextFrame(t, frames, "page")

	s.Eval(tab, `document.body.style.background = '#f00'`)
	timeout := time.After(5 * time.Second)
	for {
		select {
		case f := <-frames:
			if f.Layer == "page" {
				if r, g := centerColor(f.PNG); r > 0xc000 && g < 0x4000 {
					return
				}
			}
		case <-timeout:
			t.Fatal("no picture of the changed page")
		}
	}
}

// A page that fails to load still gets a picture: Chrome's error page.
func TestStreamShowsErrorPage(t *testing.T) {
	s, tab := newTestSession(t, map[string]string{"/": ``})
	s.Goto(tab, "about:blank")
	frames, _ := startStream(t, s, testView)
	nextFrame(t, frames, "page")

	refused := httptest.NewServer(http.NotFoundHandler())
	refused.Close() // nothing listens there now
	go s.Goto(tab, refused.URL)
	timeout := time.After(10 * time.Second)
	for {
		select {
		case f := <-frames:
			if f.Layer == "page" && !f.Start {
				if info, _ := s.info(s.tabs[tab]); strings.HasPrefix(info.URL, refused.URL) {
					return
				}
			}
		case <-timeout:
			t.Fatal("no picture of the failed page")
		}
	}
}

// pngSize reads the size from a PNG's header.
func pngSize(b []byte) (w, h int) {
	if len(b) < 24 {
		return 0, 0
	}
	return int(b[16])<<24 | int(b[17])<<16 | int(b[18])<<8 | int(b[19]),
		int(b[20])<<24 | int(b[21])<<16 | int(b[22])<<8 | int(b[23])
}

func decodePNG(t *testing.T, b []byte) image.Image {
	t.Helper()
	img, err := png.Decode(bytes.NewReader(b))
	if err != nil {
		t.Fatal(err)
	}
	return img
}

// centerColor is the red and green of a PNG's middle pixel.
func centerColor(b []byte) (r, g uint32) {
	img, err := png.Decode(bytes.NewReader(b))
	if err != nil {
		return 0, 0
	}
	r, g, _, _ = img.At(img.Bounds().Dx()/2, img.Bounds().Dy()/2).RGBA()
	return r, g
}

// Leaving a tab, such as by closing the blank one the start screen was on,
// takes the keyboard from the box.
func TestBoxClosesWhenTabChanges(t *testing.T) {
	s, first := newTestSession(t, map[string]string{"/": `<title>One</title>`})
	frames, _ := startStream(t, s, testView)
	nextFrame(t, frames, "bar")

	blank, _ := s.Open("about:blank")
	timeout := time.After(5 * time.Second)
	var f Frame
	for f.Tab != blank.Tab || !f.Editing {
		select {
		case f = <-frames:
		case <-timeout:
			t.Fatal("the start screen never had the keyboard")
		}
	}
	s.Close(blank.Tab)
	timeout = time.After(5 * time.Second)
	for f.Tab != first || f.Editing {
		select {
		case f = <-frames:
		case <-timeout:
			t.Fatalf("back on the first tab the box still has the keyboard: %+v", f)
		}
	}
}

// Across a network, a small change to the page goes as just its region.
func TestRemoteStreamSendsPartials(t *testing.T) {
	s, _ := newTestSession(t, map[string]string{"/": `<body style="margin:0;background:#fff">
		<input autofocus style="position:absolute;left:100px;top:50px;width:120px;height:30px;font:20px monospace">`})
	remote := testView
	remote.Remote, remote.Parts = true, true
	frames, inputs := startStream(t, s, remote)
	first := nextFrame(t, frames, "page")
	if first.Partial {
		t.Fatal("the first picture should be whole")
	}

	inputs <- Input{Kind: "key", Key: "a"}
	timeout := time.After(5 * time.Second)
	for {
		select {
		case f := <-frames:
			if f.Layer != "page" || !f.Partial {
				continue
			}
			w, h := pngSize(f.PNG)
			if f.X < 90 || f.X > 240 || f.Y < 40 || f.Y > 90 || w > 140 || h > 50 {
				t.Errorf("partial picture at %d,%d is %dx%d, want around the input", f.X, f.Y, w, h)
			}
			return
		case <-timeout:
			t.Fatal("no partial picture after typing into the page")
		}
	}
}

// A copy asks the page for its selection, which the viewer puts on the
// clipboard.
func TestCopySendsSelection(t *testing.T) {
	s, tab := newTestSession(t, map[string]string{"/": `<p id="p">copy me</p>`})
	s.Eval(tab, `getSelection().selectAllChildren(document.getElementById('p'))`)
	frames, inputs := startStream(t, s, testView)
	nextFrame(t, frames, "page")

	inputs <- Input{Kind: "copy", To: "page"}
	timeout := time.After(5 * time.Second)
	for {
		select {
		case f := <-frames:
			if f.Copy == "copy me" {
				return
			}
		case <-timeout:
			t.Fatal("no frame carried the selection")
		}
	}
}
