// The daemon owns the browser session and serves it on a unix socket. A client
// sends one JSON request per connection and reads one JSON response back.
// callDaemon starts the daemon in the background when none is listening, and
// again when the running one is from before an update. A lock file keeps it to
// one daemon per profile.
package main

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"sync"
	"syscall"
	"time"
)

// Request is one command. Only the fields a command uses are set. Text is the
// command's text argument: what to fill or type, the keys to press, the option
// to select, where to scroll, what to wait for, or a prompt's answer.
type Request struct {
	Cmd    string   `json:"cmd"`
	Tab    string   `json:"tab,omitempty"`
	URL    string   `json:"url,omitempty"`
	Ref    string   `json:"ref,omitempty"`
	Text   string   `json:"text,omitempty"`
	JS     string   `json:"js,omitempty"`
	Files  []string `json:"files,omitempty"`
	Accept bool     `json:"accept,omitempty"`
	Full   bool     `json:"full,omitempty"` // shot: the whole page, not just the viewport
	Refs   bool     `json:"refs,omitempty"` // shot: label the elements with their refs
	All    bool     `json:"all,omitempty"`  // text: no length limit
	Step   int      `json:"step,omitempty"` // cycle: +1 for the next tab, -1 for the previous
	View   *View    `json:"view,omitempty"` // stream: the viewer's page area
	Input  *Input   `json:"input,omitempty"`
	// Person marks requests a person makes, from the viewer or by typing
	// browse. Others come from agents, and the viewer's bar shows when one acts.
	Person bool `json:"person,omitempty"`
}

// Response is either a result or an error.
type Response struct {
	OK     bool            `json:"ok"`
	Error  string          `json:"error,omitempty"`
	Result json.RawMessage `json:"result,omitempty"`
}

// errUpdated is the daemon's answer once browse has been updated under it.
var errUpdated = errors.New("browse was updated; the browser is restarting")

// watchExecutable returns a func that reports whether the browse binary this
// process started from has since been replaced, as installing an update does.
func watchExecutable() func() bool {
	exe, err := os.Executable()
	if err != nil {
		return func() bool { return false }
	}
	first, err := os.Stat(exe)
	if err != nil {
		return func() bool { return false }
	}
	return func() bool {
		now, err := os.Stat(exe)
		return err == nil && (!now.ModTime().Equal(first.ModTime()) || now.Size() != first.Size())
	}
}

// errAlreadyRunning means another daemon is serving, so this one is not needed.
var errAlreadyRunning = errors.New("a browse daemon is already running")

// socketPath is where the daemon listens: $BROWSE_SOCKET, else daemon.sock
// in the state directory. That directory is the same from every terminal,
// SSH session and tmux pane, where temp directories are not: macOS gives SSH
// sessions a different one than the desktop.
func socketPath() string {
	if p := os.Getenv("BROWSE_SOCKET"); p != "" {
		return p
	}
	dir, _ := stateDir()
	return filepath.Join(dir, "daemon.sock")
}

// xdgDir returns $env, or ~/fallback when it is unset, joined with "browse".
func xdgDir(env, fallback string) (string, error) {
	base := os.Getenv(env)
	if base == "" {
		home, err := os.UserHomeDir()
		if err != nil {
			return "", err
		}
		base = filepath.Join(home, fallback)
	}
	return filepath.Join(base, "browse"), nil
}

// stateDir holds the profile and the daemon log. It is private to the user,
// since anyone who can read the profile can use its logins.
func stateDir() (string, error) {
	dir, err := xdgDir("XDG_STATE_HOME", ".local/state")
	if err != nil {
		return "", err
	}
	return dir, os.MkdirAll(dir, 0o700)
}

// lockDaemon takes the lock that allows one daemon, and so one Chrome, per
// profile. If another daemon is listening on sock it returns
// errAlreadyRunning. A daemon that is exiting still holds the lock for a
// moment, so this retries for up to five seconds.
func lockDaemon(profile, sock string) (*os.File, error) {
	f, err := os.OpenFile(profile+".lock", os.O_CREATE|os.O_RDWR, 0o600)
	if err != nil {
		return nil, err
	}
	for range 50 {
		if syscall.Flock(int(f.Fd()), syscall.LOCK_EX|syscall.LOCK_NB) == nil {
			return f, nil
		}
		if daemonListening(sock) {
			f.Close()
			return nil, errAlreadyRunning
		}
		time.Sleep(100 * time.Millisecond)
	}
	f.Close()
	return nil, fmt.Errorf("another browse daemon is using %s", profile)
}

