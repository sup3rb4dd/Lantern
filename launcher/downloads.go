package main

import (
	"archive/zip"
	"bufio"
	"context"
	"encoding/json"
	"encoding/xml"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"os/exec"
	"path"
	"path/filepath"
	"regexp"
	"runtime"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"
)

// Download sources. Each can be overridden with an environment variable,
// which is only used for testing.
var (
	kiwixCatalog   = envOr("LANTERN_CATALOG", "https://library.kiwix.org/catalog/v2/entries")
	kiwixReleases  = envOr("LANTERN_KIWIX_RELEASES", "https://download.kiwix.org/release/kiwix-tools/")
	pmtilesRelease = envOr("LANTERN_PMTILES_RELEASE", "https://api.github.com/repos/protomaps/go-pmtiles/releases/latest")
	mapAssetsZip   = envOr("LANTERN_MAP_ASSETS", "https://codeload.github.com/protomaps/basemaps-assets/zip/refs/heads/main")
	protomapsBuild = envOr("LANTERN_PROTOMAPS_BUILDS", "https://build.protomaps.com/")
)

func envOr(k, def string) string {
	if v := os.Getenv(k); v != "" {
		return v
	}
	return def
}

const userAgent = "Lantern-offline-drive/" + version

// MapRegion is a preset area for offline maps.
type MapRegion struct {
	ID      string     `json:"id"`
	Title   string     `json:"title"`
	Desc    string     `json:"desc"`
	BBox    string     `json:"bbox"` // minLon,minLat,maxLon,maxLat
	MaxZoom int        `json:"maxzoom"`
	File    string     `json:"file"`
	Center  [2]float64 `json:"center"`
}

var mapRegions = []MapRegion{
	{ID: "oklahoma", Title: "Oklahoma — street level", Desc: "Every road, building and place name in the state.",
		BBox: "-103.01,33.61,-94.42,37.01", MaxZoom: 15, File: "oklahoma.pmtiles", Center: [2]float64{-97.5, 35.5}},
	{ID: "us-city", Title: "United States — city level", Desc: "Highways, towns and major roads for the whole country. Not street level.",
		BBox: "-125.0,24.3,-66.9,49.5", MaxZoom: 11, File: "united-states-city.pmtiles", Center: [2]float64{-98.5, 39.5}},
	{ID: "us-street", Title: "United States — street level", Desc: "Full detail for the whole country. Very large and slow to download.",
		BBox: "-125.0,24.3,-66.9,49.5", MaxZoom: 15, File: "united-states-street.pmtiles", Center: [2]float64{-98.5, 39.5}},
	{ID: "world", Title: "World overview", Desc: "Countries, coastlines and major cities worldwide.",
		BBox: "-180,-85,180,85", MaxZoom: 6, File: "world-overview.pmtiles", Center: [2]float64{0, 20}},
}

// Job is one item in the download queue.
type Job struct {
	ID       int     `json:"id"`
	Kind     string  `json:"kind"` // kiwix-tools | zim | map
	Title    string  `json:"title"`
	Name     string  `json:"name,omitempty"`
	URL      string  `json:"url,omitempty"`
	Region   string  `json:"region,omitempty"`
	BBox     string  `json:"bbox,omitempty"`
	MaxZoom  int     `json:"maxzoom,omitempty"`
	Status   string  `json:"status"` // queued | running | done | error | canceled
	Step     string  `json:"step"`
	Bytes    int64   `json:"bytes"`
	Total    int64   `json:"total"`
	Speed    float64 `json:"speed"` // bytes per second
	Error    string  `json:"error,omitempty"`
	cancel   context.CancelFunc
	ctx      context.Context
	canceled bool
}

// Downloader runs jobs one at a time so the drive and connection aren't overloaded.
type Downloader struct {
	p      Paths
	kx     *Kiwix
	mu     sync.Mutex
	jobs   []*Job
	nextID int
	wake   chan struct{}
	ctx    context.Context
	stop   context.CancelFunc
	client *http.Client
	onDone func(kind string) // called after a job finishes successfully
}

func newDownloader(p Paths, kx *Kiwix) *Downloader {
	ctx, stop := context.WithCancel(context.Background())
	d := &Downloader{p: p, kx: kx, wake: make(chan struct{}, 1), ctx: ctx, stop: stop,
		client: &http.Client{Transport: &http.Transport{Proxy: http.ProxyFromEnvironment, ResponseHeaderTimeout: 60 * time.Second}}}
	go d.worker()
	return d
}

func (d *Downloader) Shutdown() {
	d.mu.Lock()
	for _, j := range d.jobs {
		if j.cancel != nil {
			j.cancel()
		}
	}
	d.mu.Unlock()
	d.stop()
}

