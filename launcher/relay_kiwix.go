package main

// Search and article lookups against the kiwix-serve that Lantern already runs.
// Everything here talks to 127.0.0.1 only: no internet is involved. Each .zim
// file carries its own full-text index, so there is nothing to build first.

import (
	"context"
	"encoding/xml"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"path/filepath"
	"strings"
	"sync"
	"time"
)

type searchHit struct {
	Title  string
	Book   string // ZIM name (file name without .zim)
	Path   string // entry path inside the ZIM, URL-escaped
	Source string // short label shown on the radio
	Snip   string
	pdf    *pdfDoc // set for a page of a PDF book
	page   int     // 0-based page, for PDF hits
}

type kiwixSearcher struct {
	base   func() string // e.g. http://127.0.0.1:8485/kiwix, "" when Kiwix isn't running
	books  func() []string
	client *http.Client
}

func newKiwixSearcher(kx *Kiwix) *kiwixSearcher {
	return &kiwixSearcher{
		base: func() string {
			if p := kx.Port(); p != 0 {
				return fmt.Sprintf("http://127.0.0.1:%d/kiwix", p)
			}
			return ""
		},
		books: func() []string {
			var out []string
			for _, z := range kx.zimFiles() {
				out = append(out, strings.TrimSuffix(filepath.Base(z), filepath.Ext(z)))
			}
			return out
		},
		client: &http.Client{Timeout: 60 * time.Second},
	}
}

var errNoKiwix = errors.New("Lantern has no searchable content running right now")

type rssResult struct {
	Total string `xml:"channel>totalResults"`
	Items []struct {
		Title string `xml:"title"`
		Link  string `xml:"link"`
		Desc  struct {
			Inner string `xml:",innerxml"`
		} `xml:"description"`
		Book string `xml:"book>title"`
	} `xml:"channel>item"`
}

// Search finds up to count hits starting at start, across every ZIM on the drive.
func (k *kiwixSearcher) Search(ctx context.Context, query string, start, count int) ([]searchHit, int, error) {
	base, books := k.base(), k.books()
	if base == "" || len(books) == 0 {
		return nil, 0, errNoKiwix
	}
	hits, total, err := k.searchBooks(ctx, base, books, query, start, count)
	if err == nil {
		return hits, total, nil
	}
	if len(books) == 1 {
		return nil, 0, err
	}
	// A combined search fails if the ZIMs are in different languages or one has
	// no full-text index. Search them one at a time and take turns between them.
	return k.searchEach(ctx, base, books, query, start, count)
}

