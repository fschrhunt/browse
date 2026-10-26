// browse is a small terminal browser for people and agents. Each command talks
// to a background daemon that owns one headless Chromium shell. `show` opens a
// pane to the right to display a tab.
package main

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"

	"golang.org/x/term"
)

// version is the release, such as v0.2.0, set by the release build.
var version = "dev"

const usage = `usage: browse [url]                 open the browser fullscreen in this terminal
       browse <command> [args] [--tab id] [--json]

In the browser, the mouse and keyboard go to the page. Ctrl+L edits the
address, Ctrl+T opens a tab, Ctrl+W closes one, Ctrl+PgUp and Ctrl+PgDn switch
tabs, Alt+Left and Alt+Right go back and forward, Ctrl+R reloads, Ctrl+C copies
what is selected to your clipboard, and Ctrl+Q leaves; the browser keeps
running for agents.

Pages
  open <url|file>        open a page in a new tab (x.com means https://x.com)
  goto <url|file>        load a page in the current tab
  back, forward, reload  move through the tab's history
  ls                     list tabs and which one is active
  switch <tab>           make a tab the active one
  close                  close the tab

Reading
  snapshot               list the page's elements with refs (e1, e2, ...)
  text [ref] [--all]     print the page's text, or one element's
  shot <file> [--full] [--refs]
                         save a screenshot; --full captures the whole page,
                         --refs labels the last snapshot's elements on it
  eval <js>              run JavaScript in the page and print the result

Acting (refs come from the last snapshot)
  click <ref>            click an element
  hover <ref>            move the mouse over an element
  fill <ref> <text>      replace the text in an input or editor
  type <text>            type where the focus is
  press <key>            press a key: Enter, Escape, Tab, ArrowDown, Control+a, ...
  select <ref> <option>  choose an option of a select by its text or value
  scroll [where]         down (default), up, top, bottom, or a ref
  upload <ref> <file>... set the files of a file input
  wait <text|duration>   wait for text to appear, or for a time like 2s
  dialog accept [text]   answer the open confirm or prompt; text answers a prompt
  dialog dismiss

Display
  show                   show the active tab in a pane to the right
                         (in tmux, kitty, or WezTerm)
  view                   show the active tab in this terminal

  shutdown               stop the browser
  skill [folder]         add the agent skill to every agent found, then ask
                         for other agents' skills folders; or add it to folder
  version                print browse's version

Actions wait for the page to settle, then print where the browser ended up:
the tab, its title and URL, and any tab the page opened, alert it showed,
dialog it left open, or error it hit: uncaught JavaScript errors and failed
requests since the last action. --tab picks a tab from ls; without it, commands use the
active tab. --json prints the daemon's result as JSON.

Logins persist in ~/.local/state/browse/profile, and downloads go to
~/Downloads. The browser is chrome-headless-shell: scripts/install.sh installs
the pinned build, or set BROWSE_CHROME to another one, or put it on PATH.
scripts/uninstall.sh removes browse and everything it made.`

// shellBuild is Chrome for Testing's name for this system's headless shell
// build, as in chrome-headless-shell-mac-arm64. scripts/install.sh uses the
// same names.
func shellBuild() string {
	switch runtime.GOOS + "/" + runtime.GOARCH {
	case "darwin/arm64":
		return "mac-arm64"
	case "darwin/amd64":
		return "mac-x64"
	}
	return "linux64"
}

// findChrome returns the headless shell to launch: $BROWSE_CHROME, then PATH,
// then the copy scripts/install.sh installs.
func findChrome() (string, error) {
	if p := os.Getenv("BROWSE_CHROME"); p != "" {
		return p, nil
	}
	if p, err := exec.LookPath("chrome-headless-shell"); err == nil {
		return p, nil
	}
	dir, err := xdgDir("XDG_DATA_HOME", ".local/share")
	if err != nil {
		return "", err
	}
	installed := filepath.Join(dir, "chrome-headless-shell-"+shellBuild(), "chrome-headless-shell")
	if _, err := os.Stat(installed); err == nil {
		return installed, nil
	}
	return "", errors.New("no chrome-headless-shell found; run scripts/install.sh")
}

