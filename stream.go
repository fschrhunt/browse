// What a viewer needs from the session: frames of the active tab, of the bar,
// and of the address box, and a way to send the person's mouse and keyboard
// to any of them.
package main

import (
	"context"
	"errors"
	"strconv"
	"strings"
	"sync/atomic"
	"time"

	"github.com/chromedp/cdproto/cdp"
	"github.com/chromedp/cdproto/emulation"
	"github.com/chromedp/cdproto/input"
	"github.com/chromedp/cdproto/page"
	"github.com/chromedp/chromedp"
)

const (
	barRows    = 3                      // the bar's height, in terminal rows
	boxRows    = 3                      // the address box's height, in terminal rows
	pageGap    = 66 * time.Millisecond  // at most about 15 page frames a second
	remoteGap  = 125 * time.Millisecond // 8 a second when frames cross a network
	uiGap      = 40 * time.Millisecond  // the interface is small, so it may be quicker
	stateEvery = 250 * time.Millisecond // how often the tabs and history are looked at
	agentShown = 3 * time.Second        // how long the bar shows that an agent acted
	noteShown  = 4 * time.Second        // how long an error shows under the bar
)

// View is the terminal a viewer draws into: its size in cells, a cell's size
// in device pixels, the device pixels per CSS pixel to render at, its colors,
// whether frames cross a network, which asks for fewer of them, and whether
// the viewer can place partial page pictures, which tmux cannot.
type View struct {
	Cols   int     `json:"cols"`
	Rows   int     `json:"rows"`
	CellW  float64 `json:"cellW"`
	CellH  float64 `json:"cellH"`
	Scale  float64 `json:"scale"`
	FG     string  `json:"fg"`
	BG     string  `json:"bg"`
	Remote bool    `json:"remote,omitempty"`
	Parts  bool    `json:"parts,omitempty"`
}

// screen is the whole terminal in CSS pixels, which the interface fills.
func (v View) screen() (w, h int) {
	return int(float64(v.Cols) * v.CellW / v.Scale), int(float64(v.Rows) * v.CellH / v.Scale)
}

// page is the area under the bar in CSS pixels, which the page fills.
func (v View) page() (w, h int) {
	return int(float64(v.Cols) * v.CellW / v.Scale), int(float64(v.Rows-barRows) * v.CellH / v.Scale)
}

// barHeight is the bar's height in CSS pixels: barRows cells exactly, so the
// page sits right under it.
func (v View) barHeight() float64 {
	return float64(barRows) * v.CellH / v.Scale
}

// box is where the address box goes, in whole cells from column and row 1:
// boxRows tall, three fifths of the columns wide, centered across and three
// tenths of the way down the page area, at eye level.
func (v View) box() (col, row, cols, rows int) {
	cols = min(max(v.Cols*3/5, 20), v.Cols-2)
	return (v.Cols-cols)/2 + 1, barRows + 1 + (v.Rows-barRows-boxRows)*3/10, cols, boxRows
}

// boxPx is the box's place on the screen in CSS pixels.
func (v View) boxPx() (x, y, w, h float64) {
	col, row, cols, rows := v.box()
	return float64(col-1) * v.CellW / v.Scale, float64(row-1) * v.CellH / v.Scale,
		float64(cols) * v.CellW / v.Scale, float64(rows) * v.CellH / v.Scale
}

// dark reports whether the terminal's background is dark, so pages that
// offer a dark look use it. A background that is not #rrggbb counts as dark.
func (v View) dark() bool {
	var rgb [3]int
	if len(v.BG) != 7 || v.BG[0] != '#' {
		return true
	}
	for i := range rgb {
		n, err := strconv.ParseUint(v.BG[1+2*i:3+2*i], 16, 8)
		if err != nil {
			return true
		}
		rgb[i] = int(n)
	}
	return 0.2126*float64(rgb[0])+0.7152*float64(rgb[1])+0.0722*float64(rgb[2]) < 128
}

