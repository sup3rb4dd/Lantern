package main

// Turns a Kiwix article (HTML) into plain-text sections, and splits text into
// mesh-sized messages. No HTML library: a small tag scanner is enough for
// Kiwix content, and it keeps Lantern free of dependencies.

import (
	"html"
	"regexp"
	"strings"
	"unicode/utf8"
)

type section struct {
	Title string
	Text  string
}

type article struct {
	Title    string
	Source   string // short label like Wiki, Med, Book
	Sections []section
}

// Elements whose contents never help a short radio answer.
var skipTags = map[string]bool{
	"script": true, "style": true, "table": true, "figure": true, "noscript": true,
	"math": true, "svg": true, "nav": true, "footer": true, "header": false, "audio": true,
	"video": true, "img": false, "figcaption": true, "button": true, "form": true, "head": true,
}

// Class names that mark boxes, footnotes and page furniture. A class on an
// element matches if it equals one of these or starts with it plus "-"
// (so "navbox" also catches "navbox-inner"). Whole names only: Wikipedia's
// <html> tag carries classes like "vector-toc-not-available".
var skipClasses = []string{"infobox", "navbox", "hatnote", "reference", "references", "mw-ref",
	"mw-references-wrap", "reflist", "thumb", "metadata", "mw-editsection", "noprint", "sidebar",
	"ambox", "mbox", "toc", "gallery", "mw-empty-elt", "shortdescription", "coordinates",
	"printfooter", "catlinks", "mw-normal-catlinks", "mw-hidden-catlinks", "zim-footer",
	"mw-cite-backlink", "navigation-not-searchable", "vertical-navbox", "side-box", "portalbox"}

var voidTags = map[string]bool{"br": true, "img": true, "hr": true, "meta": true, "link": true,
	"input": true, "wbr": true, "source": true, "area": true, "base": true, "col": true, "embed": true}

var blockTags = map[string]bool{"p": true, "div": true, "li": true, "ul": true, "ol": true,
	"section": true, "details": true, "summary": true, "blockquote": true, "dd": true, "dt": true,
	"dl": true, "pre": true, "tr": true, "body": true, "article": true, "main": true}

// Sections that are lists of links or citations rather than content.
var dropSections = map[string]bool{"references": true, "notes": true, "see also": true,
	"external links": true, "further reading": true, "bibliography": true, "sources": true,
	"citations": true, "footnotes": true, "notes and references": true, "works cited": true,
	"explanatory notes": true, "general and cited references": true, "gallery": true}

var (
	reClass = regexp.MustCompile(`(?i)\bclass\s*=\s*("[^"]*"|'[^']*'|[^\s>]+)`)
	reSpace = regexp.MustCompile(`[ \t\r\f\v\x{00a0}]+`)
	reLines = regexp.MustCompile(`\n\s*\n+`)
	reBrack = regexp.MustCompile(`\[(\d+|[a-z]|citation needed|note \d+|edit)\]`)
)

type rawHeading struct {
	level int
	title string
	pos   int // byte offset in the text where the heading starts
}

// parseArticle converts article HTML into sections. Level-2 headings start
// sections; if there are fewer than two of them (common in books), level 3 is used.
func parseArticle(title, source, htmlText string) *article {
	text, heads := htmlToText(htmlText)
	level := 2
	n2, n3 := 0, 0
	for _, h := range heads {
		if h.level == 2 {
			n2++
		} else if h.level == 3 {
			n3++
		}
	}
	if n2 < 2 && n3 >= 2 {
		level = 3
	}
	a := &article{Title: title, Source: source}
	start, curTitle := 0, "Summary"
	flush := func(end int) {
		body := cleanText(text[start:end])
		body = strings.TrimSpace(strings.TrimPrefix(body, "— "+curTitle+" —"))
		if dropSections[strings.ToLower(curTitle)] {
			return
		}
		if body == "" && curTitle != "Summary" {
			return
		}
		a.Sections = append(a.Sections, section{Title: curTitle, Text: body})
	}
	for _, h := range heads {
		if h.level < level {
			continue
		}
		if h.level > level {
			continue // smaller headings stay inline as a text line
		}
		flush(h.pos)
		start, curTitle = h.pos, h.title
	}
	flush(len(text))
	if len(a.Sections) > 0 && a.Sections[0].Title == "Summary" && a.Sections[0].Text == "" {
		a.Sections = a.Sections[1:]
	}
	if len(a.Sections) == 0 {
		a.Sections = []section{{Title: "Summary", Text: "(This page has no readable text.)"}}
	}
	return a
}