// flags are the options that any command accepts, anywhere on the line.
type flags struct {
	tab  string
	json bool
	full bool
	all  bool
	refs bool
}

// parseArgs pulls the flags out of argv. The rest are positional.
func parseArgs(argv []string) ([]string, flags, error) {
	var args []string
	var f flags
	for i := 0; i < len(argv); i++ {
		switch argv[i] {
		case "--tab":
			if i+1 == len(argv) {
				return nil, f, errors.New("--tab needs a tab id")
			}
			i++
			f.tab = argv[i]
		case "--json":
			f.json = true
		case "--full":
			f.full = true
		case "--all":
			f.all = true
		case "--refs":
			f.refs = true
		default:
			args = append(args, argv[i])
		}
	}
	return args, f, nil
}

// shellQuote wraps s in single quotes for a POSIX shell.
func shellQuote(s string) string {
	return "'" + strings.ReplaceAll(s, "'", `'\''`) + "'"
}

// show opens a pane to the right of the caller's and runs the viewer in it,
// using the terminal's own way to split: tmux, kitty, or WezTerm. Other
// terminals, Ghostty on Linux among them, cannot be split from outside.
func show() error {
	exe, err := os.Executable()
	if err != nil {
		return err
	}
	var cmd *exec.Cmd
	switch {
	case os.Getenv("TMUX") != "":
		args := []string{"split-window", "-h"}
		if pane := os.Getenv("TMUX_PANE"); pane != "" {
			args = append(args, "-t", pane)
		}
		cmd = exec.Command("tmux", append(args, shellQuote(exe)+" view")...)
	case os.Getenv("KITTY_WINDOW_ID") != "":
		// Needs allow_remote_control in kitty's config.
		cmd = exec.Command("kitten", "@", "launch", "--location=vsplit", "--cwd=current", "--", exe, "view")
	case os.Getenv("WEZTERM_PANE") != "":
		cmd = exec.Command("wezterm", "cli", "split-pane", "--right", "--", exe, "view")
	default:
		return errors.New("this terminal cannot be split from outside; run browse show inside tmux, kitty, or WezTerm, or split a pane yourself and run browse view")
	}
	if out, err := cmd.CombinedOutput(); err != nil {
		return fmt.Errorf("splitting the pane: %v: %s", err, strings.TrimSpace(string(out)))
	}
	return nil
}

// looksLikeURL reports whether a first argument is an address to open rather
// than a command.
func looksLikeURL(arg string) bool {
	return strings.Contains(arg, ".") || strings.Contains(arg, ":") || strings.HasPrefix(arg, "localhost")
}

// fullscreen opens the browser in this terminal on url in a new tab, or
// without a url on the start screen of a blank tab, reusing the active tab
// when it is already blank.
func fullscreen(url string) error {
	if url == "" {
		raw, err := callDaemon(Request{Cmd: "ls", Person: true})
		if err != nil {
			return err
		}
		tabs, err := decode[[]TabInfo](raw)
		if err != nil {
			return err
		}
		for _, t := range tabs {
			if t.Active && t.URL == "about:blank" {
				return runViewer()
			}
		}
		url = "about:blank"
	}
	if _, err := callDaemon(Request{Cmd: "open", URL: url, Person: true}); err != nil {
		return err
	}
	return runViewer()
}

func decode[T any](raw json.RawMessage) (T, error) {
	var v T
	err := json.Unmarshal(raw, &v)
	return v, err
}

func printJSON(raw json.RawMessage) error {
	var out bytes.Buffer
	if err := json.Indent(&out, raw, "", "  "); err != nil {
		return err
	}
	fmt.Println(out.String())
	return nil
}

// printer prints a daemon result for people. --json prints the raw result instead.
type printer func(raw json.RawMessage) error

func quiet(json.RawMessage) error { return nil }

