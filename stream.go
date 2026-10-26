// What a viewer needs from the session: a stream of frames of the active tab,
// and a way to send a person's mouse and keyboard input to it.
package main

import (
	"context"
	"errors"
	"slices"
	"strings"
	"time"

	"github.com/chromedp/cdproto/cdp"
	"github.com/chromedp/cdproto/emulation"
	"github.com/chromedp/cdproto/input"
	"github.com/chromedp/cdproto/page"
	"github.com/chromedp/chromedp"
)

const (
	frameGap    = 66 * time.Millisecond // at most about 15 frames a second
	barInterval = 250 * time.Millisecond
	agentShown  = 3 * time.Second // how long the bar shows that an agent acted
)

// Frame is one update for a viewer: the bar's contents, and a picture of the
// active tab when it changed. A frame without a Tab means every tab is
// closed. Back and Forward say whether the tab's history goes that way.
// Loading is set while the page loads, and Agent while an agent is acting on
// the tab. Tabs lists every open tab, in order, for the bar.
type Frame struct {
	Tab     string    `json:"tab"`
	URL     string    `json:"url"`
	Title   string    `json:"title"`
	Back    bool      `json:"back"`
	Forward bool      `json:"forward"`
	Loading bool      `json:"loading"`
	Agent   bool      `json:"agent"`
	Tabs    []TabInfo `json:"tabs,omitempty"`
	PNG     []byte    `json:"png,omitempty"`
}

// View is the page area a viewer draws into: its size in CSS pixels and the
// device pixels per CSS pixel. Dark says the terminal has a dark background,
// so pages that offer a dark look use it, as they would with a dark system
// theme.
type View struct {
	Width  int     `json:"width"`
	Height int     `json:"height"`
	Scale  float64 `json:"scale"`
	Dark   bool    `json:"dark,omitempty"`
}

// markAgent records that an agent is acting on a tab, for the viewer's bar.
func (s *Session) markAgent(id string) {
	t, err := s.use(id, false)
	if err != nil {
		return
	}
	t.state.Lock()
	t.agentAt = time.Now()
	t.state.Unlock()
}

// activeTab returns the active tab, or nil when no tab is open.
func (s *Session) activeTab() *tab {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.tabs[s.active]
}

// Cycle makes the next tab active, or the previous one when step is negative.
func (s *Session) Cycle(step int) (TabInfo, error) {
	s.mu.Lock()
	if len(s.order) == 0 {
		s.mu.Unlock()
		return TabInfo{}, errors.New("no tabs open")
	}
	i := 0
	for j, id := range s.order {
		if id == s.active {
			i = j
		}
	}
	n := len(s.order)
	s.active = s.order[((i+step)%n+n)%n]
	t := s.tabs[s.active]
	s.mu.Unlock()
	return s.info(t)
}

// Stream sends frames of the active tab until ctx ends, following the active
// tab as it changes. While a tab is shown, its page is sized to view. Frames
// come from Chrome's screencast, which only produces one when the page
// changes, so an idle page costs nothing.
func (s *Session) Stream(ctx context.Context, view View, send func(Frame) error) error {
	var (
		shown   *tab
		stop    = func() {}
		pending = make(chan []byte, 1)
		last    Frame
		lastPNG time.Time
	)
	defer func() { stop() }()

	bar := time.NewTicker(barInterval)
	defer bar.Stop()
	for {
		t := s.activeTab()
		if t == nil {
			return send(Frame{})
		}
		if t != shown {
			stop()
			shown = t
			stop = screencast(t, view, pending)
		}

		f := Frame{Tab: t.id}
		if info, err := s.info(t); err == nil {
			f.URL, f.Title = info.URL, info.Title
		}
		f.Back, f.Forward = history(t)
		f.Loading = t.loading()
		f.Tabs, _ = s.List()
		t.state.Lock()
		f.Agent = time.Since(t.agentAt) < agentShown
		t.state.Unlock()

		select {
		case <-ctx.Done():
			return nil
		case png := <-pending:
			if wait := frameGap - time.Since(lastPNG); wait > 0 {
				time.Sleep(wait)
				// A newer picture may have arrived while waiting.
				select {
				case png = <-pending:
				default:
				}
			}
			f.PNG = png
			lastPNG = time.Now()
		case <-bar.C:
			if sameBar(f, last) {
				continue
			}
		}
		if err := send(f); err != nil {
			return err
		}
		f.PNG = nil
		last = f
	}
}