// Jobs returns a snapshot of the queue.
func (d *Downloader) Jobs() []Job {
	d.mu.Lock()
	defer d.mu.Unlock()
	out := make([]Job, len(d.jobs))
	for i, j := range d.jobs {
		out[i] = *j
		out[i].cancel = nil
		out[i].ctx = nil
	}
	return out
}

func (d *Downloader) active(kind, key string) bool {
	for _, j := range d.jobs {
		if j.Kind == kind && (j.Name == key || j.Region == key || j.URL == key || kind == "kiwix-tools") &&
			(j.Status == "queued" || j.Status == "running") {
			return true
		}
	}
	return false
}

// Enqueue adds a job. ZIM jobs automatically queue the Kiwix reader first if needed.
func (d *Downloader) Enqueue(j Job) (*Job, error) {
	d.mu.Lock()
	defer d.mu.Unlock()
	switch j.Kind {
	case "zim":
		if j.Name == "" && j.URL == "" {
			return nil, errors.New("missing name")
		}
		if !d.kx.Installed() && !d.active("kiwix-tools", "") {
			d.addLocked(Job{Kind: "kiwix-tools", Title: "Kiwix reader (needed for Wikipedia and books)"})
		}
		// Book collections are mostly PDFs; Lantern Relay needs pdftotext to search inside them.
		if strings.HasPrefix(strings.ToLower(j.Name), "zimgit") && runtime.GOOS == "windows" &&
			pdftotextPath(d.p) == "" && !d.active("pdf-tools", "") {
			defer d.addLocked(Job{Kind: "pdf-tools", Title: "PDF reader (for searching book collections over the radio)"})
		}
		key := j.Name
		if key == "" {
			key = j.URL
		}
		if d.active("zim", key) {
			return nil, errors.New("already in the queue")
		}
	case "map":
		if j.Region == "custom" {
			if !regexp.MustCompile(`^-?[\d.]+,-?[\d.]+,-?[\d.]+,-?[\d.]+$`).MatchString(j.BBox) {
				return nil, errors.New("custom area needs minLon,minLat,maxLon,maxLat")
			}
			if j.MaxZoom < 1 || j.MaxZoom > 15 {
				j.MaxZoom = 14
			}
			if j.Title == "" {
				j.Title = "Custom map area"
			}
		} else {
			r := findRegion(j.Region)
			if r == nil {
				return nil, errors.New("unknown map region")
			}
			j.Title = "Map: " + r.Title
		}
		if d.active("map", j.Region) && j.Region != "custom" {
			return nil, errors.New("already in the queue")
		}
	case "kiwix-tools":
		if d.active("kiwix-tools", "") {
			return nil, errors.New("already in the queue")
		}
		j.Title = "Kiwix reader"
	case "pdf-tools":
		if d.active("pdf-tools", "") {
			return nil, errors.New("already in the queue")
		}
		j.Title = "PDF reader (for searching book collections over the radio)"
	case "ai":
		if d.active("ai", "") {
			return nil, errors.New("already in the queue")
		}
		j.Title = "AI for Lantern Relay"
	default:
		return nil, errors.New("unknown download type")
	}
	return d.addLocked(j), nil
}

func (d *Downloader) addLocked(j Job) *Job {
	d.nextID++
	j.ID = d.nextID
	j.Status = "queued"
	j.Step = "Waiting"
	jj := j
	d.jobs = append(d.jobs, &jj)
	select {
	case d.wake <- struct{}{}:
	default:
	}
	return &jj
}

// Cancel stops a queued or running job. Partial downloads are kept so they can resume.
func (d *Downloader) Cancel(id int) {
	d.mu.Lock()
	defer d.mu.Unlock()
	for _, j := range d.jobs {
		if j.ID == id && (j.Status == "queued" || j.Status == "running") {
			j.canceled = true
			if j.cancel != nil {
				j.cancel()
			}
			if j.Status == "queued" {
				j.Status = "canceled"
				j.Step = "Canceled"
			}
		}
	}
}

// ClearFinished removes finished, failed and canceled jobs from the list.
func (d *Downloader) ClearFinished() {
	d.mu.Lock()
	defer d.mu.Unlock()
	var keep []*Job
	for _, j := range d.jobs {
		if j.Status == "queued" || j.Status == "running" {
			keep = append(keep, j)
		}
	}
	d.jobs = keep
}

func (d *Downloader) next() *Job {
	d.mu.Lock()
	defer d.mu.Unlock()
	for _, j := range d.jobs {
		if j.Status == "queued" {
			j.ctx, j.cancel = context.WithCancel(d.ctx)
			j.Status = "running"
			j.Step = "Starting"
			return j
		}
	}
	return nil
}

