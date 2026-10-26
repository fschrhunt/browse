package main

import (
	"errors"
	"net"
	"path/filepath"
	"testing"
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
