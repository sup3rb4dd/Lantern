package main

// Text from PDFs, using pdftotext (the Xpdf command-line tool on Windows,
// downloaded on request into lantern/tools; the system's pdftotext elsewhere).

import (
	"context"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"runtime"
	"strings"
	"time"
	"unicode"
	"unicode/utf8"
)

func pdftotextPath(p Paths) string {
	name := "pdftotext"
	if runtime.GOOS == "windows" {
		name += ".exe"
	}
	local := filepath.Join(p.Tools, name)
	if _, err := os.Stat(local); err == nil {
		return local
	}
	if runtime.GOOS != "windows" {
		if found, err := exec.LookPath("pdftotext"); err == nil {
			return found
		}
	}
	return ""
}

var errNoPdfTool = errors.New("the PDF reader isn't installed")

// pdfPages extracts the raw text of each page of a PDF file. It is saved as
// is; preparePdfPages turns it into readable text when the index loads, so
// formatting fixes never need the PDFs read again.
func pdfPages(ctx context.Context, tool, pdfFile string) ([]string, error) {
	if tool == "" {
		return nil, errNoPdfTool
	}
	out := pdfFile + ".txt"
	defer os.Remove(out)
	ctx, cancel := context.WithTimeout(ctx, 2*time.Minute)
	defer cancel()
	cmd := exec.CommandContext(ctx, tool, "-enc", "UTF-8", pdfFile, out)
	hideWindow(cmd)
	if b, err := cmd.CombinedOutput(); err != nil {
		msg := strings.TrimSpace(string(b))
		if msg == "" {
			msg = err.Error()
		}
		return nil, errors.New("pdftotext: " + msg)
	}
	b, err := os.ReadFile(out)
	if err != nil {
		return nil, err
	}
	raw := strings.Split(string(b), "\f")
	if len(raw) > 0 && strings.TrimSpace(raw[len(raw)-1]) == "" {
		raw = raw[:len(raw)-1] // text ends with a page break
	}
	for i := range raw {
		raw[i] = strings.ReplaceAll(raw[i], "\r", "")
	}
	return raw, nil
}

// preparePdfPages turns raw page text into readable pages, plus a short title
// for each page (its first line of text).
func preparePdfPages(raw []string) (pages, titles []string) {
	raw = dropRepeatedLines(raw)
	pages = make([]string, len(raw))
	titles = make([]string, len(raw))
	for i, r := range raw {
		pages[i] = tidyPdfText(r)
		titles[i] = pageTitle(r)
	}
	return pages, titles
}

var (
	reDigits = regexp.MustCompile(`[0-9]+`)
	rePageOf = regexp.MustCompile(`(?i)^(page\s*)?#(\s*(of|/)\s*#)?$`)
	// a list item: bullet, "1." / "2)" or "a." / "b)" at the start of a line
	reListItem       = regexp.MustCompile(`^([•●▪■◦‣∙·○◆➢➤✓✔*–-]|\(?[0-9]{1,2}[.)]|\(?[a-zA-Z][.)])\s`)
	reInlineBullet   = regexp.MustCompile(`[ \t]+[•●▪■◦‣][ \t]+`)
	reTrailingBullet = regexp.MustCompile(`(?m)[ \t]+[•●▪■◦‣]$`)
	reBulletOnly     = regexp.MustCompile(`^[•●▪■◦‣∙·○◆➢➤✓✔*–-]$`)
	reHyphenBreak    = regexp.MustCompile(`(\p{L})-\n(\p{Ll})`)
	reBlankLines     = regexp.MustCompile(`\n[ \t]*\n+`)
)