func (d *Downloader) worker() {
	for {
		j := d.next()
		if j == nil {
			select {
			case <-d.wake:
				continue
			case <-d.ctx.Done():
				return
			}
		}
		ctx := j.ctx

		var err error
		switch j.Kind {
		case "kiwix-tools":
			err = d.runKiwixTools(ctx, j)
		case "zim":
			err = d.runZim(ctx, j)
		case "map":
			err = d.runMap(ctx, j)
		case "pdf-tools":
			err = d.runPdfTools(ctx, j)
		case "ai":
			err = d.runAI(ctx, j)
		}
		d.mu.Lock()
		switch {
		case j.canceled:
			j.Status, j.Step = "canceled", "Canceled — partial download kept, start it again to resume"
		case err != nil:
			j.Status, j.Step, j.Error = "error", "Failed", err.Error()
			fmt.Println("Download failed:", j.Title, "-", err)
		default:
			if j.Status == "running" {
				j.Status = "done"
				if j.Step == "" || !strings.HasPrefix(j.Step, "Already") {
					j.Step = "Finished"
				}
			}
			fmt.Println("Download finished:", j.Title)
			if d.onDone != nil {
				go d.onDone(j.Kind)
			}
		}
		j.Speed = 0
		if j.cancel != nil {
			j.cancel()
		}
		d.mu.Unlock()
	}
}

func (d *Downloader) setStep(j *Job, step string) {
	d.mu.Lock()
	j.Step = step
	d.mu.Unlock()
}

func (d *Downloader) setProgress(j *Job, bytes, total int64, speed float64) {
	d.mu.Lock()
	j.Bytes, j.Total, j.Speed = bytes, total, speed
	d.mu.Unlock()
}

// ---------- HTTP helpers ----------

