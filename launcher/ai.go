package main

// Local AI for Lantern Relay. Everything runs on this PC from the Lantern
// folder: llama.cpp's llama-server (lantern/tools/llama) and one small model
// file (lantern/models/*.gguf). Nothing is installed into Windows, and the
// server only runs while questions are coming in.

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"runtime"
	"sort"
	"strings"
	"sync"
	"time"
)

const (
	aiIdleStop  = 15 * time.Minute // free the memory when nobody has asked for a while
	aiCtxTokens = 4096
)

type aiEngine struct {
	p       Paths
	mu      sync.Mutex
	cmd     *exec.Cmd
	base    string // http://127.0.0.1:port of our llama-server
	model   string // model file the server was started with
	state   string // off | starting | ready | error
	err     string
	last    time.Time
	busy    sync.Mutex // the model answers one question at a time
	client  *http.Client
	url     string // LANTERN_AI_URL: use an AI server that's already running instead
	cpuOnly bool   // the graphics card didn't work; use the processor only
}

func newAI(p Paths) *aiEngine {
	a := &aiEngine{p: p, state: "off", client: &http.Client{Timeout: 5 * time.Minute},
		url: strings.TrimRight(os.Getenv("LANTERN_AI_URL"), "/")}
	go a.idleLoop()
	return a
}

func (a *aiEngine) llamaDir() string  { return filepath.Join(a.p.Tools, "llama") }
func (a *aiEngine) modelsDir() string { return filepath.Join(a.p.Data, "models") }

// serverPath finds llama-server: Lantern's own copy, an override, or (off Windows) one on PATH.
func (a *aiEngine) serverPath() string {
	if v := os.Getenv("LANTERN_LLAMA_SERVER"); v != "" {
		return v
	}
	name := "llama-server"
	if runtime.GOOS == "windows" {
		name += ".exe"
	}
	if p := filepath.Join(a.llamaDir(), name); fileExists(p) {
		return p
	}
	if runtime.GOOS != "windows" {
		if p, err := exec.LookPath(name); err == nil {
			return p
		}
	}
	return ""
}

// modelPath is the newest .gguf file in lantern/models.
func (a *aiEngine) modelPath() string {
	files, _ := filepath.Glob(filepath.Join(a.modelsDir(), "*.gguf"))
	best, bestT := "", time.Time{}
	for _, f := range files {
		if st, err := os.Stat(f); err == nil && st.Size() > 1<<20 && st.ModTime().After(bestT) {
			best, bestT = f, st.ModTime()
		}
	}
	return best
}

// Installed reports whether the AI can be used at all.
func (a *aiEngine) Installed() bool {
	return a != nil && (a.url != "" || (a.serverPath() != "" && a.modelPath() != ""))
}

type aiStatus struct {
	Installed  bool   `json:"installed"`
	HasServer  bool   `json:"hasServer"`
	Model      string `json:"model"`
	ModelBytes int64  `json:"modelBytes"`
	State      string `json:"state"`
	Error      string `json:"error,omitempty"`
	RAMTotal   uint64 `json:"ramTotal"`
	RAMFree    uint64 `json:"ramFree"`
	Pick       string `json:"pick"`      // model Setup would download for this PC
	TooSmall   bool   `json:"tooSmall"`  // not enough memory for any model
	External   string `json:"external"`  // LANTERN_AI_URL, if used
	PickBytes  int64  `json:"pickBytes"` // rough download size
	ServerName string `json:"serverName"`
}

func (a *aiEngine) Status() aiStatus {
	total, free := memoryInfo()
	pick := pickAIModel(total)
	st := aiStatus{HasServer: a.serverPath() != "", RAMTotal: total, RAMFree: free, External: a.url}
	if pick != nil {
		st.Pick, st.PickBytes = pick.Label, pick.Bytes
	} else {
		st.TooSmall = true
	}
	if m := a.modelPath(); m != "" {
		st.Model = filepath.Base(m)
		if fi, err := os.Stat(m); err == nil {
			st.ModelBytes = fi.Size()
		}
	}
	st.Installed = a.Installed()
	a.mu.Lock()
	st.State, st.Error = a.state, a.err
	a.mu.Unlock()
	return st
}

// ---------- running llama-server ----------

