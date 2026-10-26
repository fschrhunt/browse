// Commands that work on one tab. Each runs under the tab's lock through act,
// which waits for the page to settle and reports where the action left the
// browser, or through read, which only returns what it read. Both turn a dialog
// that opens mid-command into a message that says how to answer it.
package main

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"time"
	"unicode/utf8"

	"github.com/chromedp/cdproto/cdp"
	"github.com/chromedp/cdproto/dom"
	"github.com/chromedp/cdproto/input"
	"github.com/chromedp/cdproto/page"
	"github.com/chromedp/cdproto/runtime"
	"github.com/chromedp/cdproto/target"
	"github.com/chromedp/chromedp"
	"github.com/chromedp/chromedp/kb"
)

const (
	settleTimeout     = 10 * time.Second
	waitTimeout       = 15 * time.Second
	screenshotTimeout = 5 * time.Second
	snapshotLimit     = 400   // elements per snapshot
	textLimit         = 20000 // characters, unless --all
)

// Element is one element from the last snapshot of its tab. Checked is set for
// checkboxes, radios, and switches; Options for selects.
type Element struct {
	Ref       string   `json:"ref"`
	Role      string   `json:"role"`
	Name      string   `json:"name"`
	Value     string   `json:"value,omitempty"`
	Checked   *bool    `json:"checked,omitempty"`
	Disabled  bool     `json:"disabled,omitempty"`
	Offscreen bool     `json:"offscreen,omitempty"`
	Options   []string `json:"options,omitempty"`
}

// Snapshot is the page plus its elements. More counts elements past the limit.
type Snapshot struct {
	Tab      string    `json:"tab"`
	URL      string    `json:"url"`
	Title    string    `json:"title"`
	Elements []Element `json:"elements"`
	More     int       `json:"more,omitempty"`
}

// Shot is a screenshot of a tab, with the page it shows.
type Shot struct {
	URL   string `json:"url"`
	Title string `json:"title"`
	PNG   []byte `json:"png"`
}

// Text is page or element text. Total is the full length when Text is cut short.
type Text struct {
	Text  string `json:"text"`
	Total int    `json:"total"`
}

// lastID is the number of the newest tab, for finding tabs opened after it.
func (s *Session) lastID() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.nextID
}

// openedAfter lists the tabs, still open, that were added after tab number n.
func (s *Session) openedAfter(n int) []string {
	s.mu.Lock()
	defer s.mu.Unlock()
	var ids []string
	for i := n + 1; i <= s.nextID; i++ {
		if id := fmt.Sprintf("t%d", i); s.tabs[id] != nil {
			ids = append(ids, id)
		}
	}
	return ids
}

// read runs fn on a tab under its lock and returns fn's error.
func (s *Session) read(id string, acts bool, fn func(ctx context.Context, t *tab) error) error {
	t, err := s.use(id, acts)
	if err != nil {
		return err
	}
	t.mu.Lock()
	defer t.mu.Unlock()
	if d := t.pendingDialog(); d != nil {
		return dialogError(d)
	}
	ctx, end := t.begin()
	defer end()
	if err := fn(ctx, t); err != nil {
		if d := t.pendingDialog(); d != nil {
			return dialogError(d)
		}
		return err
	}
	return nil
}

// act runs an action on a tab under its lock, waits for the page to settle, and
// reports where the browser ended up. A confirm or prompt that the action
// opened is reported in the result, since the action itself happened.
func (s *Session) act(id string, acts bool, fn func(ctx context.Context) error) (TabInfo, error) {
	t, err := s.use(id, acts)
	if err != nil {
		return TabInfo{}, err
	}
	t.mu.Lock()
	defer t.mu.Unlock()
	if d := t.pendingDialog(); d != nil {
		return TabInfo{}, dialogError(d)
	}

	before := s.lastID()
	ctx, end := t.begin()
	loading := watchLoading(ctx, t.target)
	err = fn(ctx)
	if err == nil {
		settle(ctx, loading)
	}
	end()
	if errors.Is(err, context.DeadlineExceeded) {
		err = fmt.Errorf("the page did not load within %s; the site may be down or slow", commandTimeout)
	}
	if err != nil && t.pendingDialog() == nil {
		return TabInfo{}, err
	}

	info, err := s.info(t)
	if err != nil {
		return TabInfo{}, err
	}
	info.Opened = s.openedAfter(before)
	t.state.Lock()
	info.Alerts, t.alerts = t.alerts, nil
	t.state.Unlock()
	return info, nil
}

