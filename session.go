// The browser session: one headless Chromium shell and the tabs open in it.
// Each tab is a chromedp context, so closing a tab's context closes its target.
// Tabs opened by pages, such as login popups, are adopted as tabs too.
//
// Locking: s.mu guards the tab list and is never held while waiting on Chrome.
// Each tab has its own lock that serializes the actions on it, so a slow page
// does not hold up other tabs, and snapshot refs stay consistent per tab.
package main

import (
	"cmp"
	"context"
	"errors"
	"fmt"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/chromedp/cdproto/browser"
	"github.com/chromedp/cdproto/emulation"
	"github.com/chromedp/cdproto/page"
	"github.com/chromedp/cdproto/target"
	"github.com/chromedp/chromedp"
)

const (
	viewportWidth  = 1280
	viewportHeight = 800
	commandTimeout = 30 * time.Second

	// Chrome writes cookies to the profile about 30 seconds after a change, and
	// closing a headless shell does not write them sooner. Shutdown waits until
	// this long after the last action that could change cookies, so the last
	// logins are on disk. TestCookiesPersistAcrossSessions pins this behavior
	// for the Chrome version that scripts/install.sh installs.
	defaultCookieWait = 31 * time.Second
)

var errStopping = errors.New("browser is shutting down")

type tab struct {
	id     string
	ctx    context.Context
	cancel context.CancelFunc
	target target.ID

	loading func() bool // whether the page is loading, for the viewer's bar

	mu sync.Mutex // serializes actions on this tab

	// state guards the fields below. It is only held briefly, never while
	// waiting on Chrome, so the dialog watcher can always take it.
	state     sync.Mutex
	dialog    *page.EventJavascriptDialogOpening // an open confirm or prompt
	interrupt context.CancelFunc                 // cancels the running action
	alerts    []string                           // alerts accepted and not yet reported
	agentAt   time.Time                          // when an agent last acted on the tab
}

type Session struct {
	allocEnd context.CancelFunc
	// anchor is a hidden tab that launched the browser. New tabs are made from
	// it, since a context made from anything else launches a second Chrome.
	// Only it can close Chrome gracefully, and browser-level calls go through it.
	anchor context.Context
	// identity is applied to every tab; nil keeps Chrome's own.
	identity *emulation.SetUserAgentOverrideParams
	// cookieWait is how long after the last action Shutdown waits. Tests that do
	// not check persistence set it to zero.
	cookieWait time.Duration

	mu       sync.Mutex
	tabs     map[string]*tab
	order    []string // tab ids, oldest first
	nextID   int
	active   string
	lastAct  time.Time // when a command last could have changed cookies
	stopping bool
}

// TabInfo describes a tab: in listings, and after an action, where the action
// left the browser. Opened lists tabs the page opened during the action, which
// become active. Dialog is a confirm or prompt that waits for browse dialog.
type TabInfo struct {
	Tab    string   `json:"tab"`
	Active bool     `json:"active"`
	URL    string   `json:"url"`
	Title  string   `json:"title"`
	Dialog string   `json:"dialog,omitempty"`
	Opened []string `json:"opened,omitempty"`
	Alerts []string `json:"alerts,omitempty"`
}

// identity is how desktop Chrome at the installed version presents itself:
// its user agent and the client hints that go with it. The headless shell's
// own say HeadlessChrome, which sites block. It returns nil when the version
// cannot be read, which keeps Chrome's defaults.
func identity(chromePath string) *emulation.SetUserAgentOverrideParams {
	out, err := exec.Command(chromePath, "--version").Output()
	fields := strings.Fields(string(out))
	if err != nil || len(fields) == 0 {
		return nil
	}
	full := fields[len(fields)-1]
	major, _, _ := strings.Cut(full, ".")

	platform, navPlatform, uaPlatform, arch := "Linux", "Linux x86_64", "X11; Linux x86_64", "x86"
	if runtime.GOOS == "darwin" {
		platform, navPlatform, uaPlatform = "macOS", "MacIntel", "Macintosh; Intel Mac OS X 10_15_7"
	}
	if runtime.GOARCH == "arm64" {
		arch = "arm"
	}
	brands := func(version string) []*emulation.UserAgentBrandVersion {
		return []*emulation.UserAgentBrandVersion{
			{Brand: "Google Chrome", Version: version},
			{Brand: "Chromium", Version: version},
			{Brand: "Not:A-Brand", Version: "24"},
		}
	}
	return &emulation.SetUserAgentOverrideParams{
		UserAgent: fmt.Sprintf("Mozilla/5.0 (%s) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/%s.0.0.0 Safari/537.36", uaPlatform, major),
		Platform:  navPlatform,
		UserAgentMetadata: &emulation.UserAgentMetadata{
			Brands:          brands(major),
			FullVersionList: brands(full),
			Platform:        platform,
			Architecture:    arch,
			Bitness:         "64",
		},
	}
}

