package main

// Color theme for Lantern's pages, matching FieldMap by default. The colors
// live in lantern/theme.json and reach every page through /theme.css.

import (
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"sync"
)

var themeKeys = []string{"accent", "text", "dim", "grid", "panel", "mesh", "warn", "bad"}

// themePresets are the built-in looks. "green" matches FieldMap.
var themePresets = map[string]map[string]string{
	"green":  {"accent": "#39FF14", "text": "#A8FFA0", "dim": "#2E8B2E", "grid": "#0E3A0E", "panel": "#020802"},
	"amber":  {"accent": "#FFB000", "text": "#FFD98A", "dim": "#A87400", "grid": "#3A2900", "panel": "#080500"},
	"red":    {"accent": "#FF3B30", "text": "#FF9A92", "dim": "#D9443A", "grid": "#3A0E0C", "panel": "#080202"},
	"arctic": {"accent": "#00E5FF", "text": "#A8F4FF", "dim": "#2A8C99", "grid": "#0B3A40", "panel": "#020708"},
}

var themeStatus = map[string]string{"mesh": "#00E5FF", "warn": "#FFB000", "bad": "#FF3B30"}

type themeConfig struct {
	Preset  string            `json:"preset"` // green | amber | red | arctic | custom
	Colors  map[string]string `json:"colors"`
	Network bool              `json:"network"` // use it on devices viewing over the network too
	Map     string            `json:"map"`     // dark | light
}

func defaultTheme() themeConfig {
	return themeConfig{Preset: "green", Colors: presetColors("green"), Network: true, Map: "dark"}
}

func presetColors(name string) map[string]string {
	c := map[string]string{}
	for k, v := range themePresets[name] {
		c[k] = v
	}
	for k, v := range themeStatus {
		c[k] = v
	}
	return c
}

var hexColor = regexp.MustCompile(`^#[0-9a-fA-F]{6}$`)

func (t *themeConfig) normalize() {
	d := defaultTheme()
	if _, ok := themePresets[t.Preset]; !ok && t.Preset != "custom" {
		t.Preset = d.Preset
	}
	if t.Colors == nil {
		t.Colors = map[string]string{}
	}
	base := presetColors("green")
	if t.Preset != "custom" {
		base = presetColors(t.Preset)
	}
	clean := map[string]string{}
	for _, k := range themeKeys {
		v := strings.ToUpper(t.Colors[k])
		if !hexColor.MatchString(v) {
			v = base[k]
		}
		clean[k] = v
	}
	t.Colors = clean
	if t.Map != "light" {
		t.Map = "dark"
	}
}

type themeStore struct {
	path string
	mu   sync.Mutex
	cfg  themeConfig
}

func newThemeStore(p Paths) *themeStore {
	s := &themeStore{path: filepath.Join(p.Data, "theme.json"), cfg: defaultTheme()}
	if b, err := os.ReadFile(s.path); err == nil {
		var c themeConfig
		c.Network = true
		if json.Unmarshal(b, &c) == nil {
			c.normalize()
			s.cfg = c
		}
	}
	return s
}

func (s *themeStore) Get() themeConfig {
	s.mu.Lock()
	defer s.mu.Unlock()
	c := s.cfg
	c.Colors = map[string]string{}
	for k, v := range s.cfg.Colors {
		c.Colors[k] = v
	}
	return c
}

func (s *themeStore) Set(c themeConfig) (themeConfig, error) {
	c.normalize()
	b, _ := json.MarshalIndent(c, "", "  ")
	if err := os.WriteFile(s.path, b, 0o644); err != nil {
		return c, err
	}
	s.mu.Lock()
	s.cfg = c
	s.mu.Unlock()
	return c, nil
}

// CSS returns the :root variables for a page. Visitors on other devices get
// the default look unless the theme is set to apply to them too.
func (s *themeStore) CSS(visitor bool) string {
	c := s.Get()
	if visitor && !c.Network {
		c = defaultTheme()
	}
	var b strings.Builder
	b.WriteString(":root{")
	keys := append([]string{}, themeKeys...)
	sort.Strings(keys)
	for _, k := range keys {
		fmt.Fprintf(&b, "--%s:%s;", k, c.Colors[k])
	}
	fmt.Fprintf(&b, "--map:%s;}", c.Map)
	return b.String()
}

func themeHandlers(mux *http.ServeMux, th *themeStore) {
	mux.HandleFunc("/theme.css", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/css; charset=utf-8")
		w.Header().Set("Cache-Control", "no-store")
		w.Write([]byte(th.CSS(r.Header.Get("X-Lantern-Visitor") == "1")))
	})
	mux.HandleFunc("/api/theme", func(w http.ResponseWriter, r *http.Request) {
		visitor := r.Header.Get("X-Lantern-Visitor") == "1"
		if r.Method == http.MethodPost {
			if visitor {
				http.Error(w, "forbidden", http.StatusForbidden)
				return
			}
			var c themeConfig
			if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 1<<12)).Decode(&c); err != nil {
				http.Error(w, "bad request", http.StatusBadRequest)
				return
			}
			if _, err := th.Set(c); err != nil {
				writeJSON(w, map[string]string{"error": err.Error()})
				return
			}
		}
		presets := map[string]map[string]string{}
		for name := range themePresets {
			presets[name] = presetColors(name)
		}
		writeJSON(w, map[string]any{"theme": th.Get(), "presets": presets, "visitor": visitor})
	})
}