func htmlToText(s string) (string, []rawHeading) {
	var out strings.Builder
	var heads []rawHeading
	skipTag, skipDepth := "", 0
	headLevel, headStart := 0, 0
	headText := strings.Builder{}

	for i := 0; i < len(s); {
		lt := strings.IndexByte(s[i:], '<')
		if lt < 0 {
			lt = len(s) - i
		}
		if lt > 0 {
			if skipDepth == 0 {
				chunk := s[i : i+lt]
				if headLevel > 0 {
					headText.WriteString(chunk)
				} else {
					out.WriteString(chunk)
				}
			}
			i += lt
			continue
		}
		// at '<'
		if strings.HasPrefix(s[i:], "<!--") {
			end := strings.Index(s[i:], "-->")
			if end < 0 {
				break
			}
			i += end + 3
			continue
		}
		gt := strings.IndexByte(s[i:], '>')
		if gt < 0 {
			break
		}
		tag := s[i+1 : i+gt]
		i += gt + 1
		if tag == "" || tag[0] == '!' || tag[0] == '?' {
			continue
		}
		closing := tag[0] == '/'
		if closing {
			tag = tag[1:]
		}
		selfClose := strings.HasSuffix(tag, "/")
		name := tag
		if j := strings.IndexAny(name, " \t\n\r/"); j >= 0 {
			name = name[:j]
		}
		name = strings.ToLower(name)
		if name == "" {
			continue
		}

		if skipDepth > 0 {
			if name == skipTag && !voidTags[name] && !selfClose {
				if closing {
					skipDepth--
				} else {
					skipDepth++
				}
			}
			continue
		}
		structural := name == "html" || name == "body" || name == "main"
		if !closing && !voidTags[name] && !selfClose && !structural && (skipTags[name] || hasSkipClass(tag)) {
			skipTag, skipDepth = name, 1
			continue
		}

		if len(name) == 2 && name[0] == 'h' && name[1] >= '1' && name[1] <= '6' {
			lvl := int(name[1] - '0')
			if !closing {
				headLevel, headStart = lvl, out.Len()
				headText.Reset()
			} else if headLevel > 0 {
				t := cleanInline(headText.String())
				out.WriteString("\n")
				if headLevel >= 2 && t != "" {
					heads = append(heads, rawHeading{level: headLevel, title: t, pos: headStart})
					if headLevel > 2 {
						out.WriteString("— " + t + " —\n")
					}
				}
				headLevel = 0
			}
			continue
		}
		switch {
		case name == "br":
			out.WriteString("\n")
		case name == "li" && !closing:
			out.WriteString("\n• ")
		case blockTags[name]:
			out.WriteString("\n")
		case name == "td" || name == "th":
			out.WriteString(" ")
		}
	}
	return out.String(), heads
}

func hasSkipClass(tag string) bool {
	m := reClass.FindStringSubmatch(tag)
	if m == nil {
		return false
	}
	for _, c := range strings.Fields(strings.ToLower(strings.Trim(m[1], `"'`))) {
		for _, k := range skipClasses {
			if c == k || strings.HasPrefix(c, k+"-") {
				return true
			}
		}
	}
	return false
}

func cleanInline(s string) string {
	s = html.UnescapeString(s)
	s = reBrack.ReplaceAllString(s, "")
	return strings.TrimSpace(reSpace.ReplaceAllString(strings.ReplaceAll(s, "\n", " "), " "))
}

func cleanText(s string) string {
	s = html.UnescapeString(s)
	s = reBrack.ReplaceAllString(s, "")
	s = reSpace.ReplaceAllString(s, " ")
	lines := strings.Split(s, "\n")
	keep := lines[:0]
	for _, l := range lines {
		l = strings.TrimSpace(l)
		if l == "" || l == "•" {
			keep = append(keep, "")
			continue
		}
		keep = append(keep, l)
	}
	s = strings.Join(keep, "\n")
	s = reLines.ReplaceAllString(s, "\n")
	s = strings.ReplaceAll(s, " ,", ",")
	s = strings.ReplaceAll(s, " .", ".")
	return strings.TrimSpace(s)
}

// ---------- splitting text into messages ----------

// chunkText splits text into pieces of at most max bytes, breaking between
// words (or inside a word only if one word is longer than max). Never splits a
// UTF-8 character.
func chunkText(text string, max int) []string {
	if max < 20 {
		max = 20
	}
	var out []string
	text = strings.TrimSpace(text)
	for len(text) > 0 {
		if len(text) <= max {
			out = append(out, text)
			break
		}
		cut := -1
		// prefer a paragraph break, then a sentence end (in the back half), then a space
		window := text[:max]
		minCut := max / 2
		j := strings.LastIndex(window, "\n")
		for j > 0 && strings.HasSuffix(window[:j], " —") { // keep a subheading with its paragraph
			j = strings.LastIndex(window[:j], "\n")
		}
		if j >= minCut {
			cut = j
		} else if k := lastSentenceEnd(window); k >= minCut {
			cut = k
		} else if j > max/4 {
			cut = j // an earlier paragraph break beats splitting a sentence
		} else if k := strings.LastIndexAny(window, " \n"); k > 0 {
			cut = k
		}
		if cut <= 0 {
			cut = max
			for cut > 0 && !utf8.RuneStart(text[cut]) {
				cut--
			}
		}
		out = append(out, strings.TrimSpace(text[:cut]))
		text = strings.TrimSpace(text[cut:])
	}
	return out
}

func lastSentenceEnd(s string) int {
	best := -1
	for _, p := range []string{". ", "! ", "? ", "; "} {
		for end := len(s); end > 0; {
			j := strings.LastIndex(s[:end], p)
			if j < 0 {
				break
			}
			if p != ". " || !endsWithAbbrev(s[:j]) {
				if j+1 > best {
					best = j + 1
				}
				break
			}
			end = j
		}
	}
	return best
}

// endsWithAbbrev reports whether the text before a period is an abbreviation
// like "e.g" or "Dr", so the period doesn't end a sentence.
func endsWithAbbrev(before string) bool {
	w := before
	if i := strings.LastIndexAny(before, " (\n"); i >= 0 {
		w = before[i+1:]
	}
	switch strings.ToLower(w) {
	case "e.g", "i.e", "etc", "vs", "approx", "dr", "mr", "mrs", "ms", "st", "no", "fig", "ca", "c", "jr", "sr", "u.s", "mt":
		return true
	}
	return len(w) == 1 // an initial, like "J. Smith"
}

// truncBytes shortens s to at most n bytes, adding "…" when it cuts.
func truncBytes(s string, n int) string {
	if len(s) <= n {
		return s
	}
	n -= len("…")
	for n > 0 && !utf8.RuneStart(s[n]) {
		n--
	}
	return strings.TrimSpace(s[:n]) + "…"
}