// NewSession starts Chrome on profileDir. Cookies and logins are kept there, so
// they survive restarts. Sites see an ordinary browser: no automation flags,
// navigator.webdriver is false, and the user agent and client hints are
// desktop Chrome's. Downloads go to ~/Downloads. Chrome's home is inside
// profileDir too, so the files it keeps in a home, such as its certificate
// store and font cache, are browse's alone and go when the profile does.
func NewSession(chromePath, profileDir string) (*Session, error) {
	home := filepath.Join(profileDir, "home")
	opts := append(chromedp.DefaultExecAllocatorOptions[:],
		chromedp.ExecPath(chromePath),
		chromedp.UserDataDir(profileDir),
		chromedp.Env("HOME="+home,
			"XDG_CONFIG_HOME="+filepath.Join(home, ".config"),
			"XDG_CACHE_HOME="+filepath.Join(home, ".cache"),
			"XDG_DATA_HOME="+filepath.Join(home, ".local/share")),
		chromedp.Flag("enable-automation", false),
		chromedp.Flag("disable-blink-features", "AutomationControlled"),
	)
	ua := identity(chromePath)
	if ua != nil {
		// Also covers requests made before a tab gets its override, such as a
		// popup's first load.
		opts = append(opts, chromedp.UserAgent(ua.UserAgent))
	}
	alloc, allocEnd := chromedp.NewExecAllocator(context.Background(), opts...)
	anchor, _ := chromedp.NewContext(alloc)
	// The first action on the anchor launches Chrome.
	if err := chromedp.Do(anchor, chromedp.Navigate("about:blank")); err != nil {
		allocEnd()
		return nil, err
	}
	if err := allowDownloads(anchor); err != nil {
		allocEnd()
		return nil, err
	}

	s := &Session{
		allocEnd:   allocEnd,
		anchor:     anchor,
		identity:   ua,
		cookieWait: defaultCookieWait,
		tabs:       map[string]*tab{},
	}
	go s.watchOpened()
	go s.watchClosed()
	return s, nil
}

// allowDownloads saves downloads to ~/Downloads under their own names. A
// headless shell drops them otherwise.
func allowDownloads(ctx context.Context) error {
	home, err := os.UserHomeDir()
	if err != nil {
		return err
	}
	dir := filepath.Join(home, "Downloads")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return err
	}
	_, err = chromedp.CallBrowser(ctx, browser.SetDownloadBehavior, browser.SetDownloadBehaviorParams{
		Behavior:     browser.SetDownloadBehaviorBehaviorAllow,
		DownloadPath: dir,
	})
	return err
}

// watchOpened adopts tabs that pages open, such as target=_blank links and
// login popups. Tabs that browse opens itself have no opener and are skipped.
func (s *Session) watchOpened() {
	for ev, err := range chromedp.BrowserEvents(s.anchor, target.TargetCreated) {
		if err != nil {
			return
		}
		if info := ev.TargetInfo; info.Type == "page" && info.OpenerID != "" {
			go s.adopt(info.TargetID)
		}
	}
}

func (s *Session) adopt(id target.ID) {
	ctx, cancel := chromedp.NewContext(s.anchor, chromedp.WithTargetID(id))
	if err := s.prepare(ctx); err != nil {
		cancel()
		return
	}
	s.register(ctx, cancel)
}

// prepare attaches to a tab and sets it up like every other: the viewport,
// focus, and the desktop Chrome identity.
func (s *Session) prepare(ctx context.Context) error {
	if err := chromedp.Do(ctx, chromedp.EmulateViewport(viewportWidth, viewportHeight)); err != nil {
		return err
	}
	// Pages act as if focused, so focus events fire and carets show, as they
	// would in a browser window that has focus.
	if _, err := chromedp.Call(ctx, emulation.SetFocusEmulationEnabled, emulation.SetFocusEmulationEnabledParams{Enabled: true}); err != nil {
		return err
	}
	if s.identity == nil {
		return nil
	}
	_, err := chromedp.Call(ctx, emulation.SetUserAgentOverride, *s.identity)
	return err
}

