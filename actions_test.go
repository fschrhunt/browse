package main

import (
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/chromedp/chromedp"
)

// Elements come with roles, names from their labels, values, and state, and
// hidden ones are left out.
func TestSnapshotDescribesElements(t *testing.T) {
	s, tab := newTestSession(t, map[string]string{"/": `
		<h1>Shop</h1>
		<label for="q">Search</label><input id="q" value="shoes">
		<input type="checkbox" aria-label="Remember" checked>
		<select aria-label="Size"><option>S</option><option selected>M</option></select>
		<button style="display:none">Hidden</button>
		<a href="/x">Docs</a>`})

	snap, err := s.Snapshot(tab)
	if err != nil {
		t.Fatal(err)
	}
	var lines []string
	for _, el := range snap.Elements {
		lines = append(lines, elementLine(el))
	}
	want := []string{
		`@e1 heading "Shop"`,
		`@e2 textbox "Search" value="shoes"`,
		`@e3 checkbox "Remember" checked=true`,
		`@e4 combobox "Size" value="M" options=["S" "M"]`,
		`@e5 link "Docs"`,
	}
	if !slices.Equal(lines, want) {
		t.Errorf("got\n%s\nwant\n%s", strings.Join(lines, "\n"), strings.Join(want, "\n"))
	}
}

// Snapshots and clicks reach into open shadow roots and same-origin iframes.
func TestShadowRootsAndFramesAreReachable(t *testing.T) {
	s, tab := newTestSession(t, map[string]string{
		"/": `<div id="host"></div><iframe src="/frame"></iframe>
			<script>
			  const b = document.createElement('button');
			  b.textContent = 'Shadow';
			  b.onclick = () => document.title = 'shadow';
			  document.getElementById('host').attachShadow({mode: 'open'}).append(b);
			</script>`,
		"/frame": `<button onclick="parent.document.title='inner'">Inner</button>`,
	})

	snap, err := s.Snapshot(tab)
	if err != nil {
		t.Fatal(err)
	}
	refs := map[string]string{}
	for _, el := range snap.Elements {
		refs[el.Name] = el.Ref
	}
	for name, title := range map[string]string{"Shadow": "shadow", "Inner": "inner"} {
		ref, ok := refs[name]
		if !ok {
			t.Fatalf("no %q button in %+v", name, snap.Elements)
		}
		res, err := s.Click(tab, ref)
		if err != nil {
			t.Fatal(err)
		}
		if res.Title != title {
			t.Errorf("after clicking %s: title %q", name, res.Title)
		}
	}
}

// A click that starts a slow navigation returns once the new page has loaded.
func TestClickWaitsForSlowNavigation(t *testing.T) {
	chrome := testChrome(t)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/html")
		if r.URL.Path == "/next" {
			time.Sleep(700 * time.Millisecond)
			w.Write([]byte(`<title>Next</title>`))
			return
		}
		w.Write([]byte(`<a href="/next">Next</a>`))
	}))
	t.Cleanup(srv.Close)
	s, err := NewSession(chrome, t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { s.Shutdown() })
	info, err := s.Open(srv.URL)
	if err != nil {
		t.Fatal(err)
	}
	s.Snapshot(info.Tab)

	res, err := s.Click(info.Tab, "e1")
	if err != nil {
		t.Fatal(err)
	}
	if res.Title != "Next" {
		t.Errorf("click returned on %q at %s, before the next page loaded", res.Title, res.URL)
	}
}

// When a framework replaces an element with an identical one, its ref still works.
func TestRefSurvivesRerender(t *testing.T) {
	s, tab := newTestSession(t, map[string]string{"/": `<div id="app"><button>Save</button></div>`})
	s.Snapshot(tab)
	s.Eval(tab, `document.getElementById('app').innerHTML = '<button onclick="document.title=\'saved\'">Save</button>'`)

	res, err := s.Click(tab, "e1")
	if err != nil {
		t.Fatal(err)
	}
	if res.Title != "saved" {
		t.Errorf("title %q", res.Title)
	}
}

func TestUnknownRefAsksForSnapshot(t *testing.T) {
	s, tab := newTestSession(t, map[string]string{"/": `<p>no controls</p>`})

	_, err := s.Click(tab, "e9")
	if err == nil || !strings.Contains(err.Error(), "run browse snapshot again") {
		t.Errorf("want a hint to snapshot again, got %v", err)
	}
}

func TestFillReplacesText(t *testing.T) {
	s, tab := newTestSession(t, map[string]string{"/": `<input id="q" value="old">`})
	s.Snapshot(tab)

	if _, err := s.Fill(tab, "e1", "new"); err != nil {
		t.Fatal(err)
	}
	if v := evalString(t, s, tab, "document.getElementById('q').value"); v != "new" {
		t.Errorf("value %q", v)
	}
}

func TestPressEnterSubmitsForm(t *testing.T) {
	s, tab := newTestSession(t, map[string]string{"/": `
		<form onsubmit="document.title = 'sent ' + this.q.value; return false"><input name="q"></form>`})
	s.Snapshot(tab)
	s.Fill(tab, "e1", "hi")

	res, err := s.Press(tab, "Enter")
	if err != nil {
		t.Fatal(err)
	}
	if res.Title != "sent hi" {
		t.Errorf("title %q", res.Title)
	}
}

func TestSelectByVisibleText(t *testing.T) {
	s, tab := newTestSession(t, map[string]string{"/": `
		<select onchange="document.title = this.value"><option value="s">Small</option><option value="l">Large</option></select>`})
	s.Snapshot(tab)

	res, err := s.Select(tab, "e1", "large")
	if err != nil {
		t.Fatal(err)
	}
	if res.Title != "l" {
		t.Errorf("title %q", res.Title)
	}
}