// dropRepeatedLines removes running headers and footers: lines (ignoring
// numbers) that appear on at least half the pages, and bare page numbers.
func dropRepeatedLines(pages []string) []string {
	norm := func(l string) string { return reDigits.ReplaceAllString(strings.ToLower(strings.TrimSpace(l)), "#") }
	count := map[string]int{}
	for _, p := range pages {
		seen := map[string]bool{}
		for _, l := range strings.Split(p, "\n") {
			if n := norm(l); n != "" && !seen[n] {
				seen[n] = true
				count[n]++
			}
		}
	}
	limit := (len(pages) + 2) / 3 // a third: books alternate headers on left and right pages
	if limit < 2 {
		limit = 2
	}
	out := make([]string, len(pages))
	for i, p := range pages {
		var keep []string
		for _, l := range strings.Split(p, "\n") {
			n := norm(l)
			if n != "" && (count[n] >= limit || rePageOf.MatchString(n)) {
				continue
			}
			keep = append(keep, l)
		}
		out[i] = strings.Join(keep, "\n")
	}
	return out
}

// tidyPdfText joins the hard-wrapped lines of a page back into paragraphs.
func tidyPdfText(s string) string {
	s = strings.ReplaceAll(s, "\r", "")
	s = strings.ReplaceAll(s, "\uFFFD", " ")      // characters the PDF's fonts couldn't map
	s = reHyphenBreak.ReplaceAllString(s, "$1$2") // "disin-\nfect" -> "disinfect"
	// Some PDFs (like Wikibooks' printable books) put a bullet at the end of the
	// previous line or in the middle of a line: give each bullet its own line.
	s = reInlineBullet.ReplaceAllString(s, "\n• ")
	s = reTrailingBullet.ReplaceAllString(s, "\n•")
	paras := reBlankLines.Split(s, -1)
	var out []string
	var b strings.Builder
	flush := func() {
		if t := strings.TrimSpace(reSpace.ReplaceAllString(b.String(), " ")); t != "" {
			out = append(out, t)
		}
		b.Reset()
	}
	for _, p := range paras {
		bullet := false // a bullet on its own line: its text is on the next line
		inItem := false // the current line is a list item
		for _, l := range strings.Split(p, "\n") {
			l = strings.TrimSpace(l)
			if l == "" {
				continue
			}
			switch {
			case reBulletOnly.MatchString(l):
				flush()
				b.WriteString("• ")
				bullet, inItem = true, true
				continue
			case bullet:
				b.WriteString(l)
				bullet = false
				continue
			case reListItem.MatchString(l):
				flush() // each list item starts its own line
				inItem = true
				if r, size := utf8.DecodeRuneInString(l); !unicode.IsLetter(r) && !unicode.IsDigit(r) && r != '(' {
					l = "• " + strings.TrimSpace(l[size:]) // one bullet style everywhere
				}
				b.WriteString(l)
				continue
			case inItem && endsSentence(b.String()) && startsUpper(l):
				flush() // the list is over: a new paragraph begins
				inItem = false
			}
			if b.Len() > 0 {
				b.WriteByte(' ')
			}
			b.WriteString(l)
		}
		flush()
	}
	return strings.Join(out, "\n")
}

func endsSentence(s string) bool {
	s = strings.TrimSpace(s)
	return strings.HasSuffix(s, ".") || strings.HasSuffix(s, "!") || strings.HasSuffix(s, "?") || strings.HasSuffix(s, ":")
}

func startsUpper(s string) bool {
	r, _ := utf8.DecodeRuneInString(s)
	return unicode.IsUpper(r)
}

// pageTitle is a short label for a page: its first line of real text
// (from the raw text, before lines are joined into paragraphs).
func pageTitle(page string) string {
	page = strings.ReplaceAll(page, "\uFFFD", " ")
	for _, l := range strings.Split(page, "\n") {
		l = reSpace.ReplaceAllString(l, " ")
		l = strings.TrimSpace(l)
		letters := 0
		for _, r := range l {
			if unicode.IsLetter(r) {
				letters++
			}
		}
		if letters >= 3 {
			if i := strings.IndexAny(l, ".:;"); i > 12 {
				l = l[:i]
			}
			return truncBytes(l, 44)
		}
	}
	return ""
}

// looksScanned reports whether a PDF has almost no text (pages are pictures).
func looksScanned(pages []string) bool {
	letters := 0
	for _, p := range pages {
		for _, r := range p {
			if unicode.IsLetter(r) {
				letters++
			}
		}
	}
	return len(pages) == 0 || letters < 40*len(pages)
}
