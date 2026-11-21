package main

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httputil"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"time"
)

func newServer(p Paths, kx *Kiwix, dl *Downloader, rl *Relay) http.Handler {
	mux := http.NewServeMux()

	// Lantern Relay: Meshtastic gateway status and settings.
	mux.HandleFunc("/api/relay/status", func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, rl.Status())
	})
	mux.HandleFunc("/api/relay/test", func(w http.ResponseWriter, r *http.Request) {
		if !postOnly(w, r) {
			return
		}
		var body struct{ Channel int }
		json.NewDecoder(http.MaxBytesReader(w, r.Body, 1<<12)).Decode(&body)
		if err := rl.SendTest(body.Channel); err != nil {
			writeJSON(w, map[string]any{"error": err.Error()})
			return
		}
		writeJSON(w, map[string]any{"ok": true})
	})
	mux.HandleFunc("/api/relay/config", func(w http.ResponseWriter, r *http.Request) {
		if !postOnly(w, r) {
			return
		}
		var c relayConfig
		if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 1<<16)).Decode(&c); err != nil {
			writeJSON(w, map[string]any{"error": "bad request"})
			return
		}
		if err := rl.SetConfig(c); err != nil {
			writeJSON(w, map[string]any{"error": err.Error()})
			return
		}
		writeJSON(w, map[string]any{"ok": true, "config": rl.Config()})
	})

	// Kiwix content (Wikipedia, books, guides) is proxied so everything lives on one address.
	mux.HandleFunc("/kiwix/", func(w http.ResponseWriter, r *http.Request) {
		port := kx.Port()
		if port == 0 {
			w.Header().Set("Content-Type", "text/html; charset=utf-8")
			w.WriteHeader(http.StatusServiceUnavailable)
			msg := "Nothing has been downloaded yet."
			if e := kx.LastError(); e != "" {
				msg = e
			}
			fmt.Fprintf(w, `<!doctype html><meta charset="utf-8"><title>Not available</title>
<body style="font-family:system-ui;background:#0E1114;color:#E6E9ED;padding:48px">
<h1>This content isn't available yet</h1><p>%s</p>
<p><a style="color:#F5B947" href="/downloads.html">Open Downloads</a> · <a style="color:#F5B947" href="/">Home</a></p>`,
				htmlEscape(msg))
			return
		}
		target, _ := url.Parse(fmt.Sprintf("http://127.0.0.1:%d", port))
		proxy := httputil.NewSingleHostReverseProxy(target)
		proxy.ErrorHandler = func(w http.ResponseWriter, r *http.Request, err error) {
			http.Error(w, "The Kiwix server isn't responding yet. Wait a few seconds and refresh.", http.StatusBadGateway)
		}
		proxy.ServeHTTP(w, r)
	})

	mux.HandleFunc("/api/status", func(w http.ResponseWriter, r *http.Request) {
		free, total := freeSpace(p.Root)
		writeJSON(w, map[string]any{
			"version":        version,
			"kiwixInstalled": kx.Installed(),
			"kiwixRunning":   kx.Port() != 0,
			"kiwixError":     kx.LastError(),
			"zims":           listFiles(p.Zim, ".zim"),
			"maps":           listFiles(p.Maps, ".pmtiles"),
			"mapRegions":     mapRegions,
			"mapAssets":      fileExists(filepath.Join(p.Assets, "fonts", "Noto Sans Regular", "0-255.pbf")),
			"diskFree":       free,
			"diskTotal":      total,
			"drive":          p.Root,
		})
	})

	mux.HandleFunc("/api/catalog", func(w http.ResponseWriter, r *http.Request) {
		q := url.Values{}
		in := r.URL.Query()
		if v := in.Get("name"); v != "" {
			ctx, cancel := context.WithTimeout(r.Context(), 45*time.Second)
			defer cancel()
			e, err := dl.LookupBook(ctx, v)
			if err != nil {
				writeJSON(w, map[string]any{"error": "Couldn't reach the Kiwix library. Check your internet connection. (" + err.Error() + ")"})
				return
			}
			res := []CatalogEntry{}
			if e != nil {
				res = append(res, *e)
			}
			writeJSON(w, map[string]any{"results": res})
			return
		} else {
			if v := strings.TrimSpace(in.Get("q")); v != "" {
				q.Set("q", v)
			}
			lang := in.Get("lang")
			if lang == "" {
				lang = "eng"
			}
			if lang != "all" {
				q.Set("lang", lang)
			}
			q.Set("count", "100")
		}
		ctx, cancel := context.WithTimeout(r.Context(), 45*time.Second)
		defer cancel()
		res, err := dl.Catalog(ctx, q)
		if err != nil {
			writeJSON(w, map[string]any{"error": "Couldn't reach the Kiwix library. Check your internet connection. (" + err.Error() + ")"})
			return
		}
		writeJSON(w, map[string]any{"results": res})
	})

	mux.HandleFunc("/api/jobs", func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, map[string]any{"jobs": dl.Jobs()})
	})

	mux.HandleFunc("/api/download", func(w http.ResponseWriter, r *http.Request) {
		if !postOnly(w, r) {
			return
		}
		var j Job
		if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 1<<16)).Decode(&j); err != nil {
			writeJSON(w, map[string]any{"error": "bad request"})
			return
		}
		job, err := dl.Enqueue(Job{Kind: j.Kind, Name: j.Name, URL: j.URL, Title: j.Title,
			Region: j.Region, BBox: strings.ReplaceAll(j.BBox, " ", ""), MaxZoom: j.MaxZoom})
		if err != nil {
			writeJSON(w, map[string]any{"error": err.Error()})
			return
		}
		writeJSON(w, map[string]any{"ok": true, "id": job.ID})
	})

	mux.HandleFunc("/api/cancel", func(w http.ResponseWriter, r *http.Request) {
		if !postOnly(w, r) {
			return
		}
		var body struct{ ID int }
		json.NewDecoder(http.MaxBytesReader(w, r.Body, 1<<12)).Decode(&body)
		dl.Cancel(body.ID)
		writeJSON(w, map[string]any{"ok": true})
	})

	mux.HandleFunc("/api/clear", func(w http.ResponseWriter, r *http.Request) {
		if !postOnly(w, r) {
			return
		}
		dl.ClearFinished()
		writeJSON(w, map[string]any{"ok": true})
	})

	safeName := regexp.MustCompile(`^[A-Za-z0-9._-]+\.(zim|pmtiles)$`)
	mux.HandleFunc("/api/delete", func(w http.ResponseWriter, r *http.Request) {
		if !postOnly(w, r) {
			return
		}
		var body struct{ File string }
		json.NewDecoder(http.MaxBytesReader(w, r.Body, 1<<12)).Decode(&body)
		if !safeName.MatchString(body.File) {
			writeJSON(w, map[string]any{"error": "bad file name"})
			return
		}
		dir := p.Zim
		if strings.HasSuffix(body.File, ".pmtiles") {
			dir = p.Maps
		}
		isZim := dir == p.Zim
		if isZim {
			kx.Stop()
		}
		err := os.Remove(filepath.Join(dir, body.File))
		if isZim {
			kx.Start()
		}
		if err != nil {
			writeJSON(w, map[string]any{"error": err.Error()})
			return
		}
		writeJSON(w, map[string]any{"ok": true})
	})

	mux.Handle("/maps-data/", http.StripPrefix("/maps-data/", http.FileServer(noDirListing{http.Dir(p.Maps)})))
	mux.Handle("/maps-assets/", http.StripPrefix("/maps-assets/", http.FileServer(noDirListing{http.Dir(p.Assets)})))
	web := http.FileServer(noDirListing{http.Dir(p.Web)})
	mux.Handle("/", http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Cache-Control", "no-cache")
		web.ServeHTTP(w, r)
	}))

	return localOnly(mux)
}

