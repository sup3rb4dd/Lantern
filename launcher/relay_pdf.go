package main

// PDF books for Lantern Relay. Book collections (like Kiwix's Water Treatment
// or Food for Preppers) are mostly PDFs, and Kiwix's search only knows their
// titles. So Lantern reads each PDF once, keeps the text of every page in
// lantern/pdfindex, and searches it with a small index of its own. Results
// point at a page: "2 Emergency Water Treatment p.3 [Water]".

import (
	"compress/gzip"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"math"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"sync"
	"time"
	"unicode"
)

const maxPdfsPerZim = 1500 // bigger collections (like all of Gutenberg) are skipped

type pdfDoc struct {
	Book    string   `json:"book"`  // ZIM name
	Path    string   `json:"path"`  // entry path inside the ZIM
	Title   string   `json:"title"` // document title
	Pages   []string `json:"pages"`
	Titles  []string `json:"titles,omitempty"` // first line of each page
	Scanned bool     `json:"scanned,omitempty"`
	Error   string   `json:"error,omitempty"`
}

type pdfCache struct {
	Zim     string    `json:"zim"`
	Size    int64     `json:"size"`
	ModTime int64     `json:"modTime"`
	Docs    []*pdfDoc `json:"docs"`
	Skipped string    `json:"skipped,omitempty"` // why the whole ZIM was skipped
}

type pdfPosting struct {
	doc, page int
	tf        int
}

type pdfStatus struct {
	State    string   `json:"state"` // none, indexing, needs-tool, ready
	Docs     int      `json:"docs"`
	Pages    int      `json:"pages"`
	Scanned  int      `json:"scanned"`
	Done     int      `json:"done"`
	Total    int      `json:"total"`
	Current  string   `json:"current"`
	Notes    []string `json:"notes"`
	ToolPath string   `json:"-"`
}

type pdfLibrary struct {
	p       Paths
	zims    func() []string
	base    func() string // kiwix-serve base URL
	tool    func() string
	client  *http.Client
	mu      sync.RWMutex
	docs    []*pdfDoc
	index   map[string][]pdfPosting
	pageLen [][]int // words per page, for scoring
	status  pdfStatus
	running bool
	kick    chan struct{}
}

func newPdfLibrary(p Paths, kx *Kiwix) *pdfLibrary {
	ks := newKiwixSearcher(kx)
	l := &pdfLibrary{
		p: p, zims: kx.zimFiles, base: ks.base,
		tool:   func() string { return pdftotextPath(p) },
		client: &http.Client{Timeout: 5 * time.Minute},
		index:  map[string][]pdfPosting{},
		status: pdfStatus{State: "none"},
		kick:   make(chan struct{}, 1),
	}
	return l
}

// Run indexes new or changed ZIMs now and then every few minutes, until ctx ends.
func (l *pdfLibrary) Run(ctx context.Context) {
	for {
		l.refresh(ctx)
		select {
		case <-ctx.Done():
			return
		case <-l.kick:
		case <-time.After(5 * time.Minute):
		}
	}
}

// Kick asks for a refresh soon (after a download finishes or the tool arrives).
func (l *pdfLibrary) Kick() {
	select {
	case l.kick <- struct{}{}:
	default:
	}
}

func (l *pdfLibrary) Status() pdfStatus {
	l.mu.RLock()
	defer l.mu.RUnlock()
	s := l.status
	s.Notes = append([]string{}, l.status.Notes...)
	return s
}

func (l *pdfLibrary) setStatus(f func(s *pdfStatus)) {
	l.mu.Lock()
	f(&l.status)
	l.mu.Unlock()
}

func (l *pdfLibrary) cachePath(zim string) string {
	return filepath.Join(l.p.Data, "pdfindex", strings.TrimSuffix(filepath.Base(zim), ".zim")+".json.gz")
}

func (l *pdfLibrary) refresh(ctx context.Context) {
	var all []*pdfDoc
	var notes []string
	needsTool := false
	zims := l.zims()
	for _, z := range zims {
		if ctx.Err() != nil {
			return
		}
		c, err := l.loadOrBuild(ctx, z)
		if err == errNoPdfTool {
			needsTool = true
			continue
		}
		if err != nil {
			notes = append(notes, filepath.Base(z)+": "+err.Error())
			continue
		}
		if c.Skipped != "" {
			notes = append(notes, c.Skipped)
		}
		all = append(all, c.Docs...)
	}
	l.rebuildIndex(all)
	l.setStatus(func(s *pdfStatus) {
		s.Notes = notes
		s.Current, s.Done, s.Total = "", 0, 0
		switch {
		case needsTool:
			s.State = "needs-tool"
		case s.Docs == 0:
			s.State = "none"
		default:
			s.State = "ready"
		}
	})
}

