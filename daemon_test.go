package main

import (
	"errors"
	"net"
	"os"
	"path/filepath"
	"testing"
	"time"
)

// A daemon started while another one is listening steps aside instead of
// taking over the socket.
func TestSecondDaemonStepsAside(t *testing.T) {
	dir := t.TempDir()
	profile, sock := filepath.Join(dir, "profile"), filepath.Join(dir, "browse.sock")
	lock, err := lockDaemon(profile, sock)
	if err != nil {
		t.Fatal(err)
	}
	defer lock.Close()
	ln, err := net.Listen("unix", sock)
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()

	if _, err := lockDaemon(profile, sock); !errors.Is(err, errAlreadyRunning) {
		t.Errorf("want errAlreadyRunning, got %v", err)
	}
}

// A daemon notices when the browse binary it started from is replaced.
func TestWatchExecutableNoticesUpdate(t *testing.T) {
	exe, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	updated := watchExecutable()
	if updated() {
		t.Fatal("updated before anything changed")
	}
	info, _ := os.Stat(exe)
	defer os.Chtimes(exe, info.ModTime(), info.ModTime())
	later := info.ModTime().Add(time.Minute)
	if err := os.Chtimes(exe, later, later); err != nil {
		t.Fatal(err)
	}
	if !updated() {
		t.Error("did not notice the replaced binary")
	}
}