// watchLoading counts, until ctx ends, how many loads of the tab's main frame
// have started and stopped. Subscribing happens before it returns, so no load
// that the action starts is missed.
func watchLoading(ctx context.Context, main target.ID) func() bool {
	var started, stopped atomic.Int32
	starts := chromedp.Events(ctx, page.FrameStartedLoading)
	stops := chromedp.Events(ctx, page.FrameStoppedLoading)
	go func() {
		for ev, err := range starts {
			if err == nil && ev.FrameID == cdp.FrameID(main) {
				started.Add(1)
			}
		}
	}()
	go func() {
		for ev, err := range stops {
			if err == nil && ev.FrameID == cdp.FrameID(main) {
				stopped.Add(1)
			}
		}
	}()
	return func() bool { return started.Load() > stopped.Load() }
}

// settle waits for the page to finish what an action started: a navigation,
// the load, and DOM updates. It gives up quietly after settleTimeout, since
// some pages never go quiet.
func settle(ctx context.Context, loading func() bool) {
	ctx, cancel := context.WithTimeout(ctx, settleTimeout)
	defer cancel()
	pause := func(d time.Duration) bool {
		select {
		case <-ctx.Done():
			return false
		case <-time.After(d):
			return true
		}
	}
	// Give a click's navigation a moment to start.
	if !pause(100 * time.Millisecond) {
		return
	}
	for {
		for loading() {
			if !pause(50 * time.Millisecond) {
				return
			}
		}
		// A navigation replaces the page mid-evaluation, which fails; the next
		// round waits for the new page.
		_, err := chromedp.Run(ctx, chromedp.Evaluate[bool](settleJS, chromedp.EvalAwaitPromise))
		if err == nil && !loading() {
			return
		}
		if !pause(50 * time.Millisecond) {
			return
		}
	}
}

// refError says a ref is gone and what to do about it.
func refError(ref string) error {
	return fmt.Errorf("no element %s; run browse snapshot again", ref)
}

type spot struct {
	X       float64 `json:"x"`
	Y       float64 `json:"y"`
	Missing bool    `json:"missing"`
	Hidden  bool    `json:"hidden"`
}

// locateSpot scrolls the element into view and returns its center.
func locateSpot(ctx context.Context, ref string) (spot, error) {
	p, err := chromedp.Run(ctx, chromedp.Evaluate[spot](pageCall("point", ref)))
	switch {
	case err != nil:
		return p, err
	case p.Missing:
		return p, refError(ref)
	case p.Hidden:
		return p, fmt.Errorf("element %s is not visible", ref)
	}
	return p, nil
}

// Goto loads url in the tab.
func (s *Session) Goto(id, rawURL string) (TabInfo, error) {
	return s.act(id, true, func(ctx context.Context) error {
		return chromedp.Do(ctx, chromedp.Navigate(resolveURL(rawURL)))
	})
}

// Back, Forward, and Reload move through the tab's history.
func (s *Session) Back(id string) (TabInfo, error) {
	return s.act(id, true, func(ctx context.Context) error { return chromedp.Do(ctx, chromedp.NavigateBack()) })
}

func (s *Session) Forward(id string) (TabInfo, error) {
	return s.act(id, true, func(ctx context.Context) error { return chromedp.Do(ctx, chromedp.NavigateForward()) })
}