func (l *pdfLibrary) loadOrBuild(ctx context.Context, zim string) (*pdfCache, error) {
	fi, err := os.Stat(zim)
	if err != nil {
		return nil, err
	}
	if c, err := readPdfCache(l.cachePath(zim)); err == nil && c.Size == fi.Size() && c.ModTime == fi.ModTime().Unix() {
		return c, nil
	}
	book := strings.TrimSuffix(filepath.Base(zim), ".zim")
	c := &pdfCache{Zim: filepath.Base(zim), Size: fi.Size(), ModTime: fi.ModTime().Unix()}
	entries, tooMany, err := zimListByMime(zim, "application/pdf", maxPdfsPerZim+1)
	if err != nil {
		return nil, err
	}
	if tooMany {
		c.Skipped = fmt.Sprintf("%s: more than %d PDFs, too many to read for radio search", book, maxPdfsPerZim)
	}
	if len(entries) > 0 && c.Skipped == "" {
		titles := l.collectionTitles(ctx, book)
		for i := range entries {
			if t, ok := titles[entries[i].Path]; ok {
				entries[i].Title = t
			} else {
				entries[i].Title = fileTitle(entries[i].Title)
			}
		}
		tool := l.tool()
		if tool == "" {
			return nil, errNoPdfTool
		}
		base := l.base()
		if base == "" {
			return nil, errNoKiwix
		}
		tmp := filepath.Join(l.p.Temp, "pdfindex")
		os.MkdirAll(tmp, 0o755)
		for i, e := range entries {
			if ctx.Err() != nil {
				return nil, ctx.Err()
			}
			l.setStatus(func(s *pdfStatus) {
				s.State, s.Done, s.Total, s.Current = "indexing", i, len(entries), e.Title
			})
			d := &pdfDoc{Book: book, Path: e.Path, Title: cleanInline(e.Title)}
			if pages, titles, err := l.extract(ctx, base, tool, tmp, d); err != nil {
				d.Error = err.Error()
			} else if looksScanned(pages) {
				d.Scanned = true
			} else {
				d.Pages, d.Titles = pages, titles
			}
			c.Docs = append(c.Docs, d)
		}
	}
	if err := writePdfCache(l.cachePath(zim), c); err != nil {
		return nil, err
	}
	return c, nil
}