// watchClosed forgets tabs whose page closed itself, as login popups do when
// they are done.
func (s *Session) watchClosed() {
	for ev, err := range chromedp.BrowserEvents(s.anchor, target.TargetDestroyed) {
		if err != nil {
			return
		}
		s.mu.Lock()
		for id, t := range s.tabs {
			if t.target == ev.TargetID {
				s.forget(id)
				t.cancel()
			}
		}
		s.mu.Unlock()
	}
}

// register adds an attached tab context as the new active tab, and starts
// watching its dialogs.
func (s *Session) register(ctx context.Context, cancel context.CancelFunc) *tab {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.nextID++
	t := &tab{
		id:     fmt.Sprintf("t%d", s.nextID),
		ctx:    ctx,
		cancel: cancel,
		target: chromedp.FromContext(ctx).Target.TargetID,
	}
	t.loading = watchLoading(ctx, t.target)
	s.tabs[t.id] = t
	s.order = append(s.order, t.id)
	s.active = t.id
	go t.watchDialogs()
	return t
}

// forget removes a tab from the list. The caller holds s.mu.
func (s *Session) forget(id string) {
	delete(s.tabs, id)
	s.order = slices.DeleteFunc(s.order, func(other string) bool { return other == id })
	if s.active == id {
		s.active = ""
		if len(s.order) > 0 {
			s.active = s.order[len(s.order)-1]
		}
	}
}

// watchDialogs answers alerts and beforeunload prompts at once, keeping the
// alert text for the action's result. A confirm or prompt is left open for
// browse dialog, since accepting one could approve something destructive; the
// running action is interrupted so it reports the dialog instead of hanging.
func (t *tab) watchDialogs() {
	for ev, err := range chromedp.Events(t.ctx, page.JavascriptDialogOpening) {
		if err != nil {
			return
		}
		t.state.Lock()
		switch ev.Type {
		case page.DialogTypeConfirm, page.DialogTypePrompt:
			t.dialog = &ev
			if t.interrupt != nil {
				t.interrupt()
			}
		default:
			if ev.Type == page.DialogTypeAlert {
				t.alerts = append(t.alerts, ev.Message)
			}
			go chromedp.Call(t.ctx, page.HandleJavaScriptDialog, page.HandleJavaScriptDialogParams{Accept: true})
		}
		t.state.Unlock()
	}
}

// dialogError says which dialog is open and how to answer it.
func dialogError(d *page.EventJavascriptDialogOpening) error {
	return fmt.Errorf("a %s dialog is open: %q; answer it with browse dialog accept or browse dialog dismiss", d.Type, d.Message)
}

// pendingDialog returns the open confirm or prompt, if any.
func (t *tab) pendingDialog() *page.EventJavascriptDialogOpening {
	t.state.Lock()
	defer t.state.Unlock()
	return t.dialog
}

// begin starts an action on t: a context with the command timeout that an
// opening dialog interrupts. The returned func ends the action.
func (t *tab) begin() (context.Context, func()) {
	ctx, cancel := context.WithTimeout(t.ctx, commandTimeout)
	t.state.Lock()
	t.interrupt = cancel
	t.state.Unlock()
	return ctx, func() {
		t.state.Lock()
		t.interrupt = nil
		t.state.Unlock()
		cancel()
	}
}

// use finds the tab for a command, or the active tab when id is empty. acts
// marks commands that can change cookies, which Shutdown waits on. It fails
// once shutdown has begun.
func (s *Session) use(id string, acts bool) (*tab, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.stopping {
		return nil, errStopping
	}
	if acts {
		s.lastAct = time.Now()
	}
	id = cmp.Or(id, s.active)
	if id == "" {
		return nil, errors.New("no tabs open; run browse open <url>")
	}
	t, ok := s.tabs[id]
	if !ok {
		return nil, fmt.Errorf("no tab %s", id)
	}
	return t, nil
}

