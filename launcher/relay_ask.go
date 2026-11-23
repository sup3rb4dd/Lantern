package main

// ?ask: the AI acts as a librarian. It turns a plain question into searches,
// picks the best pages from Lantern's own library, and (if turned on) writes a
// short answer using only the top page. Without the AI, ?ask falls back to a
// keyword search, so it always returns something.

import (
	"context"
	"fmt"
	"regexp"
	"strings"
	"time"
)

const (
	aiModeOff     = "off"
	aiModeSources = "sources" // AI picks pages; no written answer
	aiModeAnswer  = "answer"  // short answer + pages
)

var askStop = map[string]bool{}

func init() {
	for _, w := range strings.Fields(`a an the i im i'm me my we our you your it its is are was were be been am do does did
		have has had can could would should will shall may might must how what when where why who which whom whose
		to of in on at for from by with about into over under up out off and or but if so than then that this these those
		there here just very really please tell show give walk through need want get got some any thing things way ways
		lantern help know like make sure ok okay hey hi much many lot bit`) {
		askStop[w] = true
	}
}

var fromRe = regexp.MustCompile(`\(from \d\)\s*$`)

var askWordRe = regexp.MustCompile(`[\p{L}\p{N}'-]+`)

// askKeywords strips filler words so a plain question still makes a decent search.
func askKeywords(q string) string {
	var out []string
	for _, w := range askWordRe.FindAllString(strings.ToLower(q), -1) {
		w = strings.Trim(w, "'-")
		if len(w) > 1 && !askStop[w] {
			out = append(out, w)
		}
	}
	if len(out) == 0 {
		return strings.TrimSpace(q)
	}
	if len(out) > 6 {
		out = out[:6]
	}
	return strings.Join(out, " ")
}

const askSearchPrompt = `You turn a question from someone in the field into searches for an offline library: Wikipedia, WikiMed (a medical encyclopedia), and practical books (first aid, survival, water, food, repair, radio).
Return 1 to 3 searches of 2 to 4 words each, using the terms an encyclopedia or manual would use.
Example: "I have a small cut, walk me through stitching it up" -> ["wound closure", "laceration", "first aid wounds"].
Example: "water tastes weird from the creek, can I drink it" -> ["water purification", "drinking water treatment", "waterborne disease"].`

const askPickPrompt = `You are a librarian. Pick the results that will help this person most, best first, at most 3.
For "how do I" questions prefer practical guides and first-aid or how-to pages over history or background.
Only use result numbers from the list.`

const askAnswerPrompt = `You are Lantern, an offline field reference read over a slow radio link.
Answer using ONLY the numbered sources given. Plain text, no markdown, at most 3 short sentences or numbered steps, under 350 characters.
Use whichever sources help; combine them if needed. End with the number of the source you used most, like "(from 2)".
The person is off-grid: there is no phone service, no emergency services and no doctor to reach. Never tell them to call for help, call 911, go to a hospital or see a doctor. Give the most useful steps they can do themselves with what's at hand.
Medical or safety questions: if the situation is serious (heavy bleeding, deep or gaping wounds, trouble breathing, severe allergic reaction, signs of infection), say so plainly in a few words, give the most important steps first, and name the warning signs that it is getting worse.
If the sources only partly answer, give the parts they do cover. Only if none of them is about the question at all, say "Not covered here" and name the closest source.
Never make up steps, doses or facts that are not in the source.`

var (
	askSearchSchema = map[string]any{
		"type": "object",
		"properties": map[string]any{
			"searches": map[string]any{"type": "array", "items": map[string]any{"type": "string"}, "minItems": 1, "maxItems": 3},
		},
		"required": []string{"searches"}, "additionalProperties": false,
	}
	askPickSchema = map[string]any{
		"type": "object",
		"properties": map[string]any{
			"best": map[string]any{"type": "array", "items": map[string]any{"type": "integer"}, "minItems": 1, "maxItems": 3},
		},
		"required": []string{"best"}, "additionalProperties": false,
	}
)

func (r *Relay) aiReady(cfg relayConfig) bool {
	return r.ai != nil && cfg.AI != aiModeOff && r.ai.Installed()
}