// Frame is one update for a viewer. A frame with a PNG is a picture of one
// Layer: "page", "bar", or "box". A page picture with Partial set is only
// the part of the page that changed, to be placed X, Y device pixels from
// the page's top left over what is there. Every frame carries the state the
// viewer routes by: the active tab (none means every tab is closed), whether
// the box holds the keyboard, and whether the tab is blank, which shows the
// start screen: the box over nothing.
type Frame struct {
	Layer   string `json:"layer,omitempty"`
	PNG     []byte `json:"png,omitempty"`
	Partial bool   `json:"partial,omitempty"`
	X       int    `json:"x,omitempty"`
	Y       int    `json:"y,omitempty"`
	Tab     string `json:"tab"`
	Editing bool   `json:"editing"`
	Start   bool   `json:"start"`
	Copy    string `json:"copy,omitempty"` // text the person copied, for the clipboard
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

// Stream sends frames to a viewer until ctx ends: pictures of the active tab,
// following it as it changes, pictures of the bar and the box, and the state
// the viewer routes input by. Inputs from the viewer arrive on in. Pictures
// come from Chrome's screencast, which only produces one when something
// changes, so an idle page costs nothing.
func (s *Session) Stream(ctx context.Context, view View, send func(Frame) error, in <-chan Input) error {
	var (
		shown    *tab
		stopPage = func() {}
		pngs     = map[string]chan []byte{"page": make(chan []byte, 1), "bar": make(chan []byte, 1), "box": make(chan []byte, 1)}
		sent     = map[string]time.Time{}
		editing  atomic.Bool
		changed  = make(chan struct{}, 1)
		note     string
		noteAt   time.Time
		box      *iface
		pageDiff *differ // across a network, only what changed goes
	)
	if view.Remote && view.Parts {
		pageDiff = &differ{}
	}
	defer func() { stopPage() }()

	// The interface asks the session for things on the person's behalf.
	act := func(m uiMessage) {
		switch m.T {
		case "editing":
			editing.Store(m.On)
			select {
			case changed <- struct{}{}:
			default:
			}
			return
		case "edit":
			text := ""
			if t := s.activeTab(); t != nil {
				if info, err := s.info(t); err == nil && info.URL != "about:blank" {
					text = info.URL
				}
			}
			go box.edit(text)
			return
		}
		go func() {
			var err error
			switch m.T {
			case "go":
				_, err = s.Goto("", m.URL)
			case "new":
				_, err = s.Open("about:blank")
			case "switch":
				_, err = s.Activate(m.Tab)
			case "close":
				_, err = s.Close(m.Tab)
			case "back":
				_, err = s.Back("")
			case "forward":
				_, err = s.Forward("")
			case "reload":
				_, err = s.Reload("")
			case "stop":
				_, err = s.Stop("")
			}
			if err != nil {
				note, noteAt = err.Error(), time.Now()
			}
		}()
	}
	screenW, _ := view.screen()
	bar, err := s.openInterface(view, "bar", screenW, int(view.barHeight()), act)
	if err != nil {
		return err
	}
	defer bar.close()
	_, _, boxW, boxH := view.boxPx()
	box, err = s.openInterface(view, "box", int(boxW), int(boxH), act)
	if err != nil {
		return err
	}
	defer box.close()
	defer cast(bar.ctx, screenW, int(view.barHeight()), view.Scale, pngs["bar"])()
	defer cast(box.ctx, int(boxW), int(boxH), view.Scale, pngs["box"])()

	gap := pageGap
	if view.Remote {
		gap = remoteGap
	}
	tick := time.NewTicker(stateEvery)
	defer tick.Stop()
	for {
		t := s.activeTab()
		if t == nil {
			return send(Frame{})
		}
		if t != shown {
			stopPage()
			if shown != nil && editing.Load() {
				box.blur() // the box was for the tab just left
			}
			shown = t
			stopPage = t.show(view, pngs["page"])
			if pageDiff != nil {
				pageDiff.last = nil
			}
		}

		st, start := s.uiState(t)
		if time.Since(noteAt) < noteShown {
			st.Note = note
		}
		bar.update(st)
		box.update(st)
		if start && !editing.Load() {
			box.edit("") // the start screen is the box, so it has the keyboard
		}

		f := Frame{Tab: t.id, Start: start, Editing: editing.Load()}
		select {
		case <-ctx.Done():
			return nil
		case png := <-pngs["page"]:
			f.Layer, f.PNG = "page", pace(pngs["page"], png, sent, "page", gap)
			if pageDiff != nil {
				var full bool
				f.PNG, f.X, f.Y, full = pageDiff.next(f.PNG)
				f.Partial = !full
				if f.PNG == nil {
					continue
				}
			}
		case png := <-pngs["bar"]:
			f.Layer, f.PNG = "bar", pace(pngs["bar"], png, sent, "bar", uiGap)
		case png := <-pngs["box"]:
			f.Layer, f.PNG = "box", pace(pngs["box"], png, sent, "box", uiGap)
		case <-changed:
			f.Editing = editing.Load()
		case input, ok := <-in:
			if !ok {
				return nil
			}
			if copied := s.route(input, t, bar, box); copied != "" {
				f.Copy = copied
				break
			}
			continue
		case <-tick.C:
			continue
		}
		if err := send(f); err != nil {
			return err
		}
	}
}

// pace holds a picture of layer back until gap has passed since the last
// one sent, and takes a newer picture that arrived meanwhile instead.
func pace(pending chan []byte, png []byte, sent map[string]time.Time, layer string, gap time.Duration) []byte {
	if wait := gap - time.Since(sent[layer]); wait > 0 {
		time.Sleep(wait)
		select {
		case png = <-pending:
		default:
		}
	}
	sent[layer] = time.Now()
	return png
}

// uiState is what the interface shows for the tab t, which is active: the
// tabs, where its history goes, and whether it is loading or an agent is on
// it. start says the tab is blank, which shows the start screen.
func (s *Session) uiState(t *tab) (st uiState, start bool) {
	st = uiState{Loading: t.loading() || t.acting()}
	st.Back, st.Forward = history(t)
	t.state.Lock()
	st.Agent = time.Since(t.agentAt) < agentShown
	t.state.Unlock()
	tabs, _ := s.List()
	for _, info := range tabs {
		st.Tabs = append(st.Tabs, uiTab{ID: info.Tab, Label: tabLabel(info), URL: info.URL, Active: info.Active})
		if info.Active {
			start = info.URL == "about:blank"
		}
	}
	return st, start
}

// selectionJS is the text selected in a page, in a field or on the page.
const selectionJS = `(() => {
	const a = document.activeElement;
	if (a && (a.tagName === 'INPUT' || a.tagName === 'TEXTAREA') && a.selectionStart !== a.selectionEnd)
		return a.value.slice(a.selectionStart, a.selectionEnd);
	return String(getSelection());
})()`

// route delivers a viewer's input to the bar, the box, or the tab shown. A
// copy returns the text selected there, for the viewer to put on the clipboard.
func (s *Session) route(in Input, t *tab, bar, box *iface) (copied string) {
	switch {
	case in.Kind == "copy":
		ctx := t.ctx
		if in.To == "box" {
			ctx = box.ctx
		}
		copied, _ = chromedp.Run(ctx, chromedp.Evaluate[string](selectionJS))
	case in.Kind == "edit":
		text := ""
		if info, err := s.info(t); err == nil && info.URL != "about:blank" {
			text = info.URL
		}
		box.edit(text)
	case in.Kind == "blur":
		box.blur()
	case in.To == "bar":
		dispatchInput(bar.ctx, in)
	case in.To == "box":
		dispatchInput(box.ctx, in)
	default:
		if t.pendingDialog() == nil {
			dispatchInput(t.ctx, in)
		}
	}
	return copied
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

// show sizes t's page to the view's page area and sends its pictures to out
// until the returned func is called, which restores the default size. Each
// time a document first paints content or finishes loading, a fresh picture
// follows too, since the screencast sends none of some pages, such as
// Chrome's error pages.
func (t *tab) show(view View, out chan []byte) func() {
	w, h := view.page()
	chromedp.Run(t.ctx, chromedp.EmulateViewport(int64(w), int64(h), chromedp.EmulateScale(view.Scale)))
	scheme := "light"
	if view.dark() {
		scheme = "dark"
	}
	chromedp.Call(t.ctx, emulation.SetEmulatedMedia, emulation.SetEmulatedMediaParams{
		Features: []*emulation.MediaFeature{{Name: "prefers-color-scheme", Value: scheme}},
	})
	w, h = view.page()
	stop := cast(t.ctx, w, h, view.Scale, out)
	ctx, cancel := context.WithCancel(t.ctx)
	go func() {
		tick := time.NewTicker(100 * time.Millisecond)
		defer tick.Stop()
		seen := t.painted.Load()
		for {
			select {
			case <-ctx.Done():
				return
			case <-tick.C:
			}
			if now := t.painted.Load(); now != seen {
				seen = now
				if png, err := chromedp.Run(t.ctx, chromedp.CaptureScreenshot()); err == nil {
					put(out, png)
				}
			}
		}
	}()
	return func() {
		cancel()
		stop()
		chromedp.Run(t.ctx, chromedp.EmulateViewport(viewportWidth, viewportHeight))
		chromedp.Call(t.ctx, emulation.SetEmulatedMedia, emulation.SetEmulatedMediaParams{})
	}
}

// cast streams pictures of ctx's tab, w x h CSS pixels at scale, to out,
// keeping only the newest. It starts with a picture of the tab as it is,
// since the screencast only sends one once something changes. The returned
// func stops it.
func cast(ctx context.Context, w, h int, scale float64, out chan []byte) func() {
	ctx, cancel := context.WithCancel(ctx)
	frames := chromedp.Events(ctx, page.ScreencastFrame)
	chromedp.Call(ctx, page.StartScreencast, page.StartScreencastParams{
		Format:    page.StartScreencastFormatPng,
		MaxWidth:  int64(float64(w) * scale),
		MaxHeight: int64(float64(h) * scale),
	})
	if png, err := chromedp.Run(ctx, chromedp.CaptureScreenshot()); err == nil {
		put(out, png)
	}
	go func() {
		for ev, err := range frames {
			if err != nil {
				return
			}
			chromedp.Call(ctx, page.ScreencastFrameAck, page.ScreencastFrameAckParams{SessionID: ev.SessionID})
			put(out, ev.Data)
		}
	}()
	return func() {
		chromedp.Call(ctx, page.StopScreencast, cdp.Empty{})
		cancel()
	}
}

// put replaces whatever picture is waiting in out with png.
func put(out chan []byte, png []byte) {
	select {
	case <-out:
	default:
	}
	out <- png
}

// Input is one piece of a person's input. Kind is "mouse", "key", or "text",
// for the page unless To says "bar" or "box"; "copy" asks for the selection
// there; "edit" puts the tab's address in the box and gives it the keyboard,
// and "blur" takes the keyboard back.
// Mouse coordinates are CSS pixels from the top left of what they are for.
// Key is a single character or a key name such as Enter, with Modifiers like
// Control.
type Input struct {
	Kind      string   `json:"kind"`
	To        string   `json:"to,omitempty"`
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
	return dispatchInput(t.ctx, in)
}

// dispatchInput sends one input event to ctx's tab.
func dispatchInput(tabCtx context.Context, in Input) error {
	ctx, cancel := context.WithTimeout(tabCtx, commandTimeout)
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