func (s *Session) Reload(id string) (TabInfo, error) {
	return s.act(id, true, func(ctx context.Context) error { return chromedp.Do(ctx, chromedp.Reload()) })
}

// Stop stops the tab's page loading. It skips the tab's lock, since what it
// stops is usually an action holding it.
func (s *Session) Stop(id string) (TabInfo, error) {
	t, err := s.use(id, false)
	if err != nil {
		return TabInfo{}, err
	}
	if _, err := chromedp.Call(t.ctx, page.StopLoading, cdp.Empty{}); err != nil {
		return TabInfo{}, err
	}
	return s.info(t)
}

// Click clicks the element with a real mouse event at its center, as a person
// would, so whatever is on top at that point gets the click.
func (s *Session) Click(id, ref string) (TabInfo, error) {
	return s.act(id, true, func(ctx context.Context) error {
		p, err := locateSpot(ctx, ref)
		if err != nil {
			return err
		}
		return chromedp.Do(ctx, chromedp.MouseClickXY(p.X, p.Y))
	})
}

// Hover moves the mouse over the element, which opens hover menus.
func (s *Session) Hover(id, ref string) (TabInfo, error) {
	return s.act(id, true, func(ctx context.Context) error {
		p, err := locateSpot(ctx, ref)
		if err != nil {
			return err
		}
		return chromedp.Do(ctx, chromedp.MouseEvent(input.DispatchMouseEventTypeMouseMoved, p.X, p.Y))
	})
}

// Fill replaces the contents of an input, textarea, or editable element. The
// text goes in through Chrome's text input, so frameworks and rich editors see
// a real edit.
func (s *Session) Fill(id, ref, text string) (TabInfo, error) {
	return s.act(id, true, func(ctx context.Context) error {
		status, err := chromedp.Run(ctx, chromedp.Evaluate[string](pageCall("focusField", ref)))
		switch {
		case err != nil:
			return err
		case status == "missing":
			return refError(ref)
		case status != "ok":
			return fmt.Errorf("element %s is %s", ref, status)
		}
		if text == "" {
			return chromedp.Do(ctx, chromedp.KeyEvent(kb.Backspace))
		}
		_, err = chromedp.Call(ctx, input.InsertText, input.InsertTextParams{Text: text})
		return err
	})
}

// Type inserts text where the focus is, without clearing anything.
func (s *Session) Type(id, text string) (TabInfo, error) {
	return s.act(id, true, func(ctx context.Context) error {
		_, err := chromedp.Call(ctx, input.InsertText, input.InsertTextParams{Text: text})
		return err
	})
}

var namedKeys = map[string]string{
	"enter": kb.Enter, "tab": kb.Tab, "escape": kb.Escape, "esc": kb.Escape,
	"backspace": kb.Backspace, "delete": kb.Delete, "space": " ",
	"arrowup": kb.ArrowUp, "arrowdown": kb.ArrowDown, "arrowleft": kb.ArrowLeft, "arrowright": kb.ArrowRight,
	"up": kb.ArrowUp, "down": kb.ArrowDown, "left": kb.ArrowLeft, "right": kb.ArrowRight,
	"home": kb.Home, "end": kb.End, "pageup": kb.PageUp, "pagedown": kb.PageDown,
}

var modifierKeys = map[string]chromedp.Modifier{
	"control": chromedp.ModifierCtrl, "ctrl": chromedp.ModifierCtrl,
	"shift": chromedp.ModifierShift, "alt": chromedp.ModifierAlt,
	"meta": chromedp.ModifierMeta, "cmd": chromedp.ModifierMeta,
}

// keyFor turns a key name like Enter, or a single character, into what
// chromedp.KeyEvent types.
func keyFor(name string) (string, error) {
	if named, ok := namedKeys[strings.ToLower(name)]; ok {
		return named, nil
	}
	if utf8.RuneCountInString(name) != 1 {
		return "", fmt.Errorf("unknown key %q; use a single character or a name like Enter, Escape, Tab, ArrowDown", name)
	}
	return name, nil
}