func (d *Downloader) get(ctx context.Context, u string) (*http.Response, error) {
	req, err := http.NewRequestWithContext(ctx, "GET", u, nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("User-Agent", userAgent)
	resp, err := d.client.Do(req)
	if err != nil {
		return nil, err
	}
	if resp.StatusCode >= 400 {
		resp.Body.Close()
		return nil, fmt.Errorf("%s returned %s", hostOf(u), resp.Status)
	}
	return resp, nil
}

func (d *Downloader) getBytes(ctx context.Context, u string) ([]byte, error) {
	resp, err := d.get(ctx, u)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	return io.ReadAll(io.LimitReader(resp.Body, 32<<20))
}

func hostOf(u string) string {
	if p, err := url.Parse(u); err == nil {
		return p.Host
	}
	return u
}

// downloadFile fetches u into dest with resume support and automatic retries.
func (d *Downloader) downloadFile(ctx context.Context, j *Job, u, dest string, knownSize int64) error {
	part := filepath.Join(d.p.Temp, filepath.Base(dest)+".part")
	var lastErr error
	for attempt := 0; attempt < 8; attempt++ {
		if ctx.Err() != nil {
			return ctx.Err()
		}
		if attempt > 0 {
			wait := time.Duration(attempt*attempt) * 2 * time.Second
			d.setStep(j, fmt.Sprintf("Connection problem, retrying in %ds (%v)", int(wait.Seconds()), lastErr))
			select {
			case <-time.After(wait):
			case <-ctx.Done():
				return ctx.Err()
			}
		}
		lastErr = d.downloadOnce(ctx, j, u, part, knownSize)
		if lastErr == nil {
			break
		}
		var pe permanentError
		if errors.As(lastErr, &pe) {
			return pe.err
		}
		if ctx.Err() != nil {
			return ctx.Err()
		}
	}
	if lastErr != nil {
		return lastErr
	}
	os.MkdirAll(filepath.Dir(dest), 0o755)
	os.Remove(dest)
	return os.Rename(part, dest)
}

// permanentError marks failures that retrying won't fix (missing file, full drive).
type permanentError struct{ err error }

func (p permanentError) Error() string { return p.err.Error() }

func (d *Downloader) downloadOnce(ctx context.Context, j *Job, u, part string, knownSize int64) error {
	var have int64
	if st, err := os.Stat(part); err == nil {
		have = st.Size()
	}
	if knownSize > 0 && have == knownSize {
		return nil
	}
	req, err := http.NewRequestWithContext(ctx, "GET", u, nil)
	if err != nil {
		return err
	}
	req.Header.Set("User-Agent", userAgent)
	if have > 0 {
		req.Header.Set("Range", fmt.Sprintf("bytes=%d-", have))
	}
	resp, err := d.client.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()

	flags := os.O_CREATE | os.O_WRONLY
	switch {
	case resp.StatusCode == http.StatusPartialContent:
		flags |= os.O_APPEND
	case resp.StatusCode == http.StatusRequestedRangeNotSatisfiable && have > 0:
		return nil // already complete
	case resp.StatusCode == http.StatusOK:
		have = 0
		flags |= os.O_TRUNC
	default:
		err := fmt.Errorf("%s returned %s", hostOf(u), resp.Status)
		if resp.StatusCode >= 400 && resp.StatusCode < 500 && resp.StatusCode != 408 && resp.StatusCode != 429 {
			return permanentError{err}
		}
		return err
	}

	total := knownSize
	if resp.ContentLength > 0 {
		total = have + resp.ContentLength
	}
	if total > 0 {
		if free, _ := freeSpace(d.p.Root); free > 0 && uint64(total-have) > free {
			return permanentError{fmt.Errorf("not enough space on the drive: need %s more, only %s free",
				humanBytes(total-have), humanBytes(int64(free)))}
		}
	}

	f, err := os.OpenFile(part, flags, 0o644)
	if err != nil {
		return err
	}
	defer f.Close()

	d.setStep(j, "Downloading")
	w := bufio.NewWriterSize(f, 1<<20)
	buf := make([]byte, 256<<10)
	written := have
	lastT, lastB := time.Now(), written
	speed := 0.0
	d.setProgress(j, written, total, 0)
	for {
		n, rerr := resp.Body.Read(buf)
		if n > 0 {
			if _, werr := w.Write(buf[:n]); werr != nil {
				return werr
			}
			written += int64(n)
			if dt := time.Since(lastT); dt > 700*time.Millisecond {
				inst := float64(written-lastB) / dt.Seconds()
				if speed == 0 {
					speed = inst
				} else {
					speed = speed*0.7 + inst*0.3
				}
				lastT, lastB = time.Now(), written
				d.setProgress(j, written, total, speed)
			}
		}
		if rerr == io.EOF {
			break
		}
		if rerr != nil {
			w.Flush()
			return rerr
		}
	}
	if err := w.Flush(); err != nil {
		return err
	}
	d.setProgress(j, written, total, speed)
	if total > 0 && written < total {
		return fmt.Errorf("connection closed early")
	}
	return nil
}

// ---------- Kiwix reader ----------

var kiwixZipRe = regexp.MustCompile(`kiwix-tools_win-x86_64-([0-9]+(?:\.[0-9]+)*)\.zip(?:["'<\s]|$)`)

func versionLess(a, b string) bool {
	as, bs := strings.Split(a, "."), strings.Split(b, ".")
	for i := 0; i < len(as) || i < len(bs); i++ {
		var x, y int
		if i < len(as) {
			x, _ = strconv.Atoi(as[i])
		}
		if i < len(bs) {
			y, _ = strconv.Atoi(bs[i])
		}
		if x != y {
			return x < y
		}
	}
	return false
}

func (d *Downloader) runKiwixTools(ctx context.Context, j *Job) error {
	if d.kx.Installed() {
		d.setStep(j, "Already installed")
		return nil
	}
	if runtime.GOOS != "windows" && os.Getenv("LANTERN_TEST") == "" {
		return errors.New("automatic Kiwix install is only set up for Windows")
	}
	d.setStep(j, "Finding the latest Kiwix release")
	body, err := d.getBytes(ctx, kiwixReleases)
	if err != nil {
		return err
	}
	best := ""
	for _, m := range kiwixZipRe.FindAllStringSubmatch(string(body), -1) {
		if best == "" || versionLess(best, m[1]) {
			best = m[1]
		}
	}
	if best == "" {
		return errors.New("couldn't find a Windows build of Kiwix on download.kiwix.org")
	}
	name := "kiwix-tools_win-x86_64-" + best + ".zip"
	j.Title = "Kiwix reader " + best
	zipPath := filepath.Join(d.p.Temp, name)
	if err := d.smartDownload(ctx, j, strings.TrimSuffix(kiwixReleases, "/")+"/"+name, zipPath, 0); err != nil {
		return err
	}
	d.setStep(j, "Unpacking")
	if err := unzipFlatten(zipPath, d.p.Kiwix, nil); err != nil {
		return fmt.Errorf("unpacking Kiwix: %w", err)
	}
	os.Remove(zipPath)
	if !d.kx.Installed() {
		return errors.New("kiwix-serve.exe wasn't found in the download")
	}
	d.kx.Restart()
	return nil
}

// ---------- PDF reader (Xpdf's pdftotext) ----------

var (
	xpdfPage     = envOr("LANTERN_XPDF_PAGE", "https://www.xpdfreader.com/download.html")
	xpdfDownload = envOr("LANTERN_XPDF_DL", "https://dl.xpdfreader.com/")
	xpdfZipRe    = regexp.MustCompile(`xpdf-tools-win-([0-9]+(?:\.[0-9]+)+)\.zip`)
)

func (d *Downloader) runPdfTools(ctx context.Context, j *Job) error {
	if pdftotextPath(d.p) != "" {
		d.setStep(j, "Already installed")
		return nil
	}
	if runtime.GOOS != "windows" {
		return errors.New("install pdftotext from your system's packages (poppler-utils)")
	}
	d.setStep(j, "Finding the latest PDF reader")
	best := "4.05" // fallback if the download page can't be read
	if body, err := d.getBytes(ctx, xpdfPage); err == nil {
		for _, m := range xpdfZipRe.FindAllStringSubmatch(string(body), -1) {
			if versionLess(best, m[1]) {
				best = m[1]
			}
		}
	}
	name := "xpdf-tools-win-" + best + ".zip"
	j.Title = "PDF reader (Xpdf tools " + best + ")"
	zipPath := filepath.Join(d.p.Temp, name)
	if err := d.downloadFile(ctx, j, strings.TrimSuffix(xpdfDownload, "/")+"/"+name, zipPath, 0); err != nil {
		return err
	}
	d.setStep(j, "Unpacking")
	os.MkdirAll(d.p.Tools, 0o755)
	tmp := filepath.Join(d.p.Temp, "xpdf")
	os.RemoveAll(tmp)
	err := unzipFlatten(zipPath, tmp, func(p string) bool {
		return p == "bin64/pdftotext.exe" || p == "COPYING3" || p == "COPYING" || p == "README"
	})
	os.Remove(zipPath)
	if err != nil {
		return fmt.Errorf("unpacking the PDF reader: %w", err)
	}
	defer os.RemoveAll(tmp)
	if err := os.Rename(filepath.Join(tmp, "bin64", "pdftotext.exe"), filepath.Join(d.p.Tools, "pdftotext.exe")); err != nil {
		return errors.New("pdftotext.exe wasn't found in the download")
	}
	for _, f := range []string{"COPYING3", "COPYING", "README"} {
		os.Rename(filepath.Join(tmp, f), filepath.Join(d.p.Tools, "xpdf-"+f+".txt"))
	}
	return nil
}

// unzipFlatten extracts a zip, dropping the top folder if everything is inside one.
// keep, if set, filters which entries (by path after flattening) to extract.
func unzipFlatten(zipPath, dest string, keep func(string) bool) error {
	zr, err := zip.OpenReader(zipPath)
	if err != nil {
		return err
	}
	defer zr.Close()
	prefix := ""
	if len(zr.File) > 0 {
		first := strings.SplitN(zr.File[0].Name, "/", 2)[0] + "/"
		all := true
		for _, f := range zr.File {
			if !strings.HasPrefix(f.Name, first) {
				all = false
				break
			}
		}
		if all {
			prefix = first
		}
	}
	for _, f := range zr.File {
		rel := strings.TrimPrefix(f.Name, prefix)
		if rel == "" || strings.HasSuffix(rel, "/") {
			continue
		}
		clean := path.Clean(rel)
		if strings.HasPrefix(clean, "../") || strings.HasPrefix(clean, "/") || clean == ".." {
			continue
		}
		if keep != nil && !keep(clean) {
			continue
		}
		out := filepath.Join(dest, filepath.FromSlash(clean))
		os.MkdirAll(filepath.Dir(out), 0o755)
		rc, err := f.Open()
		if err != nil {
			return err
		}
		wf, err := os.Create(out)
		if err != nil {
			rc.Close()
			return err
		}
		_, err = io.Copy(wf, rc)
		rc.Close()
		wf.Close()
		if err != nil {
			return err
		}
		if f.Mode()&0o111 != 0 {
			os.Chmod(out, 0o755)
		}
	}
	return nil
}

// ---------- Kiwix catalog and ZIM content ----------

// CatalogEntry is one downloadable item in the online Kiwix library.
type CatalogEntry struct {
	Name     string `json:"name"`
	Title    string `json:"title"`
	Summary  string `json:"summary"`
	Language string `json:"language"`
	Updated  string `json:"updated"`
	Size     int64  `json:"size"`
	URL      string `json:"url"`
	File     string `json:"file"`
}

type opdsFeed struct {
	Entries []struct {
		Title    string `xml:"title"`
		Summary  string `xml:"summary"`
		Name     string `xml:"name"`
		Flavour  string `xml:"flavour"`
		Language string `xml:"language"`
		Updated  string `xml:"updated"`
		Links    []struct {
			Rel    string `xml:"rel,attr"`
			Type   string `xml:"type,attr"`
			Href   string `xml:"href,attr"`
			Length string `xml:"length,attr"`
		} `xml:"link"`
	} `xml:"entry"`
}

// Catalog queries the online Kiwix library.
func (d *Downloader) Catalog(ctx context.Context, q url.Values) ([]CatalogEntry, error) {
	u := kiwixCatalog + "?" + q.Encode()
	body, err := d.getBytes(ctx, u)
	if err != nil {
		return nil, err
	}
	var feed opdsFeed
	if err := xml.Unmarshal(body, &feed); err != nil {
		return nil, fmt.Errorf("couldn't read the Kiwix catalog: %w", err)
	}
	var out []CatalogEntry
	for _, e := range feed.Entries {
		key := strings.TrimSpace(e.Name)
		if f := strings.TrimSpace(e.Flavour); f != "" && !strings.HasSuffix(key, "_"+f) {
			key += "_" + f // e.g. wikipedia_en_all + maxi
		}
		title := strings.TrimSpace(e.Title)
		switch strings.TrimSpace(e.Flavour) {
		case "maxi":
			title += " (full, with images)"
		case "nopic":
			title += " (text only, no images)"
		case "mini":
			title += " (intros only)"
		}
		ce := CatalogEntry{Name: key, Title: title, Summary: strings.TrimSpace(e.Summary),
			Language: e.Language, Updated: e.Updated}
		for _, l := range e.Links {
			if l.Type == "application/x-zim" || strings.HasSuffix(l.Href, ".zim.meta4") || strings.HasSuffix(l.Href, ".zim") {
				ce.URL = strings.TrimSuffix(l.Href, ".meta4")
				ce.Size, _ = strconv.ParseInt(l.Length, 10, 64)
				if pu, err := url.Parse(ce.URL); err == nil {
					ce.File = path.Base(pu.Path)
				}
			}
		}
		if ce.URL != "" {
			out = append(out, ce)
		}
	}
	return out, nil
}

var flavourRe = regexp.MustCompile(`^(.+)_(maxi|nopic|mini)$`)

// LookupBook finds the newest catalog entry for a book key such as
// "wikipedia_en_all_maxi". The catalog may list it either under that name or
// as name "wikipedia_en_all" with flavour "maxi".
func (d *Downloader) LookupBook(ctx context.Context, key string) (*CatalogEntry, error) {
	try := func(name string) (*CatalogEntry, error) {
		res, err := d.Catalog(ctx, url.Values{"name": {name}, "count": {"30"}})
		if err != nil {
			return nil, err
		}
		for i := range res {
			if res[i].Name == key {
				return &res[i], nil
			}
		}
		return nil, nil
	}
	e, err := try(key)
	if err != nil || e != nil {
		return e, err
	}
	if m := flavourRe.FindStringSubmatch(key); m != nil {
		return try(m[1])
	}
	return nil, nil
}

// installedZimsFor returns installed files that are versions of the named book.
func (d *Downloader) installedZimsFor(name string) []string {
	if name == "" {
		return nil
	}
	entries, _ := os.ReadDir(d.p.Zim)
	var out []string
	re := regexp.MustCompile(`^` + regexp.QuoteMeta(name) + `_\d{4}-\d{2}\.zim$`)
	for _, e := range entries {
		if re.MatchString(e.Name()) || e.Name() == name+".zim" {
			out = append(out, e.Name())
		}
	}
	return out
}

func (d *Downloader) runZim(ctx context.Context, j *Job) error {
	var entry CatalogEntry
	if j.Name != "" {
		d.setStep(j, "Looking up the latest version")
		e, err := d.LookupBook(ctx, j.Name)
		if err != nil {
			return err
		}
		if e == nil {
			return fmt.Errorf("%q isn't in the Kiwix library right now — try the search box instead", j.Name)
		}
		entry = *e
	} else {
		pu, err := url.Parse(j.URL)
		if err != nil || (pu.Scheme != "https" && pu.Scheme != "http") || !strings.HasSuffix(pu.Path, ".zim") {
			return errors.New("not a ZIM download link")
		}
		entry = CatalogEntry{URL: j.URL, File: path.Base(pu.Path)}
	}
	if entry.Title != "" && j.Title == "" {
		j.Title = entry.Title
	}
	if !regexp.MustCompile(`^[A-Za-z0-9._-]+\.zim$`).MatchString(entry.File) {
		return errors.New("unexpected file name from the catalog")
	}
	dest := filepath.Join(d.p.Zim, entry.File)
	if st, err := os.Stat(dest); err == nil && (entry.Size == 0 || st.Size() == entry.Size) {
		d.setProgress(j, st.Size(), st.Size(), 0)
		d.setStep(j, "Already up to date")
		return nil
	}
	if err := d.smartDownload(ctx, j, entry.URL, dest, entry.Size); err != nil {
		return err
	}
	// Remove older versions of the same book to free space.
	for _, old := range d.installedZimsFor(j.Name) {
		if old != entry.File {
			os.Remove(filepath.Join(d.p.Zim, old))
		}
	}
	d.setStep(j, "Adding to the library")
	d.kx.Restart()
	d.setStep(j, "Finished")
	return nil
}

// ---------- Offline maps ----------

func findRegion(id string) *MapRegion {
	for i := range mapRegions {
		if mapRegions[i].ID == id {
			return &mapRegions[i]
		}
	}
	return nil
}

func (d *Downloader) pmtilesExe() string {
	name := "pmtiles"
	if runtime.GOOS == "windows" {
		name += ".exe"
	}
	return filepath.Join(d.p.Tools, name)
}

func (d *Downloader) ensurePmtiles(ctx context.Context, j *Job) error {
	if _, err := os.Stat(d.pmtilesExe()); err == nil {
		return nil
	}
	d.setStep(j, "Getting the map tool")
	body, err := d.getBytes(ctx, pmtilesRelease)
	if err != nil {
		return err
	}
	var rel struct {
		Assets []struct {
			Name string `json:"name"`
			URL  string `json:"browser_download_url"`
		} `json:"assets"`
	}
	if err := json.Unmarshal(body, &rel); err != nil {
		return fmt.Errorf("reading pmtiles release info: %w", err)
	}
	want := "Windows_x86_64"
	if runtime.GOOS == "linux" {
		want = "Linux_x86_64"
	}
	var assetURL, assetName string
	for _, a := range rel.Assets {
		if strings.Contains(a.Name, want) && (strings.HasSuffix(a.Name, ".zip") || strings.HasSuffix(a.Name, ".tar.gz")) {
			assetURL, assetName = a.URL, a.Name
			break
		}
	}
	if assetURL == "" || !strings.HasSuffix(assetName, ".zip") {
		return errors.New("couldn't find the pmtiles tool for this computer")
	}
	zipPath := filepath.Join(d.p.Temp, assetName)
	if err := d.downloadFile(ctx, j, assetURL, zipPath, 0); err != nil {
		return err
	}
	exeName := filepath.Base(d.pmtilesExe())
	if err := unzipFlatten(zipPath, d.p.Tools, func(p string) bool { return path.Base(p) == exeName }); err != nil {
		return err
	}
	os.Remove(zipPath)
	// The exe may have been inside a subfolder; move it up.
	if _, err := os.Stat(d.pmtilesExe()); err != nil {
		filepath.Walk(d.p.Tools, func(p string, info os.FileInfo, err error) error {
			if err == nil && !info.IsDir() && info.Name() == exeName {
				os.Rename(p, d.pmtilesExe())
			}
			return nil
		})
	}
	if _, err := os.Stat(d.pmtilesExe()); err != nil {
		return errors.New("pmtiles tool missing after download")
	}
	return nil
}

var mapFontDirs = []string{"Noto Sans Regular", "Noto Sans Medium", "Noto Sans Italic"}

func (d *Downloader) ensureMapAssets(ctx context.Context, j *Job) error {
	marker := filepath.Join(d.p.Assets, "fonts", "Noto Sans Regular", "0-255.pbf")
	if _, err := os.Stat(marker); err == nil {
		return nil
	}
	d.setStep(j, "Getting map fonts and icons")
	zipPath := filepath.Join(d.p.Temp, "basemaps-assets.zip")
	if err := d.downloadFile(ctx, j, mapAssetsZip, zipPath, 0); err != nil {
		return err
	}
	keep := func(p string) bool {
		for _, f := range mapFontDirs {
			if strings.HasPrefix(p, "fonts/"+f+"/") {
				return true
			}
		}
		return strings.HasPrefix(p, "sprites/v4/light")
	}
	if err := unzipFlatten(zipPath, d.p.Assets, keep); err != nil {
		return err
	}
	os.Remove(zipPath)
	if _, err := os.Stat(marker); err != nil {
		return errors.New("map fonts missing after download")
	}
	return nil
}

// latestBuild finds the newest Protomaps daily world map (kept for about a week).
func (d *Downloader) latestBuild(ctx context.Context) (string, error) {
	now := time.Now().UTC()
	for i := 0; i < 21; i++ {
		u := fmt.Sprintf("%s%s.pmtiles", protomapsBuild, now.AddDate(0, 0, -i).Format("20060102"))
		req, _ := http.NewRequestWithContext(ctx, "HEAD", u, nil)
		req.Header.Set("User-Agent", userAgent)
		resp, err := d.client.Do(req)
		if err != nil {
			if ctx.Err() != nil {
				return "", ctx.Err()
			}
			continue
		}
		resp.Body.Close()
		if resp.StatusCode == 200 {
			return u, nil
		}
	}
	return "", errors.New("couldn't find a recent world map build at build.protomaps.com")
}

var sizeRe = regexp.MustCompile(`archive size of ([\d.]+)\s*([KMGT]?i?B)`)

func parseHumanSize(num, unit string) int64 {
	f, _ := strconv.ParseFloat(num, 64)
	mult := map[string]float64{"B": 1, "KB": 1e3, "MB": 1e6, "GB": 1e9, "TB": 1e12,
		"KiB": 1 << 10, "MiB": 1 << 20, "GiB": 1 << 30, "TiB": 1 << 40}[unit]
	return int64(f * mult)
}

func (d *Downloader) runMap(ctx context.Context, j *Job) error {
	bbox, maxzoom, file := j.BBox, j.MaxZoom, ""
	if r := findRegion(j.Region); r != nil {
		bbox, maxzoom, file = r.BBox, r.MaxZoom, r.File
	} else {
		file = fmt.Sprintf("custom-%s.pmtiles", time.Now().Format("20060102-150405"))
	}
	if err := d.ensurePmtiles(ctx, j); err != nil {
		return err
	}
	if err := d.ensureMapAssets(ctx, j); err != nil {
		return err
	}
	d.setProgress(j, 0, 0, 0)
	d.setStep(j, "Finding the newest world map")
	src, err := d.latestBuild(ctx)
	if err != nil {
		return err
	}
	flags := []string{"--bbox=" + bbox, fmt.Sprintf("--maxzoom=%d", maxzoom)}

	// Dry run first to learn the size.
	d.setStep(j, "Measuring the map area")
	dryArgs := append([]string{"extract", src, filepath.Join(d.p.Temp, "dry.pmtiles")}, flags...)
	dry := exec.CommandContext(ctx, d.pmtilesExe(), append(dryArgs, "--dry-run")...)
	hideWindow(dry)
	var estimate int64
	if out, err := dry.CombinedOutput(); err == nil {
		if m := sizeRe.FindStringSubmatch(string(out)); m != nil {
			estimate = parseHumanSize(m[1], m[2])
		}
	} else if ctx.Err() != nil {
		return ctx.Err()
	}
	if estimate > 0 {
		if free, _ := freeSpace(d.p.Root); free > 0 && uint64(estimate) > free {
			return fmt.Errorf("not enough space: this map needs about %s, only %s free",
				humanBytes(estimate), humanBytes(int64(free)))
		}
	}

	tmp := filepath.Join(d.p.Temp, file)
	os.Remove(tmp)
	cmd := exec.CommandContext(ctx, d.pmtilesExe(), append([]string{"extract", src, tmp}, flags...)...)
	hideWindow(cmd)
	logf, _ := os.Create(filepath.Join(d.p.Data, "maps.log"))
	if logf != nil {
		cmd.Stdout, cmd.Stderr = logf, logf
		defer logf.Close()
	}
	d.setStep(j, "Downloading map tiles")
	if err := cmd.Start(); err != nil {
		return err
	}
	attachToJob(cmd)
	done := make(chan error, 1)
	go func() { done <- cmd.Wait() }()
	tick := time.NewTicker(time.Second)
	defer tick.Stop()
	var last int64
	lastT := time.Now()
	speed := 0.0
	for {
		select {
		case err := <-done:
			if ctx.Err() != nil {
				os.Remove(tmp)
				return ctx.Err()
			}
			if err != nil {
				return fmt.Errorf("map download failed (details in lantern\\maps.log): %v", err)
			}
			d.setStep(j, "Saving")
			dest := filepath.Join(d.p.Maps, file)
			os.Remove(dest)
			if err := os.Rename(tmp, dest); err != nil {
				return err
			}
			if st, err := os.Stat(dest); err == nil {
				d.setProgress(j, st.Size(), st.Size(), 0)
			}
			return nil
		case <-tick.C:
			if st, err := os.Stat(tmp); err == nil {
				cur := st.Size()
				inst := float64(cur-last) / time.Since(lastT).Seconds()
				speed = speed*0.7 + inst*0.3
				last, lastT = cur, time.Now()
				d.setProgress(j, cur, estimate, speed)
			}
		}
	}
}

// ---------- installed content ----------

type InstalledFile struct {
	File string `json:"file"`
	Name string `json:"name"` // kiwix book name (file name without .zim)
	Size int64  `json:"size"`
}

func listFiles(dir, ext string) []InstalledFile {
	entries, _ := os.ReadDir(dir)
	out := []InstalledFile{}
	for _, e := range entries {
		if e.IsDir() || !strings.HasSuffix(strings.ToLower(e.Name()), ext) {
			continue
		}
		info, err := e.Info()
		if err != nil {
			continue
		}
		out = append(out, InstalledFile{File: e.Name(), Name: strings.TrimSuffix(e.Name(), filepath.Ext(e.Name())), Size: info.Size()})
	}
	sort.Slice(out, func(a, b int) bool { return out[a].File < out[b].File })
	return out
}

func humanBytes(n int64) string {
	f := float64(n)
	for _, u := range []string{"B", "KB", "MB", "GB", "TB"} {
		if f < 1000 || u == "TB" {
			if u == "B" {
				return fmt.Sprintf("%d B", n)
			}
			return fmt.Sprintf("%.1f %s", f, u)
		}
		f /= 1000
	}
	return ""
}