// info reads a tab's URL and title from Chrome's target list, which works even
// while the page is busy or a dialog is open.
func (s *Session) info(t *tab) (TabInfo, error) {
	res, err := chromedp.CallBrowser(s.anchor, target.GetTargetInfo, target.GetTargetInfoParams{TargetID: t.target})
	if err != nil {
		return TabInfo{}, err
	}
	s.mu.Lock()
	active := s.active == t.id
	s.mu.Unlock()
	info := TabInfo{Tab: t.id, Active: active, URL: res.TargetInfo.URL, Title: res.TargetInfo.Title}
	if d := t.pendingDialog(); d != nil {
		info.Dialog = fmt.Sprintf("%s %q", d.Type, d.Message)
	}
	return info, nil
}

// resolveURL turns an address a person types in the bar, or an agent passes
// to open or goto, into a URL to load. An existing file loads as a file URL, a bare
// host like x.com gets https:// (http:// for localhost, where dev servers
// run), and anything else is a search.
func resolveURL(raw string) string {
	if abs, err := filepath.Abs(raw); err == nil {
		if _, err := os.Stat(abs); err == nil {
			return (&url.URL{Scheme: "file", Path: abs}).String()
		}
	}
	if strings.Contains(raw, "://") || strings.HasPrefix(raw, "about:") || strings.HasPrefix(raw, "data:") {
		return raw
	}
	host, _, _ := strings.Cut(raw, "/")
	if strings.ContainsAny(raw, " \t") {
		return "https://duckduckgo.com/?q=" + url.QueryEscape(raw)
	}
	if strings.HasPrefix(host, "localhost") || strings.HasPrefix(host, "127.0.0.1") {
		return "http://" + raw
	}
	if strings.Contains(host, ".") {
		return "https://" + raw
	}
	return "https://duckduckgo.com/?q=" + url.QueryEscape(raw)
}

// Activate makes a tab the active one, which viewers show.
func (s *Session) Activate(id string) (TabInfo, error) {
	t, err := s.use(id, false)
	if err != nil {
		return TabInfo{}, err
	}
	s.mu.Lock()
	s.active = t.id
	s.mu.Unlock()
	return s.info(t)
}

// Open creates a tab, loads url in it, and makes it the active tab.
func (s *Session) Open(rawURL string) (TabInfo, error) {
	s.mu.Lock()
	stopping := s.stopping
	s.lastAct = time.Now()
	s.mu.Unlock()
	if stopping {
		return TabInfo{}, errStopping
	}

	ctx, cancel := chromedp.NewContext(s.anchor)
	if err := s.prepare(ctx); err != nil {
		cancel()
		return TabInfo{}, err
	}
	// Register before loading, so a dialog during the load is watched.
	t := s.register(ctx, cancel)
	return s.act(t.id, true, func(ctx context.Context) error {
		return chromedp.Do(ctx, chromedp.Navigate(resolveURL(rawURL)))
	})
}

// List returns every open tab, oldest first.
func (s *Session) List() ([]TabInfo, error) {
	s.mu.Lock()
	if s.stopping {
		s.mu.Unlock()
		return nil, errStopping
	}
	tabs := make([]*tab, 0, len(s.order))
	for _, id := range s.order {
		tabs = append(tabs, s.tabs[id])
	}
	s.mu.Unlock()

	list := []TabInfo{}
	for _, t := range tabs {
		info, err := s.info(t)
		if err != nil {
			return nil, err
		}
		list = append(list, info)
	}
	return list, nil
}

// Close closes a tab. If it was active, the newest remaining tab becomes active.
func (s *Session) Close(tabID string) (string, error) {
	t, err := s.use(tabID, false)
	if err != nil {
		return "", err
	}
	s.mu.Lock()
	s.forget(t.id)
	s.mu.Unlock()
	t.cancel()
	return t.id, nil
}

// Shutdown stops Chrome once the last cookie changes are on disk. New commands
// fail as soon as it starts, so the wait does not hold up other callers.
func (s *Session) Shutdown() error {
	s.mu.Lock()
	if s.stopping {
		s.mu.Unlock()
		return errors.New("browser is already shutting down")
	}
	s.stopping = true
	var wait time.Duration
	if !s.lastAct.IsZero() {
		wait = s.cookieWait - time.Since(s.lastAct)
	}
	tabs := make([]*tab, 0, len(s.tabs))
	for _, t := range s.tabs {
		tabs = append(tabs, t)
	}
	s.mu.Unlock()

	time.Sleep(wait) // returns at once when wait <= 0

	for _, t := range tabs {
		t.cancel()
	}
	// Cancelling the anchor asks Chrome to exit cleanly.
	err := chromedp.Cancel(s.anchor)
	s.allocEnd()
	return err
}