// parseKeys reads a key spec like "Enter", "a", or "Control+Shift+K".
func parseKeys(spec string) (string, []chromedp.Modifier, error) {
	parts := strings.Split(spec, "+")
	var mods []chromedp.Modifier
	for _, name := range parts[:len(parts)-1] {
		mod, ok := modifierKeys[strings.ToLower(name)]
		if !ok {
			return "", nil, fmt.Errorf("unknown modifier %q; use Control, Shift, Alt, or Meta", name)
		}
		mods = append(mods, mod)
	}
	key, err := keyFor(parts[len(parts)-1])
	return key, mods, err
}

// Press presses a key, with modifiers, where the focus is.
func (s *Session) Press(id, spec string) (TabInfo, error) {
	key, mods, err := parseKeys(spec)
	if err != nil {
		return TabInfo{}, err
	}
	return s.act(id, true, func(ctx context.Context) error {
		return chromedp.Do(ctx, chromedp.KeyEvent(key, chromedp.KeyModifiers(mods...)))
	})
}

type chosen struct {
	Selected string `json:"selected"`
	Missing  bool   `json:"missing"`
	Error    string `json:"error"`
}

// Select picks an option of a select element by its value or visible text.
func (s *Session) Select(id, ref, choice string) (TabInfo, error) {
	return s.act(id, true, func(ctx context.Context) error {
		c, err := chromedp.Run(ctx, chromedp.Evaluate[chosen](pageCall("choose", ref, choice)))
		switch {
		case err != nil:
			return err
		case c.Missing:
			return refError(ref)
		case c.Error != "":
			return fmt.Errorf("element %s %s", ref, c.Error)
		}
		return nil
	})
}

// Scroll scrolls the page down or up by most of a screen, to the top or
// bottom, or until the element with a ref is in view. Down and up use the
// mouse wheel at the middle of the page, so inner scrolling panes move too.
func (s *Session) Scroll(id, where string) (TabInfo, error) {
	return s.act(id, true, func(ctx context.Context) error {
		switch where {
		case "", "down", "up":
			delta := float64(viewportHeight) * 0.8
			if where == "up" {
				delta = -delta
			}
			return wheel(ctx, viewportWidth/2, viewportHeight/2, 0, delta, 0)
		case "top", "bottom":
			_, err := chromedp.Run(ctx, chromedp.Evaluate[any](pageCall("scrollEdge", where)))
			return err
		}
		_, err := locateSpot(ctx, strings.TrimPrefix(where, "@"))
		return err
	})
}

// Upload sets the files of a file input.
func (s *Session) Upload(id, ref string, files []string) (TabInfo, error) {
	paths := make([]string, len(files))
	for i, f := range files {
		abs, err := filepath.Abs(f)
		if err != nil {
			return TabInfo{}, err
		}
		if _, err := os.Stat(abs); err != nil {
			return TabInfo{}, err
		}
		paths[i] = abs
	}
	return s.act(id, true, func(ctx context.Context) error {
		el, err := chromedp.Run(ctx, chromedp.Evaluate[*runtime.RemoteObject](pageCall("locate", ref)))
		if err != nil {
			return err
		}
		if el.ObjectID == "" {
			return refError(ref)
		}
		_, err = chromedp.Call(ctx, dom.SetFileInputFiles, dom.SetFileInputFilesParams{Files: paths, ObjectID: el.ObjectID})
		return err
	})
}

// Wait waits for a duration like 2s, or until text appears on the page.
func (s *Session) Wait(id, what string) (TabInfo, error) {
	return s.act(id, false, func(ctx context.Context) error {
		if d, err := time.ParseDuration(what); err == nil {
			select {
			case <-ctx.Done():
				return ctx.Err()
			case <-time.After(d):
				return nil
			}
		}
		ctx, cancel := context.WithTimeout(ctx, waitTimeout)
		defer cancel()
		check := fmt.Sprintf(`!!document.body && document.body.innerText.includes(%q)`, what)
		for {
			// Errors mean the page is navigating; keep checking the next one.
			if found, err := chromedp.Run(ctx, chromedp.Evaluate[bool](check)); err == nil && found {
				return nil
			}
			select {
			case <-ctx.Done():
				return fmt.Errorf("%q did not appear within %s", what, waitTimeout)
			case <-time.After(250 * time.Millisecond):
			}
		}
	})
}