// sameBar reports whether two frames show the same bar.
func sameBar(a, b Frame) bool {
	return a.Tab == b.Tab && a.URL == b.URL && a.Title == b.Title &&
		a.Back == b.Back && a.Forward == b.Forward && a.Loading == b.Loading && a.Agent == b.Agent &&
		slices.EqualFunc(a.Tabs, b.Tabs, func(x, y TabInfo) bool {
			return x.Tab == y.Tab && x.Active == y.Active && x.URL == y.URL && x.Title == y.Title
		})
}

// history reports whether t can go back and forward. The blank page a tab
// starts on does not count as somewhere to go back to. A tab too busy to
// answer within a second goes neither way.
func history(t *tab) (back, forward bool) {
	ctx, cancel := context.WithTimeout(t.ctx, time.Second)
	defer cancel()
	h, err := chromedp.Call(ctx, page.GetNavigationHistory, cdp.Empty{})
	if err != nil {
		return false, false
	}
	for _, e := range h.Entries[:h.CurrentIndex] {
		back = back || e.URL != "about:blank"
	}
	return back, h.CurrentIndex < int64(len(h.Entries))-1
}

// screencast sizes t's page to view and sends its pictures to out, keeping
// only the newest. The returned func stops it and restores the default size.
// Chrome sends the pictures as events of the tab, so they are listened for
// on the tab's context.
func screencast(t *tab, view View, out chan []byte) func() {
	ctx, cancel := context.WithCancel(t.ctx)
	put := func(png []byte) {
		select {
		case <-out:
		default:
		}
		out <- png
	}

	chromedp.Run(t.ctx, chromedp.EmulateViewport(int64(view.Width), int64(view.Height), chromedp.EmulateScale(view.Scale)))
	scheme := "light"
	if view.Dark {
		scheme = "dark"
	}
	chromedp.Call(t.ctx, emulation.SetEmulatedMedia, emulation.SetEmulatedMediaParams{
		Features: []*emulation.MediaFeature{{Name: "prefers-color-scheme", Value: scheme}},
	})
	frames := chromedp.Events(ctx, page.ScreencastFrame)
	chromedp.Call(t.ctx, page.StartScreencast, page.StartScreencastParams{
		Format:    page.StartScreencastFormatPng,
		MaxWidth:  int64(float64(view.Width) * view.Scale),
		MaxHeight: int64(float64(view.Height) * view.Scale),
	})
	// The screencast only sends a picture once something changes, so start
	// with a screenshot of the page as it is.
	if png, err := chromedp.Run(t.ctx, chromedp.CaptureScreenshot()); err == nil {
		put(png)
	}
	go func() {
		for ev, err := range frames {
			if err != nil {
				return
			}
			chromedp.Call(t.ctx, page.ScreencastFrameAck, page.ScreencastFrameAckParams{SessionID: ev.SessionID})
			put(ev.Data)
		}
	}()

	return func() {
		cancel()
		chromedp.Call(t.ctx, page.StopScreencast, cdp.Empty{})
		chromedp.Run(t.ctx, chromedp.EmulateViewport(viewportWidth, viewportHeight))
		chromedp.Call(t.ctx, emulation.SetEmulatedMedia, emulation.SetEmulatedMediaParams{})
	}
}

