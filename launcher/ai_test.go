package main

import (
	"archive/zip"
	"bytes"
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func testPaths(t *testing.T) Paths {
	d := t.TempDir()
	p := Paths{Root: d, Data: filepath.Join(d, "lantern"), Tools: filepath.Join(d, "lantern", "tools"), Temp: filepath.Join(d, "lantern", "temp")}
	for _, x := range []string{p.Data, p.Tools, p.Temp} {
		os.MkdirAll(x, 0o755)
	}
	return p
}

// Setup downloads llama.cpp's Windows build and a model sized for the PC.
func TestAISetupDownloads(t *testing.T) {
	var zipBuf bytes.Buffer
	zw := zip.NewWriter(&zipBuf)
	for _, f := range []string{"llama-server.exe", "ggml-vulkan.dll", "ggml-cpu-haswell.dll", "llama-cli.exe", "LICENSE-curl"} {
		w, _ := zw.Create(f)
		w.Write([]byte("x " + f))
	}
	zw.Close()
	model := bytes.Repeat([]byte("g"), 2<<20)
	var srv *httptest.Server
	srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/release":
			fmt.Fprintf(w, `[{"tag_name":"b10000","assets":[{"name":"llama-b10000-bin-macos-arm64.zip","browser_download_url":"x"}]},
				{"tag_name":"b9999","assets":[
				{"name":"llama-b9999-bin-win-cpu-x64.zip","browser_download_url":"%[1]s/cpu.zip","size":5},
				{"name":"llama-b9999-bin-win-cuda-12.4-x64.zip","browser_download_url":"%[1]s/cuda.zip","size":5},
				{"name":"llama-b9999-bin-win-vulkan-x64.zip","browser_download_url":"%[1]s/vulkan.zip","size":%[2]d}]}]`, srv.URL, zipBuf.Len())
		case "/vulkan.zip":
			w.Write(zipBuf.Bytes())
		case "/api/models/Qwen/Qwen3-4B-GGUF", "/api/models/Qwen/Qwen3-1.7B-GGUF":
			w.Write([]byte(`{"siblings":[{"rfilename":"README.md"},{"rfilename":"Qwen3-X-Q8_0.gguf"},{"rfilename":"Qwen3-X-Q4_K_M.gguf"}]}`))
		case "/Qwen/Qwen3-4B-GGUF/resolve/main/Qwen3-X-Q4_K_M.gguf", "/Qwen/Qwen3-1.7B-GGUF/resolve/main/Qwen3-X-Q4_K_M.gguf":
			w.Write(model)
		default:
			t.Errorf("unexpected request %s", r.URL.Path)
			http.NotFound(w, r)
		}
	}))
	defer srv.Close()
	oldAPI, oldHF := llamaReleaseAPI, hfBase
	llamaReleaseAPI, hfBase = srv.URL+"/release", srv.URL
	defer func() { llamaReleaseAPI, hfBase = oldAPI, oldHF }()

	p := testPaths(t)
	d := &Downloader{p: p, client: http.DefaultClient}
	ai := newAIPaths(p)
	j := &Job{Kind: "ai"}
	if err := d.installLlama(context.Background(), j, ai); err != nil {
		t.Fatal(err)
	}
	for _, f := range []string{"llama-server.exe", "ggml-vulkan.dll", "ggml-cpu-haswell.dll", "LICENSE-curl"} {
		if !fileExists(filepath.Join(ai.llamaDir(), f)) {
			t.Errorf("%s not installed", f)
		}
	}
	if fileExists(filepath.Join(ai.llamaDir(), "llama-cli.exe")) {
		t.Error("extra tools should be left out")
	}
	if !strings.Contains(j.Title, "b9999") {
		t.Errorf("title %q", j.Title)
	}

	t.Setenv("LANTERN_LLAMA_SERVER", "/bin/true") // stands in for an installed server
	if err := d.runAI(context.Background(), j); err != nil {
		t.Fatal(err)
	}
	if got := filepath.Base(ai.modelPath()); got != "Qwen3-X-Q4_K_M.gguf" {
		t.Fatalf("model %q", got)
	}
	if !ai.Installed() {
		t.Fatal("not installed after setup")
	}
}

// Lantern starts llama-server itself, waits for it to load, and stops it again.
func TestAIStartsServer(t *testing.T) {
	py, err := exec.LookPath("python3")
	if err != nil {
		t.Skip("python3 not installed")
	}
	p := testPaths(t)
	os.MkdirAll(filepath.Join(p.Data, "models"), 0o755)
	os.WriteFile(filepath.Join(p.Data, "models", "m.gguf"), bytes.Repeat([]byte("g"), 2<<20), 0o644)
	script := filepath.Join(t.TempDir(), "llama-server")
	os.WriteFile(script, []byte(`#!/bin/sh
exec `+py+` -c '
import sys, json, http.server
port = int(sys.argv[sys.argv.index("--port")+1])
class H(http.server.BaseHTTPRequestHandler):
    def log_message(self, *a): pass
    def do_GET(self):
        self.send_response(200); self.end_headers(); self.wfile.write(b"{}")
    def do_POST(self):
        n = int(self.headers["Content-Length"]); body = json.loads(self.rfile.read(n))
        assert "-m" in sys.argv and "--jinja" in sys.argv
        out = {"choices":[{"message":{"content":"pong " + body["messages"][1]["content"]}}]}
        self.send_response(200); self.end_headers(); self.wfile.write(json.dumps(out).encode())
http.server.HTTPServer(("127.0.0.1", port), H).serve_forever()
' "$@"
`), 0o755)
	t.Setenv("LANTERN_LLAMA_SERVER", script)
	a := &aiEngine{p: p, state: "off", client: &http.Client{Timeout: 10 * time.Second}}
	got, err := a.Chat(context.Background(), "sys", "ping", nil, 10)
	if err != nil || got != "pong ping" {
		t.Fatalf("chat: %q %v", got, err)
	}
	if st := a.Status(); st.State != "ready" || st.Model != "m.gguf" {
		t.Fatalf("status %+v", st)
	}
	a.Stop()
	if st := a.Status(); st.State != "off" {
		t.Fatalf("after stop: %+v", st)
	}
}
