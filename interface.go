// The interface: the bar and the address box, each drawn by ui/index.html in
// a hidden tab of its own size, which agents never see. Each viewer gets its
// own pair, sized to its terminal. The daemon pushes state in with
// ui.update, and the page reports what the person did through a binding: an
// address to go to, a tab to switch to or close, and whether the box holds
// the keyboard.
package main

import (
	"context"
	_ "embed"
	"encoding/base64"
	"encoding/json"
	"net/url"
	"strings"
	"sync"

	"github.com/chromedp/cdproto/cdp"
	"github.com/chromedp/cdproto/emulation"
	"github.com/chromedp/cdproto/runtime"
	"github.com/chromedp/chromedp"
)

//go:embed ui/index.html
var uiHTML []byte

// uiState is what the interface shows.
type uiState struct {
	Tabs    []uiTab `json:"tabs"`
	Back    bool    `json:"back"`
	Forward bool    `json:"forward"`
	Loading bool    `json:"loading"`
	Agent   bool    `json:"agent"`
	Note    string  `json:"note"`
}

type uiTab struct {
	ID     string `json:"id"`
	Label  string `json:"label"`
	URL    string `json:"url"`
	Active bool   `json:"active"`
}

// tabLabel is what a tab shows: its site, its title when it has no site, or
// New tab when it is blank.
func tabLabel(t TabInfo) string {
	if t.URL == "about:blank" || t.URL == "" {
		return "New tab"
	}
	if u, err := url.Parse(t.URL); err == nil && u.Hostname() != "" {
		return strings.TrimPrefix(u.Hostname(), "www.")
	}
	if t.Title != "" {
		return t.Title
	}
	return t.URL
}

// uiMessage is what the interface sends back: t is go, edit, switch, close,
// new, back, forward, reload, stop, or editing.
type uiMessage struct {
	T   string `json:"t"`
	URL string `json:"url"`
	Tab string `json:"tab"`
	On  bool   `json:"on"`
}

// iface is one part of a viewer's interface: the bar, or the box.
type iface struct {
	ctx    context.Context
	cancel context.CancelFunc
	mu     sync.Mutex
	last   string // the state last pushed, as JSON
}

// cdproto drops a zero alpha from RGBA, so the transparent background is
// asked for by hand.
type rgba struct {
	R int     `json:"r"`
	G int     `json:"g"`
	B int     `json:"b"`
	A float64 `json:"a"`
}

type backgroundParams struct {
	Color rgba `json:"color"`
}

var setBackground = cdp.Command[backgroundParams, cdp.Empty]{Method: emulation.CommandSetDefaultBackgroundColorOverride}

// openInterface opens one part of the interface, "bar" or "box", w x h CSS
// pixels large, for a viewer of view. What the person asks for in it goes to
// act. It is transparent where it draws nothing, so the page shows through.
func (s *Session) openInterface(view View, part string, w, h int, act func(uiMessage)) (*iface, error) {
	ctx, cancel := chromedp.NewContext(s.anchor)
	u := &iface{ctx: ctx, cancel: cancel}
	fail := func(err error) (*iface, error) {
		cancel()
		return nil, err
	}
	if _, err := chromedp.Run(ctx, chromedp.Navigate("data:text/html;base64,"+base64.StdEncoding.EncodeToString(uiHTML))); err != nil {
		return fail(err)
	}
	if _, err := chromedp.Run(ctx, chromedp.EmulateViewport(int64(w), int64(h), chromedp.EmulateScale(view.Scale))); err != nil {
		return fail(err)
	}
	if _, err := chromedp.Call(ctx, setBackground, backgroundParams{Color: rgba{0, 0, 0, 0}}); err != nil {
		return fail(err)
	}
	if _, err := chromedp.Call(ctx, runtime.AddBinding, runtime.AddBindingParams{Name: "browse"}); err != nil {
		return fail(err)
	}
	go func() {
		for ev, err := range chromedp.Events(ctx, runtime.BindingCalled) {
			if err != nil {
				return
			}
			var m uiMessage
			if json.Unmarshal([]byte(ev.Payload), &m) == nil {
				act(m)
			}
		}
	}()
	setup, _ := json.Marshal(map[string]any{
		"part":  part,
		"theme": map[string]string{"fg": view.FG, "bg": view.BG},
	})
	if _, err := chromedp.Run(ctx, chromedp.Evaluate[any]("ui.update("+string(setup)+")")); err != nil {
		return fail(err)
	}
	// A new tab's mouse sits at its top left, which would count as hovering
	// the bar; the bar's last pixel row is kept clear for the mouse to rest on.
	dispatchInput(ctx, Input{Kind: "mouse", Type: "mouseMoved", X: 0, Y: float64(h) - 0.5})
	return u, nil
}

// update pushes the state to the interface when it changed.
func (u *iface) update(st uiState) {
	js, _ := json.Marshal(st)
	u.mu.Lock()
	same := string(js) == u.last
	u.last = string(js)
	u.mu.Unlock()
	if !same {
		chromedp.Run(u.ctx, chromedp.Evaluate[any]("ui.update("+string(js)+")"))
	}
}

// edit puts text in the box, selected, and gives it the keyboard.
func (u *iface) edit(text string) {
	js, _ := json.Marshal(text)
	chromedp.Run(u.ctx, chromedp.Evaluate[any]("ui.edit("+string(js)+")"))
}

// blur takes the keyboard from the box.
func (u *iface) blur() {
	chromedp.Run(u.ctx, chromedp.Evaluate[any]("ui.blur()"))
}

// close closes the interface tab.
func (u *iface) close() {
	u.cancel()
}
