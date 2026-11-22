package main

// "Share on my network": lets phones, tablets and other computers on the same
// WiFi open Lantern in a browser. Off by default. Network visitors can read
// everything and use the Relay "Try it" box, but only the Lantern PC itself
// can change settings, download or delete. An optional PIN keeps strangers on
// the same WiFi out.

import (
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"html"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"
)

const sharePort = 8488 // 8484 is Lantern on this PC, 8485 is kiwix-serve

type shareConfig struct {
	Enabled bool   `json:"enabled"`
	PIN     string `json:"pin"`
}

type sharer struct {
	path    string
	handler http.Handler // the same pages and API the PC uses
	mu      sync.Mutex
	cfg     shareConfig
	srv     *http.Server
	port    int
	err     string
	salt    string
}

func newSharer(p Paths) *sharer {
	s := &sharer{path: filepath.Join(p.Data, "share.json")}
	b := make([]byte, 16)
	rand.Read(b)
	s.salt = hex.EncodeToString(b)
	if data, err := os.ReadFile(s.path); err == nil {
		json.Unmarshal(data, &s.cfg)
	}
	return s
}

// Start opens the network address if sharing is turned on.
func (s *sharer) Start() {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.applyLocked()
}

func (s *sharer) applyLocked() {
	if !s.cfg.Enabled {
		if s.srv != nil {
			s.srv.Close()
			s.srv = nil
		}
		s.err = ""
		return
	}
	if s.srv != nil {
		return
	}
	l, err := net.Listen("tcp", fmt.Sprintf(":%d", sharePort))
	if err != nil {
		s.err = fmt.Sprintf("couldn't open port %d: %v", sharePort, err)
		return
	}
	s.port, s.err = sharePort, ""
	s.srv = &http.Server{Handler: s.guard(s.handler), ReadHeaderTimeout: 30 * time.Second}
	go s.srv.Serve(l)
	fmt.Println("Sharing on your network:", strings.Join(s.urls(), "  "))
}

func (s *sharer) Stop() {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.srv != nil {
		s.srv.Close()
		s.srv = nil
	}
}

func (s *sharer) Set(c shareConfig) error {
	c.PIN = strings.TrimSpace(c.PIN)
	if len(c.PIN) > 32 {
		c.PIN = c.PIN[:32]
	}
	b, _ := json.MarshalIndent(c, "", "  ")
	if err := os.WriteFile(s.path, b, 0o644); err != nil {
		return err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	pinChanged := c.PIN != s.cfg.PIN
	s.cfg = c
	if pinChanged {
		b := make([]byte, 16)
		rand.Read(b)
		s.salt = hex.EncodeToString(b) // everyone signs in again
	}
	s.applyLocked()
	return nil
}

// urls lists the addresses other devices can use.
func (s *sharer) urls() []string {
	out := []string{}
	ifaces, _ := net.Interfaces()
	for _, ifc := range ifaces {
		if ifc.Flags&net.FlagUp == 0 || ifc.Flags&net.FlagLoopback != 0 {
			continue
		}
		addrs, _ := ifc.Addrs()
		for _, a := range addrs {
			ipn, ok := a.(*net.IPNet)
			if !ok || ipn.IP.To4() == nil || ipn.IP.IsLinkLocalUnicast() {
				continue
			}
			out = append(out, fmt.Sprintf("http://%s:%d/", ipn.IP, sharePort))
		}
	}
	sort.Strings(out)
	return out
}

type shareStatus struct {
	Enabled bool     `json:"enabled"`
	Running bool     `json:"running"`
	HasPIN  bool     `json:"hasPin"`
	PIN     string   `json:"pin,omitempty"` // only shown on the Lantern PC
	URLs    []string `json:"urls"`
	Error   string   `json:"error,omitempty"`
	Visitor bool     `json:"visitor"` // this page was opened from another device
}

func (s *sharer) Status(visitor bool) shareStatus {
	s.mu.Lock()
	defer s.mu.Unlock()
	st := shareStatus{Enabled: s.cfg.Enabled, Running: s.srv != nil, HasPIN: s.cfg.PIN != "",
		Error: s.err, Visitor: visitor, URLs: []string{}}
	if !visitor {
		st.PIN = s.cfg.PIN
	}
	if s.srv != nil {
		st.URLs = s.urls()
	}
	return st
}

func (s *sharer) token() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	h := sha256.Sum256([]byte(s.salt + "|" + s.cfg.PIN))
	return hex.EncodeToString(h[:])
}