func (k *kiwixSearcher) searchBooks(ctx context.Context, base string, books []string, query string, start, count int) ([]searchHit, int, error) {
	q := url.Values{}
	for _, b := range books {
		q.Add("books.name", b)
	}
	q.Set("pattern", query)
	q.Set("format", "xml")
	// Always read from the first result: kiwix-serve versions disagree on
	// whether "start" counts from 0 or 1.
	q.Set("pageLength", fmt.Sprint(clampInt(start+count, 1, 140)))
	req, _ := http.NewRequestWithContext(ctx, "GET", base+"/search?"+q.Encode(), nil)
	resp, err := k.client.Do(req)
	if err != nil {
		return nil, 0, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != 200 {
		return nil, 0, fmt.Errorf("kiwix search: %s", resp.Status)
	}
	var rss rssResult
	if err := xml.NewDecoder(io.LimitReader(resp.Body, 4<<20)).Decode(&rss); err != nil {
		return nil, 0, err
	}
	var hits []searchHit
	for _, it := range rss.Items {
		book, path, ok := splitContentLink(it.Link)
		if !ok {
			continue
		}
		hits = append(hits, searchHit{Title: cleanInline(it.Title), Book: book, Path: path,
			Source: sourceLabel(book, it.Book), Snip: cleanInline(stripTags(it.Desc.Inner))})
	}
	total := 0
	fmt.Sscan(strings.TrimSpace(rss.Total), &total)
	if start >= len(hits) {
		return nil, total, nil
	}
	hits = hits[start:]
	if len(hits) > count {
		hits = hits[:count]
	}
	return hits, total, nil
}

func (k *kiwixSearcher) searchEach(ctx context.Context, base string, books []string, query string, start, count int) ([]searchHit, int, error) {
	need := start + count
	type res struct {
		hits  []searchHit
		total int
	}
	results := make([]res, len(books))
	var wg sync.WaitGroup
	for i, b := range books {
		wg.Add(1)
		go func(i int, b string) {
			defer wg.Done()
			h, t, err := k.searchBooks(ctx, base, []string{b}, query, 0, need)
			if err == nil {
				results[i] = res{h, t}
			}
		}(i, b)
	}
	wg.Wait()
	var merged []searchHit
	total := 0
	for _, r := range results {
		total += r.total
	}
	for rank := 0; len(merged) < need; rank++ {
		added := false
		for _, r := range results {
			if rank < len(r.hits) {
				merged = append(merged, r.hits[rank])
				added = true
			}
		}
		if !added {
			break
		}
	}
	if start >= len(merged) {
		return nil, total, nil
	}
	end := start + count
	if end > len(merged) {
		end = len(merged)
	}
	return merged[start:end], total, nil
}

// Fetch downloads one entry's HTML and turns it into sections.
func (k *kiwixSearcher) Fetch(ctx context.Context, h searchHit) (*article, error) {
	base := k.base()
	if base == "" {
		return nil, errNoKiwix
	}
	req, _ := http.NewRequestWithContext(ctx, "GET", base+"/raw/"+url.PathEscape(h.Book)+"/content/"+h.Path, nil)
	resp, err := k.client.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != 200 {
		return nil, fmt.Errorf("kiwix article: %s", resp.Status)
	}
	body, err := io.ReadAll(io.LimitReader(resp.Body, 12<<20))
	if err != nil {
		return nil, err
	}
	return parseArticle(h.Title, h.Source, string(body)), nil
}

// splitContentLink turns /kiwix/content/BOOK/PATH into BOOK and PATH.
func splitContentLink(link string) (book, path string, ok bool) {
	i := strings.Index(link, "/content/")
	if i < 0 {
		return "", "", false
	}
	rest := link[i+len("/content/"):]
	j := strings.IndexByte(rest, '/')
	if j <= 0 || j == len(rest)-1 {
		return "", "", false
	}
	book, _ = url.PathUnescape(rest[:j])
	return book, rest[j+1:], true
}

// sourceLabel is the short tag shown after each result, like [Wiki] or [Med].
func sourceLabel(book, bookTitle string) string {
	n := strings.ToLower(book)
	known := []struct{ prefix, label string }{
		{"wikipedia_en_medicine", "Med"}, {"mdwiki", "Med"}, {"wikem", "Med"},
		{"wikipedia", "Wiki"}, {"gutenberg", "Book"}, {"ifixit", "iFixit"},
		{"wikibooks", "Wikibooks"}, {"wikivoyage", "Travel"}, {"wikihow", "wikiHow"},
		{"wiktionary", "Dict"}, {"wikiversity", "Course"}, {"wikisource", "Source"},
		{"zimgit-post-disaster", "Survival"}, {"zimgit-water", "Water"}, {"zimgit-medicine", "Med"},
		{"zimgit-food-preparation", "Food"}, {"zimgit-knots", "Knots"}, {"usda", "USDA"},
	}
	for _, k := range known {
		if strings.HasPrefix(n, k.prefix) {
			return k.label
		}
	}
	// other book collections: "zimgit-first-aid_en_2024" -> "First"
	if rest, ok := strings.CutPrefix(n, "zimgit-"); ok {
		if w := strings.FieldsFunc(rest, func(r rune) bool { return r == '-' || r == '_' }); len(w) > 0 {
			return truncBytes(strings.ToUpper(w[0][:1])+w[0][1:], 12)
		}
	}
	t := strings.TrimSpace(bookTitle)
	if t == "" {
		t = book
	}
	if f := strings.Fields(t); len(f) > 0 {
		t = f[0]
	}
	return truncBytes(t, 12)
}

func stripTags(s string) string {
	var b strings.Builder
	in := false
	for _, r := range s {
		switch {
		case r == '<':
			in = true
		case r == '>':
			in = false
		case !in:
			b.WriteRune(r)
		}
	}
	return b.String()
}