// A confirm stays open until it is answered, and other commands say how to answer it.
func TestConfirmWaitsForAnswer(t *testing.T) {
	s, tab := newTestSession(t, map[string]string{"/": `
		<button onclick="document.title = confirm('Delete?') ? 'yes' : 'no'">Delete</button>`})
	s.Snapshot(tab)

	res, err := s.Click(tab, "e1")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(res.Dialog, "Delete?") {
		t.Errorf("click result dialog %q", res.Dialog)
	}
	if _, err := s.Text(tab, "", false); err == nil || !strings.Contains(err.Error(), "browse dialog accept") {
		t.Errorf("want a hint to answer the dialog, got %v", err)
	}
	res, err = s.Dialog(tab, true, "")
	if err != nil {
		t.Fatal(err)
	}
	if res.Title != "yes" {
		t.Errorf("title after accepting %q", res.Title)
	}
}

func TestAlertIsAcceptedAndReported(t *testing.T) {
	s, tab := newTestSession(t, map[string]string{"/": `
		<button onclick="alert('hi'); document.title = 'after'">Go</button>`})
	s.Snapshot(tab)

	res, err := s.Click(tab, "e1")
	if err != nil {
		t.Fatal(err)
	}
	if !slices.Equal(res.Alerts, []string{"hi"}) || res.Title != "after" {
		t.Errorf("alerts %v, title %q", res.Alerts, res.Title)
	}
}

func TestScrollDown(t *testing.T) {
	s, tab := newTestSession(t, map[string]string{"/": `<div style="height:5000px">long</div>`})

	if _, err := s.Scroll(tab, "down"); err != nil {
		t.Fatal(err)
	}
	if y := evalString(t, s, tab, "String(scrollY)"); y == "0" {
		t.Error("the page did not scroll")
	}
}

func TestUploadSetsFiles(t *testing.T) {
	s, tab := newTestSession(t, map[string]string{"/": `<input type="file" id="f">`})
	file := filepath.Join(t.TempDir(), "photo.png")
	os.WriteFile(file, []byte("x"), 0o644)
	s.Snapshot(tab)

	if _, err := s.Upload(tab, "e1", []string{file}); err != nil {
		t.Fatal(err)
	}
	if name := evalString(t, s, tab, "document.getElementById('f').files[0].name"); name != "photo.png" {
		t.Errorf("file name %q", name)
	}
}

func TestWaitForText(t *testing.T) {
	s, tab := newTestSession(t, map[string]string{"/": `
		<script>setTimeout(() => document.body.append('Ready'), 500)</script>`})

	if _, err := s.Wait(tab, "Ready"); err != nil {
		t.Fatal(err)
	}
}

func TestEvalAwaitsPromises(t *testing.T) {
	s, tab := newTestSession(t, map[string]string{"/": `<p>x</p>`})

	v, err := s.Eval(tab, "Promise.resolve(42)")
	if err != nil {
		t.Fatal(err)
	}
	if v != float64(42) {
		t.Errorf("got %v", v)
	}
}

func TestParseKeys(t *testing.T) {
	key, mods, err := parseKeys("Control+Shift+k")
	if err != nil || key != "k" || !slices.Equal(mods, []chromedp.Modifier{chromedp.ModifierCtrl, chromedp.ModifierShift}) {
		t.Errorf("Control+Shift+k: %q %v %v", key, mods, err)
	}
	if _, _, err := parseKeys("Banana"); err == nil {
		t.Error("want an error for an unknown key")
	}
}

// An action's result carries the page's uncaught errors and failed requests,
// so an agent sees why a page misbehaves; they are reported once.
func TestActionReportsPageErrors(t *testing.T) {
	s, tab := newTestSession(t, map[string]string{"/": `
		<button onclick="fetch('http://127.0.0.1:1/missing').catch(() => {}); setTimeout(() => { throw new Error('boom') }, 0)">Go</button>`})
	s.Snapshot(tab)

	res, err := s.Click(tab, "e1")
	if err != nil {
		t.Fatal(err)
	}
	joined := strings.Join(res.Errors, "\n")
	if !strings.Contains(joined, "boom") || !strings.Contains(joined, "127.0.0.1:1/missing") {
		t.Errorf("errors %q", res.Errors)
	}
	if again, _ := s.Reload(tab); len(again.Errors) != 0 {
		t.Errorf("errors reported twice: %q", again.Errors)
	}
}

// A screenshot with --refs shows each element's ref on it, and leaves the
// page as it was.
func TestScreenshotLabelsRefs(t *testing.T) {
	s, tab := newTestSession(t, map[string]string{"/": `<body style="margin:0;background:#fff"><button style="position:absolute;left:200px;top:200px">Go</button>`})
	s.Snapshot(tab)

	shot, err := s.Screenshot(tab, false, true)
	if err != nil {
		t.Fatal(err)
	}
	img := decodePNG(t, shot.PNG)
	orange := false
	for y := 170; y < 210 && !orange; y++ {
		for x := 190; x < 240; x++ {
			if r, g, b, _ := img.At(x, y).RGBA(); r>>8 > 230 && g>>8 > 90 && g>>8 < 140 && b>>8 < 60 {
				orange = true
				break
			}
		}
	}
	if !orange {
		t.Error("no orange label near the button")
	}
	if left := evalString(t, s, tab, "String(document.getElementById('__browse_labels'))"); left != "null" {
		t.Errorf("labels left on the page: %s", left)
	}
}
