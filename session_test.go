package main

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
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
// "/" in a browser with a throwaway profile.
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

// Logins live in the profile, so a session started later on the same profile
// still has the cookie that an earlier session got, even when the first
// session is shut down right after getting it. The server sets the cookie
// only on /set, so the check on /check can only see it if the profile kept it.
func TestCookiesPersistAcrossSessions(t *testing.T) {
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
