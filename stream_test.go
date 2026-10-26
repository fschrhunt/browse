package main

import (
	"bytes"
	"context"
	"image/png"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

// nextFrame waits for a frame with a picture from the stream.
func nextFrame(t *testing.T, frames <-chan Frame) Frame {
	t.Helper()
	timeout := time.After(10 * time.Second)
	for {
		select {
		case f := <-frames:
			if f.PNG != nil {
				return f
			}
		case <-timeout:
			t.Fatal("no frame within 10s")
		}
	}
}

// startStream streams frames of the session into a channel until the test ends.
func startStream(t *testing.T, s *Session, view View) <-chan Frame {
	ctx, cancel := context.WithCancel(context.Background())
	frames := make(chan Frame, 64)
	done := make(chan struct{})
	go func() {
		defer close(done)
		s.Stream(ctx, view, func(f Frame) error { frames <- f; return nil })
	}()
	t.Cleanup(func() { cancel(); <-done })
	return frames
}

// A viewer gets a picture of the page at once, sized to its view, and the
// stream follows the active tab.
func TestStreamFollowsActiveTab(t *testing.T) {
	s, first := newTestSession(t, map[string]string{
		"/":      `<title>One</title>`,
		"/other": `<title>Two</title>`,
	})
	frames := startStream(t, s, View{Width: 400, Height: 300, Scale: 1})

	f := nextFrame(t, frames)
	if f.Tab != first || f.Title != "One" {
		t.Errorf("first frame: %+v", f)
	}
	if w := pngWidth(f.PNG); w != 400 {
		t.Errorf("frame width %d, want the view's 400", w)
	}

	info, _ := s.Open(f.URL + "other")
	for f = nextFrame(t, frames); f.Tab != info.Tab; f = nextFrame(t, frames) {
	}
	if f.Title != "Two" {
		t.Errorf("frame after opening a tab: %+v", f)
	}
}

// pngWidth reads the width from a PNG's header.
func pngWidth(png []byte) int {
	if len(png) < 24 {
		return 0
	}
	return int(png[16])<<24 | int(png[17])<<16 | int(png[18])<<8 | int(png[19])
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

// Frames say which way the tab's history goes, so the bar can dim back and
// forward. A new tab has nowhere to go back to.
func TestFramesKnowHistory(t *testing.T) {
	s, tab := newTestSession(t, map[string]string{"/": `<title>One</title>`, "/two": `<title>Two</title>`})
	frames := startStream(t, s, View{Width: 400, Height: 300, Scale: 1})

	f := nextFrame(t, frames)
	if f.Back || f.Forward {
		t.Errorf("new tab: back %t, forward %t", f.Back, f.Forward)
	}
	s.Goto(tab, f.URL+"two")
	s.Back(tab)
	timeout := time.After(5 * time.Second)
	for f.Title != "One" || !f.Forward {
		select {
		case f = <-frames:
		case <-timeout:
			t.Fatalf("no frame with forward after going back; last %+v", f)
		}
	}
	if f.Back {
		t.Error("back on the first page")
	}
}

// Frames say when the page is loading, so the bar can offer stop.
func TestFramesKnowLoading(t *testing.T) {
	s, tab := newTestSession(t, map[string]string{"/": `<title>One</title>`})
	frames := startStream(t, s, View{Width: 400, Height: 300, Scale: 1})
	f := nextFrame(t, frames)

	// 10.255.255.1 is unroutable, so the load hangs until stopped.
	go s.Goto(tab, "http://10.255.255.1/")
	timeout := time.After(5 * time.Second)
	for !f.Loading {
		select {
		case f = <-frames:
		case <-timeout:
			t.Fatal("no frame said the page was loading")
		}
	}
	s.Stop(tab)
}

// When the page changes after the viewer opened, a new picture follows.
func TestStreamSendsChanges(t *testing.T) {
	s, tab := newTestSession(t, map[string]string{"/": `<body style="background:#fff">`})
	frames := startStream(t, s, View{Width: 400, Height: 300, Scale: 1})
	nextFrame(t, frames)

	s.Eval(tab, `document.body.style.background = '#f00'`)
	timeout := time.After(5 * time.Second)
	for {
		select {
		case f := <-frames:
			if f.PNG != nil && redCenter(f.PNG) {
				return
			}
		case <-timeout:
			t.Fatal("no picture of the changed page")
		}
	}
}

// redCenter reports whether a PNG is red in the middle.
func redCenter(b []byte) bool {
	img, err := png.Decode(bytes.NewReader(b))
	if err != nil {
		return false
	}
	r, g, _, _ := img.At(img.Bounds().Dx()/2, img.Bounds().Dy()/2).RGBA()
	return r > 0xc000 && g < 0x4000
}

// A page that fails to load still gets a picture: Chrome's error page.
func TestStreamShowsErrorPage(t *testing.T) {
	s, tab := newTestSession(t, map[string]string{"/": ``})
	s.Goto(tab, "about:blank")
	frames := startStream(t, s, View{Width: 400, Height: 300, Scale: 1})
	nextFrame(t, frames)

	refused := httptest.NewServer(http.NotFoundHandler())
	refused.Close() // nothing listens there now
	go s.Goto(tab, refused.URL)
	timeout := time.After(10 * time.Second)
	for {
		select {
		case f := <-frames:
			if f.PNG != nil && strings.HasPrefix(f.URL, refused.URL) {
				return
			}
		case <-timeout:
			t.Fatal("no picture of the failed page")
		}
	}
}