// printPlace prints where an action left the browser.
func printPlace(raw json.RawMessage) error {
	t, err := decode[TabInfo](raw)
	if err != nil {
		return err
	}
	fmt.Printf("%s  %s  %s\n", t.Tab, t.Title, t.URL)
	for _, id := range t.Opened {
		fmt.Printf("opened %s, now the active tab\n", id)
	}
	for _, msg := range t.Alerts {
		fmt.Printf("alert: %q\n", msg)
	}
	for _, msg := range t.Errors {
		fmt.Println(msg)
	}
	if t.Dialog != "" {
		fmt.Printf("dialog open: %s; answer with browse dialog accept or browse dialog dismiss\n", t.Dialog)
	}
	return nil
}

func printTabs(raw json.RawMessage) error {
	tabs, err := decode[[]TabInfo](raw)
	if err != nil {
		return err
	}
	for _, t := range tabs {
		mark := " "
		if t.Active {
			mark = "*"
		}
		fmt.Printf("%s %s  %s  %s\n", mark, t.Tab, t.Title, t.URL)
		if t.Dialog != "" {
			fmt.Printf("    dialog open: %s\n", t.Dialog)
		}
	}
	return nil
}

// elementLine is one snapshot element as a line, such as
// @e3 textbox "Email" value="a@b.c" offscreen.
func elementLine(el Element) string {
	var b strings.Builder
	fmt.Fprintf(&b, "@%s %s %q", el.Ref, el.Role, el.Name)
	if el.Value != "" {
		fmt.Fprintf(&b, " value=%q", el.Value)
	}
	if el.Checked != nil {
		fmt.Fprintf(&b, " checked=%t", *el.Checked)
	}
	if len(el.Options) > 0 {
		fmt.Fprintf(&b, " options=%q", el.Options)
	}
	if el.Disabled {
		b.WriteString(" disabled")
	}
	if el.Offscreen {
		b.WriteString(" offscreen")
	}
	return b.String()
}

func printSnapshot(raw json.RawMessage) error {
	snap, err := decode[Snapshot](raw)
	if err != nil {
		return err
	}
	fmt.Printf("%s\n%s\n", snap.Title, snap.URL)
	for _, el := range snap.Elements {
		fmt.Println(elementLine(el))
	}
	if snap.More > 0 {
		fmt.Printf("... %d more elements not listed\n", snap.More)
	}
	return nil
}

func printText(raw json.RawMessage) error {
	text, err := decode[Text](raw)
	if err != nil {
		return err
	}
	fmt.Println(text.Text)
	if text.Total > len([]rune(text.Text)) {
		fmt.Printf("[cut at %d of %d characters; use --all for everything]\n", len([]rune(text.Text)), text.Total)
	}
	return nil
}

// saveShot returns a printer that writes the screenshot to path.
func saveShot(path string) printer {
	return func(raw json.RawMessage) error {
		shot, err := decode[Shot](raw)
		if err != nil {
			return err
		}
		if err := os.WriteFile(path, shot.PNG, 0o644); err != nil {
			return err
		}
		fmt.Println("saved", path)
		return nil
	}
}