// ask answers ?ask <question>.
func (r *Relay) ask(s *relaySession, cfg relayConfig, q string) ([]string, string) {
	ctx, cancel := context.WithTimeout(context.Background(), 6*time.Minute)
	defer cancel()
	kw := askKeywords(q)
	header := "🔎 " + truncBytes(q, 40)

	if !r.aiReady(cfg) {
		s.query, s.results, s.total, s.pool = kw, nil, 0, nil
		msgs, info := r.searchPage(ctx, s, cfg)
		return msgs, "ask without AI → search " + kw + ": " + info
	}
	if st := r.ai.Status(); st.State != "ready" && st.External == "" && s.notify != nil {
		s.notify("Lantern: starting the AI, this can take a minute…")
	}

	// 1. Searches in library words.
	var sq struct {
		Searches []string `json:"searches"`
	}
	searches := []string{}
	aiErr := r.ai.ChatJSON(ctx, askSearchPrompt, q, askSearchSchema, 80, &sq)
	if aiErr == nil {
		for _, x := range sq.Searches {
			if x = strings.TrimSpace(x); x != "" && len(searches) < 3 {
				searches = append(searches, x)
			}
		}
	}
	searches = append(searches, kw)

	// 2. Gather candidates from every search.
	var cands []searchHit
	seen := map[string]bool{}
	for _, sq := range searches {
		hits, _, err := r.search(ctx, sq, 0, 5)
		if err != nil {
			continue
		}
		for _, h := range hits {
			key := fmt.Sprintf("%s|%s|%d", h.Book, h.Path, h.page)
			if !seen[key] && len(cands) < 12 {
				seen[key] = true
				cands = append(cands, h)
			}
		}
	}
	if len(cands) == 0 {
		s.query, s.results, s.total, s.pool = kw, nil, 0, nil
		s.mode = modeSearch
		return []string{header + "\nNo matches. Try other words."}, "ask: 0 results"
	}
	info := "ask searched " + strings.Join(searches, " / ")
	if aiErr != nil {
		info += " (AI: " + aiErr.Error() + ")"
	}

	// 3. Let the AI pick the most useful ones.
	order := make([]int, 0, len(cands))
	if aiErr == nil {
		var pick struct {
			Best []int `json:"best"`
		}
		var b strings.Builder
		fmt.Fprintf(&b, "Question: %s\n\nResults:\n", q)
		for i, h := range cands {
			fmt.Fprintf(&b, "%d. %s\n", i+1, candidateLine(h, searches))
		}
		if err := r.ai.ChatJSON(ctx, askPickPrompt, b.String(), askPickSchema, 40, &pick); err == nil {
			used := map[int]bool{}
			for _, n := range pick.Best {
				if n >= 1 && n <= len(cands) && !used[n-1] {
					used[n-1] = true
					order = append(order, n-1)
				}
			}
		}
	}
	picked := map[int]bool{}
	for _, i := range order {
		picked[i] = true
	}
	for i := range cands {
		if !picked[i] {
			order = append(order, i)
		}
	}
	pool := make([]searchHit, len(order))
	for i, n := range order {
		pool[i] = cands[n]
	}

	// 4. Short answer from the best pages (the AI's top picks, numbered as the reply lists them).
	var msgs []string
	if cfg.AI == aiModeAnswer && aiErr == nil {
		terms := append(append([]string{}, searches...), q)
		var b strings.Builder
		fmt.Fprintf(&b, "Question: %s\n", q)
		used := 0
		for i := 0; i < len(pool) && i < 3; i++ {
			if src := r.sourceText(ctx, pool[i], terms, 2400); src != "" {
				fmt.Fprintf(&b, "\nSource %d: %s [%s]\n%s\n", i+1, pool[i].Title, pool[i].Source, src)
				used++
			}
		}
		if used > 0 {
			ans, err := r.ai.Chat(ctx, askAnswerPrompt, b.String(), nil, 200)
			if err == nil && ans != "" {
				ans = tidyAnswer(ans, 400)
				if !fromRe.MatchString(ans) {
					ans += " (from 1)"
				}
				for _, m := range chunkText("🤖 "+ans, cfg.ChunkBytes-2) {
					msgs = append(msgs, m)
				}
				info += "; answered"
			} else if err != nil {
				info += "; answer failed: " + err.Error()
			}
		}
	}

	s.query, s.results, s.total, s.pool = q, nil, len(pool), pool
	src, sinfo := r.searchPage(ctx, s, cfg)
	return append(msgs, src...), info + "; " + sinfo
}

// candidateLine is one result as the AI sees it while picking.
func candidateLine(h searchHit, terms []string) string {
	line := h.Title + " (" + h.Source
	if h.pdf != nil {
		line += fmt.Sprintf(" book, page %d", h.page+1)
	}
	line += ")"
	snip := h.Snip
	if h.pdf != nil && h.page < len(h.pdf.Pages) {
		snip = excerpt(h.pdf.Pages[h.page], terms, 220)
	}
	if snip != "" {
		line += ": " + truncBytes(strings.Join(strings.Fields(snip), " "), 240)
	}
	return line
}