// ensure starts llama-server if needed and waits until it has loaded the model.
// Callers hold a.busy, so only one start happens at a time.
func (a *aiEngine) ensure(ctx context.Context) (string, error) {
	if a.url != "" {
		return a.url, nil
	}
	a.mu.Lock()
	a.last = time.Now()
	if a.cmd != nil && a.state == "ready" {
		base := a.base
		a.mu.Unlock()
		return base, nil
	}
	a.mu.Unlock()
	fail := func(msg string) (string, error) {
		a.mu.Lock()
		a.stopLocked()
		a.state, a.err = "error", msg
		a.mu.Unlock()
		return "", errors.New(msg)
	}
	exe, model := a.serverPath(), a.modelPath()
	if exe == "" || model == "" {
		return "", errors.New("AI isn't set up on this PC")
	}
	if fi, err := os.Stat(model); err == nil {
		if _, free := memoryInfo(); free > 0 && free < uint64(fi.Size())+600<<20 {
			return fail("not enough free memory to load the AI right now")
		}
	}
	carryVCRuntime(filepath.Dir(exe))
	port := pickPort(0)
	args := []string{"-m", model, "--host", "127.0.0.1", "--port", fmt.Sprint(port),
		"-c", fmt.Sprint(aiCtxTokens), "--jinja", "--no-webui", "-np", "1"}
	if a.cpuOnly {
		args = append(args, "--device", "none")
	}
	cmd := exec.Command(exe, args...)
	cmd.Dir = filepath.Dir(exe)
	if lf, err := os.Create(filepath.Join(a.p.Data, "ai.log")); err == nil {
		cmd.Stdout, cmd.Stderr = lf, lf
	}
	hideWindow(cmd)
	if err := cmd.Start(); err != nil {
		return fail("couldn't start the AI: " + err.Error())
	}
	attachToJob(cmd)
	exited := make(chan struct{})
	go func() { cmd.Wait(); close(exited) }()
	base := fmt.Sprintf("http://127.0.0.1:%d", port)
	a.mu.Lock()
	a.cmd, a.base, a.model, a.state, a.err = cmd, base, model, "starting", ""
	a.mu.Unlock()

	// Loading a model from a thumb drive can take a minute or two.
	deadline := time.Now().Add(4 * time.Minute)
	for time.Now().Before(deadline) {
		select {
		case <-exited:
			if cmd.ProcessState != nil && uint32(cmd.ProcessState.ExitCode()) == 0xC0000135 {
				return fail("this PC is missing a Windows file the AI needs (Microsoft Visual C++ runtime). Install \"VC_redist.x64.exe\" from microsoft.com once, then Lantern carries the files on the drive")
			}
			if !a.cpuOnly {
				// Usually a graphics driver problem: try again on the processor alone.
				a.cpuOnly = true
				fmt.Println("AI: stopped while loading, retrying without the graphics card")
				return a.ensure(ctx)
			}
			return fail("the AI stopped while loading (see lantern\\ai.log)")
		case <-ctx.Done():
			a.Stop()
			return "", ctx.Err()
		case <-time.After(500 * time.Millisecond):
		}
		resp, err := a.client.Get(base + "/health")
		if err == nil {
			resp.Body.Close()
			if resp.StatusCode == 200 {
				a.mu.Lock()
				a.state, a.last = "ready", time.Now()
				a.mu.Unlock()
				fmt.Println("AI ready:", filepath.Base(model))
				return base, nil
			}
		}
	}
	return fail("the AI took too long to load")
}

func (a *aiEngine) stopLocked() {
	if a.cmd != nil && a.cmd.Process != nil {
		a.cmd.Process.Kill()
	}
	a.cmd = nil
	if a.state != "error" {
		a.state = "off"
	}
}

func (a *aiEngine) Stop() {
	a.mu.Lock()
	a.stopLocked()
	a.mu.Unlock()
}

func (a *aiEngine) idleLoop() {
	for range time.Tick(time.Minute) {
		if !a.busy.TryLock() {
			continue
		}
		a.mu.Lock()
		if a.cmd != nil && time.Since(a.last) > aiIdleStop {
			a.stopLocked()
		}
		a.mu.Unlock()
		a.busy.Unlock()
	}
}

// ---------- asking ----------

var thinkRe = regexp.MustCompile(`(?s)<think>.*?</think>`)

