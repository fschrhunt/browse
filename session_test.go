package main

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

// testChrome returns the headless shell, or skips the test when none is installed.
func testChrome(t *testing.T) string {
	t.Helper()
	chrome, err := findChrome()
	if err != nil {
		t.Skip(err)
	}
	return chrome
}

// newTestSession serves pages, keyed by path, from a local server, and opens
// "/" in a browser with a throwaway profile. It skips the cookie wait, since
// these tests do not check persistence.
func newTestSession(t *testing.T, pages map[string]string) (*Session, string) {
	t.Helper()
	chrome := testChrome(t)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		page, ok := pages[r.URL.Path]
		if !ok {
			http.NotFound(w, r)
			return
		}
		w.Header().Set("Content-Type", "text/html")
		w.Write([]byte(page))
	}))
	t.Cleanup(srv.Close)

	s, err := NewSession(chrome, t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	s.cookieWait = 0
	t.Cleanup(func() { s.Shutdown() })
	info, err := s.Open(srv.URL + "/")
	if err != nil {
		t.Fatal(err)
	}
	return s, info.Tab
}

// evalString runs js in the tab and returns its string result.
func evalString(t *testing.T, s *Session, tab, js string) string {
	t.Helper()
	v, err := s.Eval(tab, js)
	if err != nil {
		t.Fatal(err)
	}
	str, _ := v.(string)
	return str
}

// While Shutdown waits for cookies to be written, other commands fail at once
// instead of queueing behind it.
func TestCommandsFailFastDuringShutdown(t *testing.T) {
	s, tab := newTestSession(t, map[string]string{"/": `<p>x</p>`})
	s.cookieWait = time.Second
	s.Eval(tab, "1") // an action, so Shutdown waits
	done := make(chan error)
	go func() { done <- s.Shutdown() }()
	time.Sleep(100 * time.Millisecond)

	start := time.Now()
	_, err := s.Text(tab, "", false)
	if err == nil || !strings.Contains(err.Error(), "shutting down") {
		t.Errorf("want a shutting down error, got %v", err)
	}
	if waited := time.Since(start); waited > 500*time.Millisecond {
		t.Errorf("command waited %v for shutdown", waited)
	}
	if err := <-done; err != nil {
		t.Errorf("shutdown: %v", err)
	}
}

// Logins live in the profile, so a session started later on the same profile
// still has the cookie that an earlier session got. The server sets the cookie
// only on /set, so the check on /check can only see it if the profile kept it.
func TestCookiesPersistAcrossSessions(t *testing.T) {
	if testing.Short() {
		t.Skip("shutdown waits for cookies to be written, which takes about 30 seconds")
	}
	chrome := testChrome(t)
	profile := t.TempDir()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/set" {
			http.SetCookie(w, &http.Cookie{Name: "login", Value: "ok", Path: "/", MaxAge: 86400})
		}
		w.Write([]byte("<p>hi</p>"))
	}))
	t.Cleanup(srv.Close)

	first, err := NewSession(chrome, profile)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := first.Open(srv.URL + "/set"); err != nil {
		t.Fatal(err)
	}
	if err := first.Shutdown(); err != nil {
		t.Fatal(err)
	}

	second, err := NewSession(chrome, profile)
	if err != nil {
		t.Fatal(err)
	}
	second.cookieWait = 0
	t.Cleanup(func() { second.Shutdown() })
	info, err := second.Open(srv.URL + "/check")
	if err != nil {
		t.Fatal(err)
	}
	if cookies := evalString(t, second, info.Tab, "document.cookie"); !strings.Contains(cookies, "login=ok") {
		t.Errorf("cookie lost across sessions: %q", cookies)
	}
}

// Sites block HeadlessChrome and automated browsers, so the browser presents
// itself as desktop Chrome in its user agent, client hints, and navigator.webdriver.
func TestUserAgentIsNotHeadless(t *testing.T) {
	s, tab := newTestSession(t, map[string]string{"/": `<p>x</p>`})

	for _, js := range []string{
		"navigator.userAgent",
		"navigator.userAgentData.brands.map(b => b.brand).join(', ')",
		"String(navigator.webdriver)",
	} {
		got := evalString(t, s, tab, js)
		if strings.Contains(got, "Headless") || got == "true" {
			t.Errorf("%s = %q", js, got)
		}
	}
}

// A page that opens a tab, like a login popup, gets it tracked as the new
// active tab, and the action that opened it says so.
func TestPopupBecomesActiveTab(t *testing.T) {
	s, tab := newTestSession(t, map[string]string{
		"/":      `<a href="/other" target="_blank">Other</a>`,
		"/other": `<title>Other</title>`,
	})
	s.Snapshot(tab)

	res, err := s.Click(tab, "e1")
	if err != nil {
		t.Fatal(err)
	}
	if len(res.Opened) != 1 {
		t.Fatalf("want one opened tab, got %v", res.Opened)
	}
	tabs, err := s.List()
	if err != nil {
		t.Fatal(err)
	}
	if len(tabs) != 2 || !tabs[1].Active || tabs[1].Title != "Other" {
		t.Errorf("tabs after popup: %+v", tabs)
	}
}

func TestResolveURL(t *testing.T) {
	cases := map[string]string{
		"x.com":                 "https://x.com",
		"x.com/home":            "https://x.com/home",
		"localhost:3000":        "http://localhost:3000",
		"http://localhost:3000": "http://localhost:3000",
		"about:blank":           "about:blank",
		"chromedp docs":         "https://duckduckgo.com/?q=chromedp+docs",
	}
	for in, want := range cases {
		if got := resolveURL(in); got != want {
			t.Errorf("resolveURL(%q) = %q, want %q", in, got, want)
		}
	}
}
