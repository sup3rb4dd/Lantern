package main

import (
	"io"
	"net/http"
	"net/http/cookiejar"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestShareGuard(t *testing.T) {
	p := testPaths(t)
	os.MkdirAll(p.Web, 0o755)
	os.WriteFile(filepath.Join(p.Web, "index.html"), []byte("<h1>home</h1>"), 0o644)
	sh := newSharer(p)
	local := newServer(p, newKiwix(p), newDownloader(p, nil), newRelay(p, nil), sh, newThemeStore(p))
	visitor := httptest.NewServer(sh.guard(sh.handler))
	defer visitor.Close()
	lsrv := httptest.NewServer(local)
	defer lsrv.Close()

	get := func(c *http.Client, u string) (int, string) {
		resp, err := c.Get(u)
		if err != nil {
			t.Fatal(err)
		}
		defer resp.Body.Close()
		b, _ := io.ReadAll(resp.Body)
		return resp.StatusCode, string(b)
	}
	post := func(c *http.Client, u, body string) int {
		resp, err := c.Post(u, "application/json", strings.NewReader(body))
		if err != nil {
			t.Fatal(err)
		}
		resp.Body.Close()
		return resp.StatusCode
	}
	c := &http.Client{}

	// No PIN: visitors can read pages, can't change anything.
	if code, body := get(c, visitor.URL+"/"); code != 200 || !strings.Contains(body, "home") {
		t.Fatalf("visitor home: %d %q", code, body)
	}
	if _, body := get(c, visitor.URL+"/api/share"); !strings.Contains(body, `"visitor":true`) || strings.Contains(body, `"pin"`) {
		t.Fatalf("visitor share status: %s", body)
	}
	if code := post(c, visitor.URL+"/api/relay/config", `{"enabled":true}`); code != 403 {
		t.Fatalf("visitor changed relay config: %d", code)
	}
	if code := post(c, visitor.URL+"/api/share", `{"enabled":false}`); code != 403 {
		t.Fatalf("visitor changed sharing: %d", code)
	}
	if code := post(c, visitor.URL+"/api/relay/try", `{"text":"?help"}`); code != 200 {
		t.Fatalf("visitor try box: %d", code)
	}

	// The PC sets a PIN (local requests can't pretend to be visitors or vice versa).
	req, _ := http.NewRequest("POST", lsrv.URL+"/api/share", strings.NewReader(`{"enabled":false,"pin":"4321"}`))
	req.Host = "127.0.0.1"
	req.Header.Set("Content-Type", "application/json")
	resp, err := http.DefaultClient.Do(req)
	if err != nil || resp.StatusCode != 200 {
		t.Fatalf("local set pin: %v %v", err, resp)
	}
	resp.Body.Close()

	if code, body := get(c, visitor.URL+"/relay.html"); code != 200 || !strings.Contains(body, "Enter the PIN") {
		t.Fatalf("PIN page: %d %q", code, body)
	}
	if code, _ := get(c, visitor.URL+"/api/status"); code != 401 {
		t.Fatalf("api without PIN: %d", code)
	}
	jar, _ := cookiejar.New(nil)
	cj := &http.Client{Jar: jar}
	r2, _ := cj.PostForm(visitor.URL+"/lantern-pin", url.Values{"pin": {"9999"}, "next": {"/"}})
	b, _ := io.ReadAll(r2.Body)
	r2.Body.Close()
	if !strings.Contains(string(b), "didn't match") {
		t.Fatalf("wrong pin accepted: %s", b)
	}
	r3, _ := cj.PostForm(visitor.URL+"/lantern-pin", url.Values{"pin": {"4321"}, "next": {"//evil.example/"}})
	b, _ = io.ReadAll(r3.Body)
	r3.Body.Close()
	if !strings.Contains(string(b), "home") {
		t.Fatalf("right pin didn't open Lantern: %s", b)
	}
	if code, body := get(cj, visitor.URL+"/"); code != 200 || !strings.Contains(body, "home") {
		t.Fatalf("after PIN: %d %q", code, body)
	}
}

func TestTheme(t *testing.T) {
	p := testPaths(t)
	th := newThemeStore(p)
	if css := th.CSS(false); !strings.Contains(css, "--accent:#39FF14;") || !strings.Contains(css, "--map:dark;") {
		t.Fatalf("default css: %s", css)
	}
	c, _ := th.Set(themeConfig{Preset: "custom", Colors: map[string]string{"accent": "#ff0000", "dim": "red;}body{x"}, Network: false})
	if c.Colors["accent"] != "#FF0000" || c.Colors["dim"] != "#2E8B2E" || c.Colors["mesh"] != "#00E5FF" {
		t.Fatalf("normalize: %+v", c.Colors)
	}
	if !strings.Contains(th.CSS(false), "--accent:#FF0000;") || !strings.Contains(th.CSS(true), "--accent:#39FF14;") {
		t.Fatal("visitor should get the default when network is off")
	}
	if got := newThemeStore(p).Get(); got.Colors["accent"] != "#FF0000" || got.Network {
		t.Fatalf("not saved: %+v", got)
	}
}