// excerpt returns about n bytes of text around the first search word found.
func excerpt(text string, terms []string, n int) string {
	low := strings.ToLower(text)
	at := -1
	for _, t := range terms {
		for _, w := range strings.Fields(strings.ToLower(t)) {
			if len(w) < 3 {
				continue
			}
			if i := strings.Index(low, w); i >= 0 && (at < 0 || i < at) {
				at = i
			}
		}
	}
	if at < 0 {
		at = 0
	}
	start := at - n/3
	if start < 0 {
		start = 0
	}
	for start > 0 && start < len(text) && text[start]&0xC0 == 0x80 {
		start--
	}
	return truncBytes(text[start:], n)
}

// sourceText is the part of a result most likely to answer the question.
func (r *Relay) sourceText(ctx context.Context, h searchHit, terms []string, max int) string {
	if h.pdf != nil {
		t := ""
		for p := h.page; p < len(h.pdf.Pages) && p <= h.page+1; p++ {
			t += h.pdf.Pages[p] + "\n"
		}
		return truncBytes(strings.TrimSpace(t), max)
	}
	a, err := r.ks.Fetch(ctx, h)
	if err != nil || len(a.Sections) == 0 {
		return truncBytes(h.Snip, max)
	}
	words := map[string]bool{}
	for _, t := range terms {
		for _, w := range strings.Fields(strings.ToLower(t)) {
			if w = strings.Trim(w, ".,?!;:\"()"); len(w) > 2 && !askStop[w] {
				words[w] = true
			}
		}
	}
	// The introduction, then the sections that mention the search words most.
	type scored struct {
		i     int
		score int
	}
	var ss []scored
	for i, sec := range a.Sections {
		if i == 0 {
			continue
		}
		low := strings.ToLower(sec.Title + " " + sec.Text)
		n := 0
		for w := range words {
			n += strings.Count(low, w)
			if strings.Contains(strings.ToLower(sec.Title), w) {
				n += 5
			}
		}
		if n > 0 {
			ss = append(ss, scored{i, n})
		}
	}
	for i := 1; i < len(ss); i++ {
		for j := i; j > 0 && ss[j].score > ss[j-1].score; j-- {
			ss[j], ss[j-1] = ss[j-1], ss[j]
		}
	}
	out := truncBytes(a.Sections[0].Text, max/3)
	for _, x := range ss {
		if len(out) >= max {
			break
		}
		out += "\n\n" + a.Sections[x.i].Title + ": " + a.Sections[x.i].Text
	}
	return truncBytes(out, max)
}

var (
	sentenceRe    = regexp.MustCompile(`\s*(?:\d{1,2}[.)]\s*)?[^.!?]+[.!?]*`)
	callForHelpRe = regexp.MustCompile(`(?i)\b(call|dial|phone|contact)\b[^.!?]*\b(911|999|112|emergency|ambulance|ems|doctor|help|poison control)\b|\b(seek|get)\b[^.!?]*\b(medical|professional|emergency)\b[^.!?]*\b(attention|help|care|treatment)\b|\b(go to|visit)\b[^.!?]*\b(hospital|emergency room|er|clinic|urgent care)\b|\b(see|consult)\b[^.!?]*\b(a|your)\b[^.!?]*\b(doctor|physician|healthcare|professional)\b`)
)

// dropCallForHelp removes "call 911 / see a doctor" sentences, which small
// models add out of habit even when told there's no one to call.
func dropCallForHelp(s string) string {
	var keep []string
	for _, sen := range sentenceRe.FindAllString(s, -1) {
		if t := strings.TrimSpace(sen); t != "" && !callForHelpRe.MatchString(t) {
			keep = append(keep, t)
		}
	}
	if len(keep) == 0 {
		return s
	}
	return strings.Join(keep, " ")
}

// tidyAnswer flattens markdown and keeps whole sentences within max bytes.
func tidyAnswer(s string, max int) string {
	s = strings.NewReplacer("**", "", "__", "", "`", "", "#", "").Replace(s)
	lines := strings.Split(s, "\n")
	for i := range lines {
		l := strings.TrimSpace(lines[i])
		l = strings.TrimPrefix(l, "- ")
		l = strings.TrimPrefix(l, "* ")
		lines[i] = l
	}
	s = strings.Join(strings.Fields(strings.Join(lines, " ")), " ")
	s = dropCallForHelp(s)
	if len(s) <= max {
		return s
	}
	cut := truncBytes(s, max)
	if i := strings.LastIndexAny(cut, ".!?"); i > max/2 {
		return cut[:i+1]
	}
	return strings.TrimSuffix(cut, "…") + "…"
}