// Chat sends one system+user prompt. With a schema the reply is JSON matching it.
func (a *aiEngine) Chat(ctx context.Context, system, user string, schema any, maxTokens int) (string, error) {
	a.busy.Lock()
	defer a.busy.Unlock()
	base, err := a.ensure(ctx)
	if err != nil {
		return "", err
	}
	body := map[string]any{
		"messages": []map[string]string{
			{"role": "system", "content": system},
			{"role": "user", "content": user},
		},
		"temperature":          0.2,
		"max_tokens":           maxTokens,
		"stream":               false,
		"chat_template_kwargs": map[string]any{"enable_thinking": false},
	}
	if schema != nil {
		body["response_format"] = map[string]any{"type": "json_schema",
			"json_schema": map[string]any{"name": "reply", "strict": true, "schema": schema}}
	}
	b, _ := json.Marshal(body)
	req, _ := http.NewRequestWithContext(ctx, "POST", base+"/v1/chat/completions", bytes.NewReader(b))
	req.Header.Set("Content-Type", "application/json")
	resp, err := a.client.Do(req)
	a.mu.Lock()
	a.last = time.Now()
	a.mu.Unlock()
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()
	rb, _ := io.ReadAll(io.LimitReader(resp.Body, 4<<20))
	if resp.StatusCode != 200 {
		return "", fmt.Errorf("AI server said %s: %s", resp.Status, truncBytes(string(rb), 200))
	}
	var out struct {
		Choices []struct {
			Message struct {
				Content string `json:"content"`
			} `json:"message"`
		} `json:"choices"`
	}
	if err := json.Unmarshal(rb, &out); err != nil || len(out.Choices) == 0 {
		return "", errors.New("AI gave an empty reply")
	}
	txt := thinkRe.ReplaceAllString(out.Choices[0].Message.Content, "")
	return strings.TrimSpace(txt), nil
}

// ChatJSON is Chat with a schema, decoded into v.
func (a *aiEngine) ChatJSON(ctx context.Context, system, user string, schema any, maxTokens int, v any) error {
	txt, err := a.Chat(ctx, system, user, schema, maxTokens)
	if err != nil {
		return err
	}
	// Some models wrap JSON in a code fence even when asked not to.
	if i := strings.Index(txt, "{"); i > 0 {
		txt = txt[i:]
	}
	if j := strings.LastIndex(txt, "}"); j >= 0 {
		txt = txt[:j+1]
	}
	if err := json.Unmarshal([]byte(txt), v); err != nil {
		return fmt.Errorf("AI reply wasn't readable: %v", err)
	}
	return nil
}

// ---------- which model to download ----------

type aiModelChoice struct {
	Label   string
	Repo    string   // Hugging Face repo
	Prefer  []string // quantizations to look for, best first
	Bytes   int64    // rough size, for the page
	MinRAM  uint64   // installed RAM needed
	Default string   // file name if the repo listing can't be read
}

// Official Qwen3 releases (Apache 2.0, no sign-in needed). Bigger PCs get the 4B model.
var aiModels = []aiModelChoice{
	{Label: "Qwen3 4B", Repo: "Qwen/Qwen3-4B-GGUF", Prefer: []string{"Q4_K_M", "Q5_K_M", "Q8_0"},
		Bytes: 2500 << 20, MinRAM: 7 << 30, Default: "Qwen3-4B-Q4_K_M.gguf"},
	{Label: "Qwen3 1.7B (small PC)", Repo: "Qwen/Qwen3-1.7B-GGUF", Prefer: []string{"Q4_K_M", "Q8_0"},
		Bytes: 1830 << 20, MinRAM: 3500 << 20, Default: "Qwen3-1.7B-Q8_0.gguf"},
}

func pickAIModel(totalRAM uint64) *aiModelChoice {
	if totalRAM == 0 {
		return &aiModels[0]
	}
	for i := range aiModels {
		if totalRAM >= aiModels[i].MinRAM {
			return &aiModels[i]
		}
	}
	return nil
}

var (
	llamaReleaseAPI = envOr("LANTERN_LLAMA_API", "https://api.github.com/repos/ggml-org/llama.cpp/releases/latest")
	hfBase          = strings.TrimRight(envOr("LANTERN_HF", "https://huggingface.co"), "/")
)

// runAI downloads llama-server and a model sized for this PC.
func (d *Downloader) runAI(ctx context.Context, j *Job) error {
	ai := newAIPaths(d.p)
	if ai.serverPath() == "" {
		if runtime.GOOS != "windows" {
			return errors.New("install llama.cpp (llama-server) from your system's packages")
		}
		if err := d.installLlama(ctx, j, ai); err != nil {
			return err
		}
	}
	if ai.modelPath() != "" {
		d.setStep(j, "Already installed")
		return nil
	}
	total, _ := memoryInfo()
	m := pickAIModel(total)
	if m == nil {
		return fmt.Errorf("this PC has %.1f GB of memory; the AI needs at least 4 GB", float64(total)/(1<<30))
	}
	d.setStep(j, "Finding the "+m.Label+" model")
	file := m.Default
	if body, err := d.getBytes(ctx, hfBase+"/api/models/"+m.Repo); err == nil {
		var info struct {
			Siblings []struct {
				Name string `json:"rfilename"`
			} `json:"siblings"`
		}
		if json.Unmarshal(body, &info) == nil {
		found:
			for _, q := range m.Prefer {
				for _, s := range info.Siblings {
					if strings.HasSuffix(s.Name, ".gguf") && strings.Contains(strings.ToUpper(s.Name), q) && !strings.Contains(s.Name, "/") {
						file = s.Name
						break found
					}
				}
			}
		}
	}
	j.Title = "AI for Lantern Relay (" + m.Label + ")"
	os.MkdirAll(ai.modelsDir(), 0o755)
	return d.downloadFile(ctx, j, hfBase+"/"+m.Repo+"/resolve/main/"+file, filepath.Join(ai.modelsDir(), file), 0)
}