// localOnly blocks requests that don't come from this computer, and rejects
// cross-site form posts to the API.
func localOnly(h http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		host := r.Host
		if i := strings.LastIndex(host, ":"); i >= 0 {
			host = host[:i]
		}
		if host != "127.0.0.1" && host != "localhost" {
			http.Error(w, "forbidden", http.StatusForbidden)
			return
		}
		if r.Method == http.MethodPost {
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

type noDirListing struct{ fs http.FileSystem }

func (n noDirListing) Open(name string) (http.File, error) {
	f, err := n.fs.Open(name)
	if err != nil {
		return nil, err
	}
	if st, err := f.Stat(); err == nil && st.IsDir() {
		if idx, err := n.fs.Open(strings.TrimSuffix(name, "/") + "/index.html"); err == nil {
			idx.Close()
			return f, nil
		}
		f.Close()
		return nil, os.ErrNotExist
	}
	return f, nil
}

func postOnly(w http.ResponseWriter, r *http.Request) bool {
	if r.Method != http.MethodPost {
		http.Error(w, "POST only", http.StatusMethodNotAllowed)
		return false
	}
	return true
}

func writeJSON(w http.ResponseWriter, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Cache-Control", "no-store")
	json.NewEncoder(w).Encode(v)
}

func fileExists(p string) bool { _, err := os.Stat(p); return err == nil }

func htmlEscape(s string) string {
	r := strings.NewReplacer("&", "&amp;", "<", "&lt;", ">", "&gt;", `"`, "&quot;")
	return r.Replace(s)
}