// command builds the daemon request for a CLI command and the printer for its
// result. The error reports a missing argument.
func command(name string, args []string, f flags) (Request, printer, error) {
	arg := func(i int) string {
		if i < len(args) {
			return args[i]
		}
		return ""
	}
	need := func(n int, what string) error {
		if len(args) < n {
			return fmt.Errorf("%s needs %s", name, what)
		}
		return nil
	}
	ref := strings.TrimPrefix(arg(0), "@")
	tab := f.tab

	switch name {
	case "open":
		return Request{Cmd: "open", URL: arg(0)}, printPlace, need(1, "a url")
	case "goto":
		return Request{Cmd: "goto", Tab: tab, URL: arg(0)}, printPlace, need(1, "a url")
	case "back", "forward", "reload":
		return Request{Cmd: name, Tab: tab}, printPlace, nil
	case "ls":
		return Request{Cmd: "ls"}, printTabs, nil
	case "switch":
		return Request{Cmd: "switch", Tab: arg(0)}, printPlace, need(1, "a tab")
	case "close":
		return Request{Cmd: "close", Tab: tab}, quiet, nil
	case "snapshot":
		return Request{Cmd: "snapshot", Tab: tab}, printSnapshot, nil
	case "text":
		return Request{Cmd: "text", Tab: tab, Ref: ref, All: f.all}, printText, nil
	case "shot":
		return Request{Cmd: "screenshot", Tab: tab, Full: f.full, Refs: f.refs}, saveShot(arg(0)), need(1, "a file")
	case "eval":
		return Request{Cmd: "eval", Tab: tab, JS: arg(0)}, printJSON, need(1, "javascript")
	case "click", "hover":
		return Request{Cmd: name, Tab: tab, Ref: ref}, printPlace, need(1, "a ref")
	case "fill":
		return Request{Cmd: "fill", Tab: tab, Ref: ref, Text: arg(1)}, printPlace, need(2, "a ref and text")
	case "type":
		return Request{Cmd: "type", Tab: tab, Text: arg(0)}, printPlace, need(1, "text")
	case "press":
		return Request{Cmd: "press", Tab: tab, Text: arg(0)}, printPlace, need(1, "a key")
	case "select":
		return Request{Cmd: "select", Tab: tab, Ref: ref, Text: arg(1)}, printPlace, need(2, "a ref and an option")
	case "scroll":
		return Request{Cmd: "scroll", Tab: tab, Text: arg(0)}, printPlace, nil
	case "upload":
		var files []string
		if len(args) > 1 {
			files = args[1:]
		}
		return Request{Cmd: "upload", Tab: tab, Ref: ref, Files: files}, printPlace, need(2, "a ref and a file")
	case "wait":
		return Request{Cmd: "wait", Tab: tab, Text: arg(0)}, printPlace, need(1, "text or a duration")
	case "dialog":
		if arg(0) != "accept" && arg(0) != "dismiss" {
			return Request{}, nil, errors.New("dialog needs accept or dismiss")
		}
		return Request{Cmd: "dialog", Tab: tab, Accept: arg(0) == "accept", Text: arg(1)}, printPlace, nil
	case "shutdown":
		return Request{Cmd: "shutdown"}, quiet, nil
	}
	return Request{}, nil, fmt.Errorf("unknown command %s; run browse help", name)
}

func run(argv []string) error {
	args, f, err := parseArgs(argv)
	if err != nil {
		return err
	}
	interactive := term.IsTerminal(int(os.Stdin.Fd())) && term.IsTerminal(int(os.Stdout.Fd()))
	if len(args) == 0 {
		if interactive {
			return fullscreen("")
		}
		fmt.Println(usage)
		return nil
	}
	if interactive && len(args) == 1 && looksLikeURL(args[0]) {
		return fullscreen(args[0])
	}

	switch args[0] {
	case "help", "-h", "--help":
		fmt.Println(usage)
		return nil

	case "version", "--version":
		fmt.Println("browse", version)
		return nil

	case "skill":
		if len(args) > 2 {
			return errors.New("usage: browse skill [folder]")
		}
		return installSkill(strings.Join(args[1:], ""))

	case "serve":
		chrome, err := findChrome()
		if err != nil {
			return err
		}
		dir, err := stateDir()
		if err != nil {
			return err
		}
		return serveDaemon(chrome, filepath.Join(dir, "profile"))

	case "show", "view":
		// Viewers show the active tab, so --tab picks it first.
		if f.tab != "" {
			if _, err := callDaemon(Request{Cmd: "switch", Tab: f.tab}); err != nil {
				return err
			}
		}
		if args[0] == "show" {
			return show()
		}
		return runViewer()
	}

	req, printResult, err := command(args[0], args[1:], f)
	if err != nil {
		return err
	}
	raw, err := callDaemon(req)
	if err != nil {
		return err
	}
	if f.json {
		return printJSON(raw)
	}
	return printResult(raw)
}

func main() {
	if err := run(os.Args[1:]); err != nil {
		fmt.Fprintln(os.Stderr, "browse:", err)
		os.Exit(1)
	}
}