// serveDaemon runs the session until a shutdown request has been answered.
// Requests are handled concurrently; the session serializes the commands.
func serveDaemon(chromePath, profile string) error {
	sock := socketPath()
	lock, err := lockDaemon(profile, sock)
	if errors.Is(err, errAlreadyRunning) {
		return nil
	}
	if err != nil {
		return err
	}
	defer lock.Close()

	session, err := NewSession(chromePath, profile)
	if err != nil {
		return err
	}
	// Holding the lock means any socket file left here is stale.
	os.Remove(sock)
	ln, err := net.Listen("unix", sock)
	if err != nil {
		session.Shutdown()
		return err
	}
	defer os.Remove(sock)

	updated := watchExecutable()
	done := make(chan struct{})
	var stop sync.Once
	for {
		conn, err := ln.Accept()
		if err != nil {
			select {
			case <-done:
				return nil
			default:
				return err
			}
		}
		go func() {
			if handle(session, conn, updated) {
				stop.Do(func() {
					close(done)
					ln.Close()
				})
			}
		}()
	}
}

// handle serves one connection and reports whether the daemon should stop.
// A stream request keeps the connection: frames go to the viewer one per
// line, and the viewer's inputs come back the same way, until it hangs up.
// Once browse has been updated, every request is answered with errUpdated
// and the daemon shuts down, so the caller can start the new one.
func handle(s *Session, conn net.Conn, updated func() bool) bool {
	defer conn.Close()
	reader := bufio.NewReader(conn)
	line, err := reader.ReadBytes('\n')
	if err != nil {
		return false
	}
	var req Request
	if err := json.Unmarshal(line, &req); err != nil {
		writeResponse(conn, nil, err)
		return false
	}
	if req.Cmd != "shutdown" && updated() {
		writeResponse(conn, nil, errUpdated)
		s.Shutdown()
		return true
	}
	if req.Cmd == "stream" && req.View != nil {
		ctx, cancel := context.WithCancel(context.Background())
		inputs := make(chan Input, 64)
		go func() {
			defer cancel()
			for {
				line, err := reader.ReadBytes('\n')
				if err != nil {
					return // the viewer hung up
				}
				var in Input
				if json.Unmarshal(line, &in) == nil {
					inputs <- in
				}
			}
		}()
		enc := json.NewEncoder(conn)
		s.Stream(ctx, *req.View, func(f Frame) error { return enc.Encode(f) }, inputs)
		cancel()
		return false
	}
	result, err := dispatch(s, req)
	if !req.Person && req.Cmd != "ls" && req.Cmd != "shutdown" {
		// Mark the tab the command acted on, which for open is the new tab.
		tab := req.Tab
		if info, ok := result.(TabInfo); ok {
			tab = info.Tab
		}
		s.markAgent(tab)
	}
	writeResponse(conn, result, err)
	return req.Cmd == "shutdown" && err == nil
}

// writeResponse sends the result or error. A client that has gone away gets nothing.
func writeResponse(conn net.Conn, result any, err error) {
	resp := Response{OK: true}
	if err == nil {
		resp.Result, err = json.Marshal(result)
	}
	if err != nil {
		resp = Response{Error: err.Error()}
	}
	json.NewEncoder(conn).Encode(resp)
}

func dispatch(s *Session, req Request) (any, error) {
	switch req.Cmd {
	case "open":
		return s.Open(req.URL)
	case "goto":
		return s.Goto(req.Tab, req.URL)
	case "back":
		return s.Back(req.Tab)
	case "forward":
		return s.Forward(req.Tab)
	case "reload":
		return s.Reload(req.Tab)
	case "stop":
		return s.Stop(req.Tab)
	case "ls":
		return s.List()
	case "snapshot":
		return s.Snapshot(req.Tab)
	case "click":
		return s.Click(req.Tab, req.Ref)
	case "hover":
		return s.Hover(req.Tab, req.Ref)
	case "fill":
		return s.Fill(req.Tab, req.Ref, req.Text)
	case "type":
		return s.Type(req.Tab, req.Text)
	case "press":
		return s.Press(req.Tab, req.Text)
	case "select":
		return s.Select(req.Tab, req.Ref, req.Text)
	case "scroll":
		return s.Scroll(req.Tab, req.Text)
	case "upload":
		return s.Upload(req.Tab, req.Ref, req.Files)
	case "wait":
		return s.Wait(req.Tab, req.Text)
	case "dialog":
		return s.Dialog(req.Tab, req.Accept, req.Text)
	case "text":
		return s.Text(req.Tab, req.Ref, req.All)
	case "eval":
		return s.Eval(req.Tab, req.JS)
	case "screenshot":
		return s.Screenshot(req.Tab, req.Full, req.Refs)
	case "close":
		return s.Close(req.Tab)
	case "cycle":
		return s.Cycle(req.Step)
	case "switch":
		return s.Activate(req.Tab)
	case "input":
		if req.Input == nil {
			return nil, errors.New("input needs an event")
		}
		return nil, s.SendInput(*req.Input)
	case "shutdown":
		// Stop Chrome before replying, so the reply means the profile is saved.
		return nil, s.Shutdown()
	}
	return nil, fmt.Errorf("unknown command %s", req.Cmd)
}