func newAIPaths(p Paths) *aiEngine { return &aiEngine{p: p} }

var llamaZipRe = regexp.MustCompile(`^llama-(b[0-9]+)-bin-win-(vulkan|cpu)-x64\.zip$`)

func (d *Downloader) installLlama(ctx context.Context, j *Job, ai *aiEngine) error {
	d.setStep(j, "Finding the latest llama.cpp")
	body, err := d.getBytes(ctx, llamaReleaseAPI)
	if err != nil {
		return fmt.Errorf("couldn't look up llama.cpp: %w", err)
	}
	var rel struct {
		Tag    string `json:"tag_name"`
		Assets []struct {
			Name string `json:"name"`
			URL  string `json:"browser_download_url"`
			Size int64  `json:"size"`
		} `json:"assets"`
	}
	if err := json.Unmarshal(body, &rel); err != nil {
		return errors.New("couldn't read the llama.cpp release list")
	}
	// Vulkan uses almost any graphics card when there is one and falls back to the CPU.
	sort.SliceStable(rel.Assets, func(x, y int) bool {
		return strings.Contains(rel.Assets[x].Name, "vulkan") && !strings.Contains(rel.Assets[y].Name, "vulkan")
	})
	var url, name string
	var size int64
	for _, as := range rel.Assets {
		if llamaZipRe.MatchString(as.Name) {
			url, name, size = as.URL, as.Name, as.Size
			break
		}
	}
	if url == "" {
		return errors.New("no Windows build in the latest llama.cpp release")
	}
	j.Title = "AI engine (llama.cpp " + rel.Tag + ")"
	zipPath := filepath.Join(d.p.Temp, name)
	if err := d.downloadFile(ctx, j, url, zipPath, size); err != nil {
		return err
	}
	d.setStep(j, "Unpacking")
	tmp := filepath.Join(d.p.Temp, "llama")
	os.RemoveAll(tmp)
	err = unzipFlatten(zipPath, tmp, func(p string) bool {
		base := strings.ToLower(path_base(p))
		return !strings.Contains(p, "/") && (base == "llama-server.exe" || strings.HasSuffix(base, ".dll") ||
			strings.HasPrefix(base, "license"))
	})
	os.Remove(zipPath)
	if err != nil {
		return fmt.Errorf("unpacking llama.cpp: %w", err)
	}
	if !fileExists(filepath.Join(tmp, "llama-server.exe")) {
		os.RemoveAll(tmp)
		return errors.New("llama-server.exe wasn't found in the download")
	}
	os.MkdirAll(d.p.Tools, 0o755)
	os.RemoveAll(ai.llamaDir())
	if err := os.Rename(tmp, ai.llamaDir()); err != nil {
		return err
	}
	carryVCRuntime(ai.llamaDir())
	return nil
}

// carryVCRuntime copies Microsoft's C++ runtime files next to llama-server
// when this PC has them, so the drive also works on PCs that don't.
// (Microsoft allows these files to be shipped alongside a program.)
func carryVCRuntime(dir string) {
	if runtime.GOOS != "windows" {
		return
	}
	sys := filepath.Join(envOr("SystemRoot", `C:\Windows`), "System32")
	for _, f := range []string{"vcruntime140.dll", "vcruntime140_1.dll", "msvcp140.dll", "msvcp140_1.dll", "msvcp140_2.dll"} {
		dst := filepath.Join(dir, f)
		if fileExists(dst) {
			continue
		}
		if b, err := os.ReadFile(filepath.Join(sys, f)); err == nil {
			os.WriteFile(dst, b, 0o644)
		}
	}
}

func path_base(p string) string {
	if i := strings.LastIndexAny(p, `/\`); i >= 0 {
		return p[i+1:]
	}
	return p
}