// visitorPosts are the only changes network visitors may make.
var visitorPosts = map[string]bool{"/api/relay/try": true}

// guard wraps the pages for visitors from other devices.
func (s *sharer) guard(h http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		r.Header.Set("X-Lantern-Visitor", "1")
		s.mu.Lock()
		pin := s.cfg.PIN
		s.mu.Unlock()

		if pin != "" {
			if r.URL.Path == "/lantern-pin" && r.Method == http.MethodPost {
				r.ParseForm()
				if subtle.ConstantTimeCompare([]byte(r.PostFormValue("pin")), []byte(pin)) == 1 {
					http.SetCookie(w, &http.Cookie{Name: "lantern_pin", Value: s.token(), Path: "/",
						HttpOnly: true, SameSite: http.SameSiteStrictMode, MaxAge: 60 * 60 * 24 * 365})
					next := r.PostFormValue("next")
					if !strings.HasPrefix(next, "/") || strings.HasPrefix(next, "//") {
						next = "/"
					}
					http.Redirect(w, r, next, http.StatusSeeOther)
					return
				}
				time.Sleep(time.Second) // slow down guessing
				pinPage(w, r.PostFormValue("next"), true)
				return
			}
			c, err := r.Cookie("lantern_pin")
			if err != nil || subtle.ConstantTimeCompare([]byte(c.Value), []byte(s.token())) != 1 {
				if strings.HasPrefix(r.URL.Path, "/api/") {
					http.Error(w, `{"error":"enter the PIN first"}`, http.StatusUnauthorized)
					return
				}
				pinPage(w, r.URL.RequestURI(), false)
				return
			}
		}

		if r.Method != http.MethodGet && r.Method != http.MethodHead {
			if !visitorPosts[r.URL.Path] {
				w.Header().Set("Content-Type", "application/json")
				w.WriteHeader(http.StatusForbidden)
				w.Write([]byte(`{"error":"Changes can only be made on the Lantern computer itself."}`))
				return
			}
			if o := r.Header.Get("Origin"); o != "" && o != "http://"+r.Host {
				http.Error(w, "forbidden", http.StatusForbidden)
				return
			}
			if !strings.HasPrefix(r.Header.Get("Content-Type"), "application/json") {
				http.Error(w, "expected JSON", http.StatusUnsupportedMediaType)
				return
			}
		}
		h.ServeHTTP(w, r)
	})
}

func pinPage(w http.ResponseWriter, next string, wrong bool) {
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.Header().Set("Cache-Control", "no-store")
	msg := ""
	if wrong {
		msg = `<p style="color:#ef6b6b">That PIN didn't match.</p>`
	}
	fmt.Fprintf(w, `<!doctype html><meta charset="utf-8"><meta name="viewport" content="width=device-width, initial-scale=1">
<title>Lantern</title>
<body style="font-family:system-ui;background:#0E1114;color:#E6E9ED;padding:48px 24px;max-width:420px;margin:auto">
<h1 style="color:#F5B947">Lantern</h1><p>Enter the PIN shown on the Lantern computer.</p>%s
<form method="post" action="/lantern-pin">
<input type="hidden" name="next" value="%s">
<input name="pin" type="password" inputmode="numeric" autofocus autocomplete="off"
 style="font-size:20px;padding:12px;width:100%%;box-sizing:border-box;border-radius:8px;border:1px solid #333;background:#161b20;color:#E6E9ED">
<button style="margin-top:14px;font-size:18px;padding:12px 20px;border-radius:8px;border:0;background:#F5B947;color:#111">Open Lantern</button>
</form>`, msg, html.EscapeString(next))
}