func (l *pdfLibrary) extract(ctx context.Context, base, tool, tmp string, d *pdfDoc) ([]string, []string, error) {
	var parts []string
	for _, s := range strings.Split(d.Path, "/") {
		parts = append(parts, url.PathEscape(s))
	}
	req, _ := http.NewRequestWithContext(ctx, "GET", base+"/raw/"+url.PathEscape(d.Book)+"/content/"+strings.Join(parts, "/"), nil)
	resp, err := l.client.Do(req)
	if err != nil {
		return nil, nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != 200 {
		return nil, nil, fmt.Errorf("couldn't read it from the ZIM (%s)", resp.Status)
	}
	f := filepath.Join(tmp, "doc.pdf")
	w, err := os.Create(f)
	if err != nil {
		return nil, nil, err
	}
	_, err = io.Copy(w, io.LimitReader(resp.Body, 500<<20))
	w.Close()
	defer os.Remove(f)
	if err != nil {
		return nil, nil, err
	}
	return pdfPages(ctx, tool, f)
}

// Book collections made with openZIM's nautilus (zimgit-*, usda-*) list their
// documents in database.js, e.g.
//
//	{'_id': '00001', 'ti': 'Purifying Water During an Emergency', ..., 'fp': ['Water (2).pdf']},
//
// The PDFs themselves are just files/Water (2).pdf, so the titles come from here.
var (
	reNautilusRow = regexp.MustCompile(`\{[^{}]*\}`)
	reNautilusTi  = regexp.MustCompile(`['"]ti['"]\s*:\s*(?:'((?:[^'\\]|\\.)*)'|"((?:[^"\\]|\\.)*)")`)
	reNautilusDsc = regexp.MustCompile(`['"]dsc['"]\s*:\s*(?:'((?:[^'\\]|\\.)*)'|"((?:[^"\\]|\\.)*)")`)
	reGenericName = regexp.MustCompile(`(?i)^(part|volume|vol\.?|chapter|book|section|guide)\s*[0-9ivx]+$`)
	reNautilusFp  = regexp.MustCompile(`['"]fp['"]\s*:\s*\[([^\]]*)\]`)
	reQuoted      = regexp.MustCompile(`'((?:[^'\\]|\\.)*)'|"((?:[^"\\]|\\.)*)"`)
)

func (l *pdfLibrary) collectionTitles(ctx context.Context, book string) map[string]string {
	out := map[string]string{}
	base := l.base()
	if base == "" {
		return out
	}
	req, _ := http.NewRequestWithContext(ctx, "GET", base+"/raw/"+url.PathEscape(book)+"/content/database.js", nil)
	resp, err := l.client.Do(req)
	if err != nil {
		return out
	}
	defer resp.Body.Close()
	if resp.StatusCode != 200 {
		return out
	}
	b, _ := io.ReadAll(io.LimitReader(resp.Body, 8<<20))
	return parseNautilusTitles(string(b))
}

func parseNautilusTitles(js string) map[string]string {
	out := map[string]string{}
	unq := func(m []string) string {
		s := m[1]
		if s == "" {
			s = m[2]
		}
		return strings.NewReplacer(`\'`, "'", `\"`, `"`, `\\`, `\`).Replace(s)
	}
	for _, row := range reNautilusRow.FindAllString(js, -1) {
		ti := reNautilusTi.FindStringSubmatch(row)
		fp := reNautilusFp.FindStringSubmatch(row)
		if ti == nil || fp == nil {
			continue
		}
		title := cleanInline(unq(ti))
		// "Part 4" says nothing over the radio; the description ("Vegetables and
		// Vegetable Products") does.
		if d := reNautilusDsc.FindStringSubmatch(row); d != nil && reGenericName.MatchString(title) {
			if dsc := cleanInline(unq(d)); dsc != "" {
				title = dsc
			}
		}
		for _, f := range reQuoted.FindAllStringSubmatch(fp[1], -1) {
			out["files/"+unq(f)] = title
		}
	}
	return out
}

// fileTitle makes a readable title from a file path: "files/Water (1).pdf" -> "Water (1)".
func fileTitle(p string) string {
	p = p[strings.LastIndex(p, "/")+1:]
	return strings.TrimSuffix(strings.TrimSuffix(p, ".pdf"), ".PDF")
}

func readPdfCache(p string) (*pdfCache, error) {
	f, err := os.Open(p)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	zr, err := gzip.NewReader(f)
	if err != nil {
		return nil, err
	}
	var c pdfCache
	return &c, json.NewDecoder(zr).Decode(&c)
}

func writePdfCache(p string, c *pdfCache) error {
	os.MkdirAll(filepath.Dir(p), 0o755)
	tmp := p + ".part"
	f, err := os.Create(tmp)
	if err != nil {
		return err
	}
	zw := gzip.NewWriter(f)
	err = json.NewEncoder(zw).Encode(c)
	if err2 := zw.Close(); err == nil {
		err = err2
	}
	if err2 := f.Close(); err == nil {
		err = err2
	}
	if err != nil {
		os.Remove(tmp)
		return err
	}
	return os.Rename(tmp, p)
}

// ---------- searching ----------

var pdfStopWords = map[string]bool{"the": true, "a": true, "an": true, "and": true, "or": true, "of": true,
	"to": true, "in": true, "on": true, "for": true, "is": true, "it": true, "how": true, "do": true,
	"i": true, "with": true, "what": true, "my": true, "be": true, "can": true, "at": true, "by": true,
	"long": true, "much": true, "many": true, "when": true, "where": true, "why": true, "which": true,
	"does": true, "should": true, "need": true, "you": true, "get": true, "make": true, "use": true}

func pdfTerms(s string) []string {
	f := strings.FieldsFunc(strings.ToLower(s), func(r rune) bool { return !unicode.IsLetter(r) && !unicode.IsDigit(r) })
	out := f[:0]
	for _, w := range f {
		if len(w) > 1 && !pdfStopWords[w] {
			out = append(out, pdfStem(w))
		}
	}
	return out
}

// pdfStem folds simple English word endings so "filters" finds "filter" and
// "boiling" finds "boil".
func pdfStem(w string) string {
	switch {
	case len(w) > 5 && strings.HasSuffix(w, "ing"):
		return undouble(w[:len(w)-3])
	case len(w) > 4 && strings.HasSuffix(w, "ed") && !strings.HasSuffix(w, "eed"):
		return undouble(w[:len(w)-2])
	case len(w) > 4 && strings.HasSuffix(w, "ies"):
		return w[:len(w)-3] + "y"
	case len(w) > 3 && strings.HasSuffix(w, "es") && strings.ContainsAny(w[len(w)-3:len(w)-2], "sxz"):
		return w[:len(w)-2]
	case len(w) > 3 && strings.HasSuffix(w, "s") && !strings.HasSuffix(w, "ss"):
		return w[:len(w)-1]
	}
	return w
}

// undouble turns "stopp" (from "stopping") back into "stop".
func undouble(w string) string {
	n := len(w)
	if n > 3 && w[n-1] == w[n-2] && !strings.ContainsRune("lsz", rune(w[n-1])) {
		return w[:n-1]
	}
	return w
}

func (l *pdfLibrary) rebuildIndex(docs []*pdfDoc) {
	idx := map[string][]pdfPosting{}
	lens := make([][]int, len(docs))
	nd, np, ns := 0, 0, 0
	for di, d := range docs {
		if d.Scanned {
			ns++
		}
		if len(d.Pages) == 0 {
			continue
		}
		nd++
		lens[di] = make([]int, len(d.Pages))
		titleTerms := pdfTerms(d.Title)
		for pi, p := range d.Pages {
			np++
			tf := map[string]int{}
			terms := pdfTerms(p)
			lens[di][pi] = len(terms)
			for _, t := range terms {
				tf[t]++
			}
			for _, t := range titleTerms {
				tf[t] += 2 // a document about the topic counts on every page
			}
			for t, n := range tf {
				idx[t] = append(idx[t], pdfPosting{doc: di, page: pi, tf: n})
			}
		}
	}
	l.mu.Lock()
	l.docs, l.index, l.pageLen = docs, idx, lens
	l.status.Docs, l.status.Pages, l.status.Scanned = nd, np, ns
	l.mu.Unlock()
}

type pdfHit struct {
	doc   *pdfDoc
	page  int // 0-based
	score float64
}

// Search ranks pages: pages with every word come first, then the rest, by a
// simple TF-IDF score. At most one hit per document, to keep results varied.
func (l *pdfLibrary) Search(query string, n int) []pdfHit {
	terms := pdfTerms(query)
	if len(terms) == 0 {
		return nil
	}
	l.mu.RLock()
	defer l.mu.RUnlock()
	total := float64(l.status.Pages) + 1
	type key struct{ doc, page int }
	score := map[key]float64{}
	matched := map[key]int{}
	seen := map[string]bool{}
	for _, t := range terms {
		if seen[t] {
			continue
		}
		seen[t] = true
		posts := l.index[t]
		if len(posts) == 0 {
			continue
		}
		idf := math.Log(total / float64(len(posts)))
		if idf < 0.1 {
			idf = 0.1
		}
		for _, p := range posts {
			k := key{p.doc, p.page}
			length := float64(l.pageLen[p.doc][p.page]) + 50
			score[k] += (1 + math.Log(float64(p.tf))) * idf * 300 / length
			matched[k]++
		}
	}
	nTerms := len(seen)
	best := map[int]pdfHit{}
	for k, s := range score {
		s += float64(matched[k]) * 100 // all words beat some words
		if matched[k] == nTerms {
			s += 1000
		}
		if h, ok := best[k.doc]; !ok || s > h.score {
			best[k.doc] = pdfHit{doc: l.docs[k.doc], page: k.page, score: s}
		}
	}
	hits := make([]pdfHit, 0, len(best))
	for _, h := range best {
		hits = append(hits, h)
	}
	sort.Slice(hits, func(a, b int) bool {
		if hits[a].score != hits[b].score {
			return hits[a].score > hits[b].score
		}
		return hits[a].doc.Title < hits[b].doc.Title
	})
	// only keep hits that matched at least as many words as half the query
	keep := hits[:0]
	for _, h := range hits {
		if h.score >= 100*math.Ceil(float64(nTerms)/2) {
			keep = append(keep, h)
		}
	}
	if len(keep) > n {
		keep = keep[:n]
	}
	return keep
}

// pdfArticle turns a document into sections: 0 is an overview, then one per page.
func pdfArticle(d *pdfDoc, source string) *article {
	a := &article{Title: d.Title, Source: source}
	first := ""
	if len(d.Pages) > 0 {
		first = d.Pages[0]
	}
	a.Sections = append(a.Sections, section{Title: "Summary",
		Text: fmt.Sprintf("%d pages. ?go N reads page N, ?toc lists them.\n%s", len(d.Pages), first)})
	for i, p := range d.Pages {
		t := ""
		if i < len(d.Titles) {
			t = d.Titles[i]
		}
		title := fmt.Sprintf("p.%d", i+1)
		if t != "" {
			title += " " + t
		}
		text := p
		if strings.TrimSpace(text) == "" {
			text = "(blank page)"
		}
		sec := section{Title: title, Text: text}
		if t != "" && strings.HasPrefix(text, strings.TrimSuffix(t, "…")) {
			sec.Short = fmt.Sprintf("p.%d", i+1) // the page's first words are its title: don't send them twice
		}
		a.Sections = append(a.Sections, sec)
	}
	return a
}