// callDaemon sends one request and returns the raw result, starting the daemon
// first when nothing is listening, and starting it anew when the running one
// is from before an update.
func callDaemon(req Request) (json.RawMessage, error) {
	sock := socketPath()
	if !daemonListening(sock) {
		if req.Cmd == "shutdown" {
			return json.RawMessage("null"), nil
		}
		if err := startDaemon(sock); err != nil {
			return nil, err
		}
	}
	raw, err := send(sock, req)
	if err == nil || err.Error() != errUpdated.Error() {
		return raw, err
	}
	// The old daemon is saving the logins and stopping; start the new one after it.
	fmt.Fprintln(os.Stderr, "browse was updated; restarting the browser, which keeps your logins...")
	for i := 0; i < 450 && daemonListening(sock); i++ {
		time.Sleep(100 * time.Millisecond)
	}
	if err := startDaemon(sock); err != nil {
		return nil, err
	}
	return send(sock, req)
}

// send sends one request to a running daemon and never starts one. After a
// shutdown it waits for the daemon to stop listening, so the next command can
// start a fresh one.
func send(sock string, req Request) (json.RawMessage, error) {
	resp, err := roundTrip(sock, req)
	if err != nil {
		return nil, err
	}
	if !resp.OK {
		return nil, errors.New(resp.Error)
	}
	if req.Cmd == "shutdown" {
		for i := 0; i < 50 && daemonListening(sock); i++ {
			time.Sleep(100 * time.Millisecond)
		}
	}
	return resp.Result, nil
}

// daemonListening reports whether a daemon accepts connections. It only dials,
// so it never sends a request.
func daemonListening(sock string) bool {
	conn, err := net.Dial("unix", sock)
	if err != nil {
		return false
	}
	conn.Close()
	return true
}

func roundTrip(sock string, req Request) (Response, error) {
	var resp Response
	conn, err := net.Dial("unix", sock)
	if err != nil {
		return resp, err
	}
	defer conn.Close()
	if err := json.NewEncoder(conn).Encode(req); err != nil {
		return resp, err
	}
	err = json.NewDecoder(conn).Decode(&resp)
	return resp, err
}

// startDaemon launches a detached daemon, logging to daemon.log in the state
// directory, and waits until it listens. If the daemon exits instead, the error
// carries the last line it logged.
func startDaemon(sock string) error {
	if _, err := findChrome(); err != nil {
		return err
	}
	exe, err := os.Executable()
	if err != nil {
		return err
	}
	dir, err := stateDir()
	if err != nil {
		return err
	}
	logPath := filepath.Join(dir, "daemon.log")
	logFile, err := os.Create(logPath)
	if err != nil {
		return err
	}
	defer logFile.Close()

	cmd := exec.Command(exe, "serve")
	cmd.Stdout, cmd.Stderr = logFile, logFile
	cmd.SysProcAttr = &syscall.SysProcAttr{Setsid: true}
	if err := cmd.Start(); err != nil {
		return err
	}
	exited := make(chan struct{})
	go func() {
		cmd.Wait()
		close(exited)
	}()

	for range 100 {
		if daemonListening(sock) {
			return nil
		}
		select {
		case <-exited:
			// A daemon that found another one running exits quietly; use that one.
			if daemonListening(sock) {
				return nil
			}
			return fmt.Errorf("browse daemon exited: %s", lastLine(logPath))
		case <-time.After(100 * time.Millisecond):
		}
	}
	return fmt.Errorf("browse daemon did not start; see %s", logPath)
}

// lastLine returns the last non-empty line of a file, for error messages.
func lastLine(path string) string {
	data, _ := os.ReadFile(path)
	lines := bytes.Split(bytes.TrimSpace(data), []byte("\n"))
	if last := lines[len(lines)-1]; len(last) > 0 {
		return string(last)
	}
	return "no output in " + path
}
