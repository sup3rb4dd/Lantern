package main

import (
	"fmt"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"sort"
	"strings"
	"sync"
	"time"
)

// Kiwix runs kiwix-serve from the drive and restarts it when content changes.
type Kiwix struct {
	p    Paths
	mu   sync.Mutex
	cmd  *exec.Cmd
	port int
	err  string
}

func newKiwix(p Paths) *Kiwix { return &Kiwix{p: p} }

func (k *Kiwix) exePath() string {
	name := "kiwix-serve"
	if runtime.GOOS == "windows" {
		name += ".exe"
	}
	return filepath.Join(k.p.Kiwix, name)
}

// Installed reports whether kiwix-serve has been downloaded.
func (k *Kiwix) Installed() bool {
	_, err := os.Stat(k.exePath())
	return err == nil
}

// zimFiles lists finished .zim files on the drive.
func (k *Kiwix) zimFiles() []string {
	entries, _ := os.ReadDir(k.p.Zim)
	var out []string
	for _, e := range entries {
		if !e.IsDir() && strings.HasSuffix(strings.ToLower(e.Name()), ".zim") {
			out = append(out, filepath.Join(k.p.Zim, e.Name()))
		}
	}
	sort.Strings(out)
	return out
}

// Port returns kiwix-serve's local port, or 0 if it isn't running.
func (k *Kiwix) Port() int {
	k.mu.Lock()
	defer k.mu.Unlock()
	if k.cmd == nil {
		return 0
	}
	return k.port
}

func (k *Kiwix) LastError() string {
	k.mu.Lock()
	defer k.mu.Unlock()
	return k.err
}

// Start launches kiwix-serve if it is installed and there is content to serve.
func (k *Kiwix) Start() {
	k.mu.Lock()
	defer k.mu.Unlock()
	k.startLocked()
}

func (k *Kiwix) startLocked() {
	k.err = ""
	if !k.Installed() {
		fmt.Println("Kiwix server: not downloaded yet (use the Downloads page).")
		return
	}
	zims := k.zimFiles()
	if len(zims) == 0 {
		fmt.Println("Kiwix server: no content downloaded yet.")
		return
	}
	port := pickPort(8485)
	args := []string{
		"--address=127.0.0.1",
		fmt.Sprintf("--port=%d", port),
		"--urlRootLocation=/kiwix",
		"--nolibrarybutton",
	}
	args = append(args, zims...)
	cmd := exec.Command(k.exePath(), args...)
	cmd.Dir = k.p.Kiwix
	logPath := filepath.Join(k.p.Data, "kiwix.log")
	if lf, err := os.Create(logPath); err == nil {
		cmd.Stdout = lf
		cmd.Stderr = lf
	}
	hideWindow(cmd)
	if err := cmd.Start(); err != nil {
		k.err = "Could not start kiwix-serve: " + err.Error()
		fmt.Println(k.err)
		return
	}
	attachToJob(cmd)
	k.cmd = cmd
	k.port = port
	fmt.Printf("Kiwix server: serving %d content file(s).\n", len(zims))

	go func(c *exec.Cmd) {
		err := c.Wait()
		k.mu.Lock()
		if k.cmd == c {
			k.cmd = nil
			if err != nil {
				k.err = "kiwix-serve stopped: " + err.Error() + " (see lantern\\kiwix.log)"
				fmt.Println(k.err)
			}
		}
		k.mu.Unlock()
	}(cmd)

	// Wait briefly for it to accept connections.
	for i := 0; i < 40; i++ {
		c, err := net.DialTimeout("tcp", fmt.Sprintf("127.0.0.1:%d", port), 200*time.Millisecond)
		if err == nil {
			c.Close()
			return
		}
		time.Sleep(150 * time.Millisecond)
	}
}

func (k *Kiwix) stopLocked() {
	if k.cmd != nil && k.cmd.Process != nil {
		c := k.cmd
		k.cmd = nil
		c.Process.Kill()
	}
}

// Stop shuts kiwix-serve down.
func (k *Kiwix) Stop() {
	k.mu.Lock()
	defer k.mu.Unlock()
	k.stopLocked()
}

// Restart reloads kiwix-serve so newly downloaded content shows up.
func (k *Kiwix) Restart() {
	k.mu.Lock()
	defer k.mu.Unlock()
	k.stopLocked()
	time.Sleep(500 * time.Millisecond)
	k.startLocked()
}