// Input is one piece of a person's input for the active tab. Kind is "mouse",
// "key", or "text". Mouse coordinates are CSS pixels in the page. Key is a
// single character or a key name such as Enter, with Modifiers like Control.
type Input struct {
	Kind      string   `json:"kind"`
	Type      string   `json:"type,omitempty"` // mousePressed, mouseReleased, mouseMoved, mouseWheel
	X         float64  `json:"x,omitempty"`
	Y         float64  `json:"y,omitempty"`
	Button    string   `json:"button,omitempty"` // left, middle, right
	Clicks    int      `json:"clicks,omitempty"`
	DeltaX    float64  `json:"deltaX,omitempty"`
	DeltaY    float64  `json:"deltaY,omitempty"`
	Key       string   `json:"key,omitempty"`
	Modifiers []string `json:"modifiers,omitempty"`
	Text      string   `json:"text,omitempty"`
}

// wheelParams is Input.dispatchMouseEvent for a wheel turn. cdproto's own
// params drop a zero delta, and Chrome rejects a wheel event without both.
type wheelParams struct {
	Type      string  `json:"type"`
	X         float64 `json:"x"`
	Y         float64 `json:"y"`
	DeltaX    float64 `json:"deltaX"`
	DeltaY    float64 `json:"deltaY"`
	Modifiers int64   `json:"modifiers"`
}

var dispatchWheel = cdp.Command[wheelParams, cdp.Empty]{Method: input.CommandDispatchMouseEvent}

// wheel turns the mouse wheel at x, y by the given CSS pixels.
func wheel(ctx context.Context, x, y, dx, dy float64, modifiers int64) error {
	_, err := chromedp.Call(ctx, dispatchWheel, wheelParams{
		Type: "mouseWheel", X: x, Y: y, DeltaX: dx, DeltaY: dy, Modifiers: modifiers,
	})
	return err
}

// mouseButtons is the buttons bit field Chrome wants while a button is held.
var mouseButtons = map[string]int64{"left": 1, "right": 2, "middle": 4}

// SendInput delivers a person's input to the active tab at once, without the
// tab's lock or settling, so the page responds as fast as it can.
func (s *Session) SendInput(in Input) error {
	t := s.activeTab()
	if t == nil {
		return errors.New("no tabs open")
	}
	if d := t.pendingDialog(); d != nil {
		return dialogError(d)
	}
	ctx, cancel := context.WithTimeout(t.ctx, commandTimeout)
	defer cancel()

	var mods []chromedp.Modifier
	var bits int64
	for _, name := range in.Modifiers {
		mod, ok := modifierKeys[strings.ToLower(name)]
		if !ok {
			return errors.New("unknown modifier " + name)
		}
		mods = append(mods, mod)
		bits |= int64(mod)
	}

	switch in.Kind {
	case "mouse":
		if in.Type == "mouseWheel" {
			return wheel(ctx, in.X, in.Y, in.DeltaX, in.DeltaY, bits)
		}
		p := input.DispatchMouseEventParams{
			Type: input.DispatchMouseEventType(in.Type), X: in.X, Y: in.Y,
			Modifiers: bits, DeltaX: in.DeltaX, DeltaY: in.DeltaY,
		}
		if in.Button != "" {
			p.Button = input.MouseButton(in.Button)
			p.ClickCount = int64(in.Clicks)
			if in.Type != "mouseReleased" {
				p.Buttons = mouseButtons[in.Button]
			}
		}
		_, err := chromedp.Call(ctx, input.DispatchMouseEvent, p)
		return err
	case "key":
		key, err := keyFor(in.Key)
		if err != nil {
			return err
		}
		return chromedp.Do(ctx, chromedp.KeyEvent(key, chromedp.KeyModifiers(mods...)))
	case "text":
		_, err := chromedp.Call(ctx, input.InsertText, input.InsertTextParams{Text: in.Text})
		return err
	}
	return errors.New("unknown input kind " + in.Kind)
}