// Dialog accepts or dismisses the tab's open confirm or prompt. text answers a prompt.
func (s *Session) Dialog(id string, accept bool, text string) (TabInfo, error) {
	t, err := s.use(id, true)
	if err != nil {
		return TabInfo{}, err
	}
	if t.pendingDialog() == nil {
		return TabInfo{}, errors.New("no dialog is open")
	}
	ctx, cancel := context.WithTimeout(t.ctx, commandTimeout)
	defer cancel()
	if _, err := chromedp.Call(ctx, page.HandleJavaScriptDialog, page.HandleJavaScriptDialogParams{Accept: accept, PromptText: text}); err != nil {
		return TabInfo{}, err
	}
	t.state.Lock()
	t.dialog = nil
	t.state.Unlock()
	// Settle, since answering may navigate.
	return s.act(t.id, false, func(context.Context) error { return nil })
}

// Snapshot tags the page's elements and returns them with their refs.
func (s *Session) Snapshot(id string) (Snapshot, error) {
	var snap Snapshot
	err := s.read(id, false, func(ctx context.Context, t *tab) error {
		var err error
		snap, err = chromedp.Run(ctx, chromedp.Evaluate[Snapshot](pageCall("snapshot", snapshotLimit)))
		snap.Tab = t.id
		return err
	})
	return snap, err
}

// textResult is what textOf returns. Its fields are spelled out, since the
// JSON decoder chromedp uses does not inline embedded structs.
type textResult struct {
	Text    string `json:"text"`
	Total   int    `json:"total"`
	Missing bool   `json:"missing"`
}

// Text returns the visible text of the page, or of one element. Unless all is
// set, it stops at textLimit characters.
func (s *Session) Text(id, ref string, all bool) (Text, error) {
	limit := textLimit
	if all {
		limit = 0
	}
	var res textResult
	err := s.read(id, false, func(ctx context.Context, t *tab) error {
		var err error
		res, err = chromedp.Run(ctx, chromedp.Evaluate[textResult](pageCall("textOf", ref, limit)))
		if err == nil && res.Missing {
			return refError(ref)
		}
		return err
	})
	return Text{Text: res.Text, Total: res.Total}, err
}

// Eval runs JavaScript in the page and returns its JSON-compatible result. A
// promise is awaited, so fetch(...).then(...) returns its final value.
func (s *Session) Eval(id, js string) (any, error) {
	var value any
	err := s.read(id, true, func(ctx context.Context, t *tab) error {
		var err error
		value, err = chromedp.Run(ctx, chromedp.Evaluate[any](js, chromedp.EvalAwaitPromise))
		return err
	})
	return value, err
}

// Screenshot captures the tab's viewport, or the whole page when full is set.
// It does not wait for the tab's lock, so a viewer keeps drawing while an
// action runs.
func (s *Session) Screenshot(id string, full bool) (Shot, error) {
	t, err := s.use(id, false)
	if err != nil {
		return Shot{}, err
	}
	if d := t.pendingDialog(); d != nil {
		return Shot{}, dialogError(d)
	}
	info, err := s.info(t)
	if err != nil {
		return Shot{}, err
	}
	ctx, cancel := context.WithTimeout(t.ctx, screenshotTimeout)
	defer cancel()
	capture := chromedp.CaptureScreenshot()
	if full {
		capture = chromedp.FullScreenshot(100)
	}
	png, err := chromedp.Run(ctx, capture)
	return Shot{URL: info.URL, Title: info.Title, PNG: png}, err
}
