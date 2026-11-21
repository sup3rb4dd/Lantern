// Lantern: a portable offline knowledge drive launcher.
//
// Lantern.exe sits in the root of the drive. It serves the home page,
// runs kiwix-serve for ZIM content, serves offline maps, manages
// downloads, and can answer questions over Meshtastic (Lantern Relay),
// all from the drive itself. Nothing is installed on the PC.
package main

import (
	"context"
	"fmt"
	"net"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"syscall"
	"time"
)

const version = "1.8.2"

// Paths holds every folder the launcher uses, all relative to the drive.
type Paths struct {
	Root   string // folder containing Lantern.exe
	Data   string // <root>/lantern
	Web    string // home page, map viewer, JS libraries
	Kiwix  string // kiwix-serve.exe and its files
	Zim    string // downloaded .zim content
	Maps   string // downloaded .pmtiles maps
	Assets string // map fonts and sprites
	Tools  string // helper tools (pmtiles.exe)
	Temp   string // in-progress downloads
}

func newPaths(root string) Paths {
	d := filepath.Join(root, "lantern")
	return Paths{
		Root:   root,
		Data:   d,
		Web:    filepath.Join(d, "web"),
		Kiwix:  filepath.Join(d, "kiwix"),
		Zim:    filepath.Join(d, "zim"),
		Maps:   filepath.Join(d, "maps"),
		Assets: filepath.Join(d, "maps-assets"),
		Tools:  filepath.Join(d, "tools"),
		Temp:   filepath.Join(d, "temp"),
	}
}

func exeDir() string {
	if v := os.Getenv("LANTERN_ROOT"); v != "" {
		return v
	}
	exe, err := os.Executable()
	if err != nil {
		return "."
	}
	if r, err := filepath.EvalSymlinks(exe); err == nil {
		exe = r
	}
	return filepath.Dir(exe)
}

// pickPort returns the preferred port if free, else any free local port.
func pickPort(preferred int) int {
	if preferred > 0 {
		if l, err := net.Listen("tcp", fmt.Sprintf("127.0.0.1:%d", preferred)); err == nil {
			l.Close()
			return preferred
		}
	}
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		return preferred
	}
	defer l.Close()
	return l.Addr().(*net.TCPAddr).Port
}

func main() {
	setConsoleTitle("Lantern")
	p := newPaths(exeDir())
	for _, d := range []string{p.Data, p.Kiwix, p.Zim, p.Maps, p.Assets, p.Tools, p.Temp} {
		os.MkdirAll(d, 0o755)
	}

	fmt.Println("==============================================")
	fmt.Println("  LANTERN  -  offline knowledge drive  v" + version)
	fmt.Println("==============================================")
	fmt.Println("Drive folder:", p.Root)

	if _, err := os.Stat(filepath.Join(p.Web, "index.html")); err != nil {
		fmt.Println()
		fmt.Println("ERROR: the 'lantern\\web' folder is missing next to Lantern.exe.")
		fmt.Println("Copy the whole Lantern folder to the drive, not just the .exe.")
		waitForEnter()
		os.Exit(1)
	}

	kx := newKiwix(p)
	kx.Start()

	dl := newDownloader(p, kx)

	rl := newRelay(p, kx)
	pdfCtx, stopPdf := context.WithCancel(context.Background())
	defer stopPdf()
	rl.pdf = newPdfLibrary(p, kx)
	go rl.pdf.Run(pdfCtx) // reads PDFs in book collections once, for Relay search
	dl.onDone = func(kind string) {
		if kind == "zim" || kind == "pdf-tools" {
			time.Sleep(5 * time.Second) // let kiwix-serve restart with the new file
			rl.pdf.Kick()
		}
	}
	rl.ai = newAI(p) // local AI for ?ask; starts only when a question comes in
	rl.Start()       // only connects if Lantern Relay is turned on in its settings

	port := pickPort(8484)
	addr := fmt.Sprintf("127.0.0.1:%d", port)
	url := "http://" + addr + "/"
	srv := &http.Server{
		Addr:              addr,
		Handler:           newServer(p, kx, dl, rl),
		ReadHeaderTimeout: 30 * time.Second,
	}

	go func() {
		if err := srv.ListenAndServe(); err != nil && err != http.ErrServerClosed {
			fmt.Println("ERROR starting web server:", err)
			waitForEnter()
			os.Exit(1)
		}
	}()

	time.Sleep(300 * time.Millisecond)
	fmt.Println()
	fmt.Println("Lantern is running at", url)
	fmt.Println("Your browser should open automatically.")
	fmt.Println("If it doesn't, open the address above in any browser.")
	fmt.Println()
	fmt.Println("Keep this window open while you use Lantern.")
	fmt.Println("Close this window (or press Ctrl+C) to stop.")
	fmt.Println()
	if os.Getenv("LANTERN_NO_BROWSER") == "" {
		openBrowser(url)
	}

	sig := make(chan os.Signal, 1)
	signal.Notify(sig, os.Interrupt, syscall.SIGTERM)
	<-sig
	fmt.Println("Shutting down...")
	dl.Shutdown()
	rl.Stop()
	rl.ai.Stop()
	kx.Stop()
	srv.Close()
}

func waitForEnter() {
	fmt.Println("Press Enter to close.")
	var b [1]byte
	os.Stdin.Read(b[:])
}
