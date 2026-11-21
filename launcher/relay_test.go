package main

import (
	"context"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path"
	"path/filepath"
	"strings"
	"testing"
	"time"
	"unicode/utf8"
)

// The files in testdata/relay were captured from a real kiwix-serve 3.5
// serving two small test ZIMs (a Wikipedia-style page set and a survival guide).

const (
	wikiBook = "wikipedia_en_test_maxi_2026-11"
	survBook = "zimgit-water_en_2026-11"
	gwNode   = 0x0000abcd // the gateway node
	teamNode = 0x11111111 // allowed
	strNode  = 0x22222222 // not allowed
)

func fakeKiwix(t *testing.T) *httptest.Server {
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		dir := "testdata/relay"
		switch {
		case r.URL.Path == "/kiwix/search":
			name := "search_none.xml"
			switch strings.ToLower(r.URL.Query().Get("pattern")) {
			case "water":
				name = "search_water.xml"
			case "oklahoma city":
				name = "search_oklahoma_city.xml"
			}
			http.ServeFile(w, r, filepath.Join(dir, name))
		case strings.HasPrefix(r.URL.Path, "/kiwix/raw/zimgit-water_en_2026-11/content/files/"):
			http.ServeFile(w, r, filepath.Join(dir, "pdf", path.Base(r.URL.Path)))
		case strings.HasPrefix(r.URL.Path, "/kiwix/raw/"):
			rest := strings.TrimPrefix(r.URL.Path, "/kiwix/raw/")
			book, path, _ := strings.Cut(rest, "/content/")
			http.ServeFile(w, r, filepath.Join(dir, book+"_"+path+".html"))
		default:
			http.NotFound(w, r)
		}
	}))
}

// fakeNode plays the part of the Heltec gateway node on the other end of the USB cable.
type fakeNode struct {
	t    *testing.T
	mc   *meshConn
	sent chan sentMsg // text messages Relay transmitted
}

type sentMsg struct {
	to   uint32
	text string
}

func fromRadio(field int, m *pbWriter) []byte { return (&pbWriter{}).message(field, m).b }

func (n *fakeNode) run() {
	for {
		f, err := n.mc.readFrame()
		if err != nil {
			return
		}
		r := pbReader{b: f}
		for r.next() {
			switch r.field {
			case 3: // want_config: send my info, the node list, then done
				n.mc.send(fromRadio(3, (&pbWriter{}).varint(1, gwNode)))
				for _, nd := range []struct {
					num  uint32
					name string
				}{{gwNode, "Lantern Relay 🤖"}, {teamNode, "Elijah"}, {strNode, "Stranger"}} {
					user := (&pbWriter{}).bytes(1, []byte(nodeID(nd.num))).bytes(2, []byte(nd.name))
					n.mc.send(fromRadio(4, (&pbWriter{}).varint(1, uint64(nd.num)).message(2, user)))
				}
				n.mc.send((&pbWriter{}).varint(7, r.val).b)
			case 1: // an outgoing packet
				var to, id uint32
				var text string
				p := pbReader{b: r.data}
				for p.next() {
					switch p.field {
					case 2:
						to = p.u32()
					case 6:
						id = p.u32()
					case 4:
						d := pbReader{b: p.data}
						for d.next() {
							if d.field == 2 {
								text = string(d.data)
							}
						}
					}
				}
				n.sent <- sentMsg{to, text}
				// the destination acknowledges it
				data := (&pbWriter{}).varint(1, portRouting).bytes(2, nil).fixed32(6, id)
				mp := (&pbWriter{}).fixed32(1, to).fixed32(2, gwNode).message(4, data)
				n.mc.send(fromRadio(2, mp))
			}
		}
	}
}

func (n *fakeNode) receive(from uint32, text string) {
	data := (&pbWriter{}).varint(1, portText).bytes(2, []byte(text))
	mp := (&pbWriter{}).fixed32(1, from).fixed32(2, gwNode).message(4, data).fixed32(6, randomPacketID())
	n.mc.send(fromRadio(2, mp))
}

// replies collects messages until none arrive for a moment.
func (n *fakeNode) replies() []string {
	var out []string
	for {
		select {
		case m := <-n.sent:
			out = append(out, m.text)
		case <-time.After(400 * time.Millisecond):
			return out
		}
	}
}

func startRelay(t *testing.T, base string, books []string) (*Relay, *fakeNode) {
	relaySide, nodeSide := net.Pipe()
	ks := &kiwixSearcher{base: func() string { return base }, books: func() []string { return books }, client: http.DefaultClient}
	r := newRelayWith(ks, filepath.Join(t.TempDir(), "relay.json"))
	r.sleep = func(time.Duration) {}
	r.openFn = func(string) (io.ReadWriteCloser, string, error) { return relaySide, "test", nil }
	node := &fakeNode{t: t, mc: newMeshConn(nodeSide), sent: make(chan sentMsg, 100)}
	go node.run()
	cfg := defaultRelayConfig()
	cfg.Enabled = true
	cfg.AllowedNodes = []string{"!11111111"}
	if err := r.SetConfig(cfg); err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 100 && r.Status().State != "connected"; i++ {
		time.Sleep(20 * time.Millisecond)
	}
	if st := r.Status().State; st != "connected" {
		t.Fatalf("relay state %q, want connected", st)
	}
	t.Cleanup(func() { r.Stop(); nodeSide.Close() })
	return r, node
}

func checkSizes(t *testing.T, msgs []string, max int) {
	t.Helper()
	for _, m := range msgs {
		if len(m) > max {
			t.Errorf("message is %d bytes (max %d): %q", len(m), max, m)
		}
		if !utf8.ValidString(m) {
			t.Errorf("message splits a character: %q", m)
		}
	}
}

func TestRelayConversation(t *testing.T) {
	ks := fakeKiwix(t)
	defer ks.Close()
	base, books := ks.URL+"/kiwix", []string{wikiBook, survBook}
	if real := os.Getenv("LANTERN_TEST_KIWIX"); real != "" {
		base = real // run against a real kiwix-serve with the test ZIMs
	}
	r, node := startRelay(t, base, books)

	if st := r.Status(); st.MyNode == nil || st.MyNode.LongName != "Lantern Relay 🤖" {
		t.Fatalf("gateway node not identified: %+v", st.MyNode)
	}

	node.receive(teamNode, "?s water")
	got := node.replies()
	checkSizes(t, got, 200)
	want := "🔎 water\n1 Water purification [Wiki]\n2 Emergency water treatment [Water]\n3 Water filter [Wiki]"
	if len(got) != 1 || got[0] != want {
		t.Fatalf("search reply:\n%q\nwant\n%q", got, want)
	}

	node.receive(teamNode, "?1")
	got = node.replies()
	checkSizes(t, got, 200)
	if len(got) != 3 || !strings.HasPrefix(got[0], "[1/") || !strings.Contains(got[0], "Water purification [Wiki]\nWater purification is the process") {
		t.Fatalf("article reply: %q", got)
	}
	all := strings.Join(got, " ")
	for _, bad := range []string{"Infobox junk", "For the album", "[1]", "<"} {
		if strings.Contains(all, bad) {
			t.Errorf("article text contains %q: %q", bad, all)
		}
	}

	node.receive(teamNode, "?more")
	got = node.replies()
	checkSizes(t, got, 200)
	if len(got) == 0 {
		t.Fatal("?more sent nothing")
	}

	node.receive(teamNode, "?toc")
	got = node.replies()
	if len(got) != 1 || got[0] != "☰ 1/1\n0 Summary\n1 History\n2 Boiling" {
		t.Fatalf("toc: %q", got)
	}

	node.receive(teamNode, "?go 2")
	got = node.replies()
	if len(got) != 2 || !strings.HasPrefix(got[0], "[1/2] §2 Boiling\nBring water to a rolling boil") ||
		!strings.HasPrefix(got[1], "[2/2] ") || strings.Contains(strings.Join(got, ""), "pot of water") {
		t.Fatalf("?go 2: %q", got)
	}

	node.receive(teamNode, "?go 1")
	got = node.replies()
	if len(got) != 3 || !strings.HasSuffix(got[0], "straining.") || !strings.HasSuffix(got[1], "safer to drink.") ||
		!strings.HasPrefix(got[2], "[3/3] — Modern era —\nChlorination") {
		t.Fatalf("?go 1 (subheading inline): %q", got)
	}

	node.receive(teamNode, "?more") // History is done, so this moves on to Boiling
	got = node.replies()
	if len(got) != 2 || !strings.HasPrefix(got[0], "[1/2] §2 Boiling") {
		t.Fatalf("?more into next section: %q", got)
	}
	node.receive(teamNode, "?more")
	got = node.replies()
	if len(got) != 1 || !strings.HasPrefix(got[0], "Lantern: end of") {
		t.Fatalf("end of article: %q", got)
	}

	// The survival guide, through plain words (no ?s)
	node.receive(teamNode, "water")
	node.replies()
	node.receive(teamNode, "?2")
	got = node.replies()
	if len(got) != 2 || !strings.Contains(got[0], "8 drops (1/8 teaspoon) of unscented household bleach") {
		t.Fatalf("survival guide: %q", got)
	}

	node.receive(teamNode, "?s zzqx")
	if got = node.replies(); len(got) != 1 || !strings.Contains(got[0], "No matches") {
		t.Fatalf("no results: %q", got)
	}
	node.receive(teamNode, "?4")
	if got = node.replies(); len(got) != 1 || got[0] != "Lantern: search first: ?s words" {
		t.Fatalf("?4 with no results: %q", got)
	}
	node.receive(teamNode, "?bogus")
	if got = node.replies(); len(got) != 1 || !strings.Contains(got[0], "unknown command") {
		t.Fatalf("unknown: %q", got)
	}
	node.receive(teamNode, "?help")
	if got = node.replies(); len(got) != 1 || got[0] != relayHelp || len(relayHelp) > 200 {
		t.Fatalf("help: %q", got)
	}

	// Someone not on the allowed list gets no answer, and shows up on the Relay page.
	node.receive(strNode, "?s water")
	if got = node.replies(); len(got) != 0 {
		t.Fatalf("stranger got an answer: %q", got)
	}
	st := r.Status()
	if len(st.Ignored) != 1 || st.Ignored[0].ID != "!22222222" || st.Ignored[0].LongName != "Stranger" {
		t.Fatalf("ignored list: %+v", st.Ignored)
	}
}

func TestRelayNewCommandCancelsOldReply(t *testing.T) {
	ks := fakeKiwix(t)
	defer ks.Close()
	r, node := startRelay(t, ks.URL+"/kiwix", []string{wikiBook, survBook})
	gate := make(chan struct{})
	r.sleep = func(time.Duration) { <-gate } // hold the sender after each message
	node.receive(teamNode, "?s water")
	<-node.sent // first message goes out, then the sender waits
	node.receive(teamNode, "?s oklahoma city")
	time.Sleep(200 * time.Millisecond)
	close(gate)
	got := node.replies()
	if len(got) != 1 || !strings.HasPrefix(got[0], "🔎 oklahoma city\n1 Oklahoma City [Wiki]") {
		t.Fatalf("after the second search: %q", got)
	}
}

func TestChunkText(t *testing.T) {
	text := strings.Repeat("Tornadoes 🌪️ are common in spring. ", 40)
	for _, max := range []int{80, 150, 188} {
		chunks := chunkText(text, max)
		if strings.Join(chunks, " ") != strings.TrimSpace(text) {
			t.Fatalf("chunks lost text at max %d", max)
		}
		checkSizes(t, chunks, max)
	}
	long := strings.Repeat("é", 300) // one "word" longer than a message
	checkSizes(t, chunkText(long, 100), 100)
}

func TestNodeIDs(t *testing.T) {
	for in, want := range map[string]string{"!A1B2C3D4": "!a1b2c3d4", "a1b2c3d4": "!a1b2c3d4", "305419896": "!12345678", "nope": ""} {
		if got := normalizeNodeID(in); got != want {
			t.Errorf("normalizeNodeID(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestSourceLabels(t *testing.T) {
	for book, want := range map[string]string{
		"wikipedia_en_all_maxi_2026-10": "Wiki", "wikipedia_en_medicine_maxi_2026-09": "Med",
		"gutenberg_en_lcc-r_2026-08": "Book", "ifixit_en_all_2026-07": "iFixit",
	} {
		if got := sourceLabel(book, ""); got != want {
			t.Errorf("sourceLabel(%q) = %q, want %q", book, got, want)
		}
	}
	if got := sourceLabel("something_odd", "Field Guide to Edible Plants"); got != "Field" {
		t.Errorf("fallback label: %q", got)
	}
}

func TestSearchFallsBackToEachBook(t *testing.T) {
	base := os.Getenv("LANTERN_TEST_KIWIX")
	if base == "" {
		t.Skip("set LANTERN_TEST_KIWIX to a kiwix-serve with the test ZIMs")
	}
	// A missing book makes kiwix-serve reject the combined search; Relay should still answer.
	ks := &kiwixSearcher{base: func() string { return base }, client: http.DefaultClient,
		books: func() []string { return []string{wikiBook, "missing_book", survBook} }}
	hits, _, err := ks.Search(t.Context(), "water", 0, 3)
	if err != nil || len(hits) != 3 || hits[0].Source != "Wiki" || hits[1].Source != "Water" {
		t.Fatalf("fallback search: %v %+v", err, hits)
	}
	a, err := ks.Fetch(t.Context(), hits[0])
	if err != nil || a.Sections[0].Title != "Summary" {
		t.Fatalf("fetch: %v %+v", err, a)
	}
}

func TestNodeAddressCleanup(t *testing.T) {
	for in, want := range map[string]string{"tcp:http://192.168.1.13/": "tcp:192.168.1.13", "tcp: 192.168.1.13 ": "tcp:192.168.1.13",
		"tcp:HTTPS://node.local:4403/x": "tcp:node.local:4403", "tcp:192.168.1.13": "tcp:192.168.1.13", "COM5": "COM5"} {
		c := relayConfig{Port: in}
		c.normalize()
		if c.Port != want {
			t.Errorf("%q -> %q, want %q", in, c.Port, want)
		}
	}
}

func TestRelaySendTest(t *testing.T) {
	ks := fakeKiwix(t)
	defer ks.Close()
	r, node := startRelay(t, ks.URL+"/kiwix", []string{wikiBook})
	if err := r.SendTest(1); err != nil {
		t.Fatal(err)
	}
	m := <-node.sent
	if m.to != meshBroadcast || !strings.HasPrefix(m.text, "Lantern Relay test ") {
		t.Fatalf("test message: %+v", m)
	}
	for i := 0; i < 50; i++ {
		if l := r.Status().Log; len(l) > 0 && strings.Contains(l[0].Info, "can transmit") {
			return
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatalf("no transmit confirmation in log: %+v", r.Status().Log)
}

func TestRealWikipediaPage(t *testing.T) {
	b, err := os.ReadFile("testdata/relay/real_portable_water_purification.html")
	if err != nil {
		t.Skip(err)
	}
	a := parseArticle("Portable water purification", "Wiki", string(b))
	if len(a.Sections) < 3 || !strings.HasPrefix(a.Sections[0].Text, "Portable water purification devices") {
		t.Fatalf("sections: %d, summary: %.200q", len(a.Sections), a.Sections[0].Text)
	}
	for _, c := range chunkText(a.Sections[0].Text, 188) {
		if strings.HasSuffix(c, "e.g.") {
			t.Errorf("split after an abbreviation: %q", c)
		}
	}
	for _, s := range a.Sections {
		for _, bad := range []string{"[1]", "Retrieved", "<", "RLCONF", "doi:"} {
			if strings.Contains(s.Text, bad) {
				t.Errorf("section %q contains %q", s.Title, bad)
			}
		}
	}
}

func TestRealWikiMedPage(t *testing.T) {
	b, err := os.ReadFile("testdata/relay/real_wikimed_mms.html")
	if err != nil {
		t.Skip(err)
	}
	a := parseArticle("Miracle Mineral Supplement", "Med", string(b))
	if len(a.Sections) < 4 || !strings.HasPrefix(a.Sections[0].Text, "Miracle Mineral Supplement, often referred to") {
		t.Fatalf("sections: %d, summary: %.200q", len(a.Sections), a.Sections[0].Text)
	}
	for _, s := range a.Sections {
		if strings.Contains(s.Text, "[1]") || strings.Contains(s.Text, "Retrieved") {
			t.Errorf("section %q has citation clutter", s.Title)
		}
	}
}

// newTestPdfLibrary indexes testdata/relay/zimgit-water_en_2026-11.zim: a small book
// collection with two real PDFs and one scanned (image-only) PDF.
func newTestPdfLibrary(t *testing.T, base string) *pdfLibrary {
	if _, err := exec.LookPath("pdftotext"); err != nil {
		t.Skip("pdftotext not installed")
	}
	dir := t.TempDir()
	p := Paths{Data: dir, Temp: dir, Tools: dir}
	l := &pdfLibrary{p: p, zims: func() []string { return []string{"testdata/relay/zimgit-water_en_2026-11.zim"} },
		base: func() string { return base }, tool: func() string { return pdftotextPath(p) },
		client: http.DefaultClient, index: map[string][]pdfPosting{}, status: pdfStatus{State: "none"}, kick: make(chan struct{}, 1)}
	l.refresh(context.Background())
	return l
}

func TestPdfLibrary(t *testing.T) {
	ks := fakeKiwix(t)
	defer ks.Close()
	l := newTestPdfLibrary(t, ks.URL+"/kiwix")
	st := l.Status()
	if st.State != "ready" || st.Docs != 2 || st.Scanned != 1 || st.Pages != 6 {
		t.Fatalf("status: %+v", st)
	}
	hits := l.Search("how many drops of bleach per gallon", 5)
	if len(hits) == 0 || hits[0].doc.Title != "Emergency Water Treatment" || hits[0].page != 2 {
		t.Fatalf("bleach search: %+v", hits)
	}
	if h := l.Search("ceramic filters", 5); len(h) == 0 || h[0].doc.Title != "Ceramic Water Filters" {
		t.Fatalf("plural search: %+v", h)
	}
	if h := l.Search("zzqx", 5); len(h) != 0 {
		t.Fatalf("nonsense found something: %+v", h)
	}
	// a second library on the same folder loads the saved index without reading PDFs again
	l2 := &pdfLibrary{p: l.p, zims: l.zims, base: func() string { return "" }, tool: func() string { return "" },
		index: map[string][]pdfPosting{}, kick: make(chan struct{}, 1)}
	l2.refresh(context.Background())
	if l2.Status().Docs != 2 {
		t.Fatalf("cached index not reused: %+v", l2.Status())
	}
}

func TestRelayAnswersFromPdf(t *testing.T) {
	ks := fakeKiwix(t)
	defer ks.Close()
	r, node := startRelay(t, ks.URL+"/kiwix", []string{wikiBook, survBook})
	r.pdf = newTestPdfLibrary(t, ks.URL+"/kiwix")

	node.receive(teamNode, "?s bleach")
	got := node.replies()
	if len(got) != 1 || !strings.Contains(got[0], "Emergency Water Treatment p.3 [Water]") {
		t.Fatalf("search with a PDF hit: %q", got)
	}
	n := ""
	for _, line := range strings.Split(got[0], "\n") {
		if strings.Contains(line, "Emergency Water Treatment") {
			n = strings.Fields(line)[0]
		}
	}
	node.receive(teamNode, "?"+n)
	got = node.replies()
	all := strings.Join(got, " ")
	if len(got) == 0 || !strings.HasPrefix(got[0], "[1/") || !strings.Contains(got[0], "§3 p.3\nDisinfecting with bleach") ||
		!strings.Contains(all, "8 drops of bleach per gallon") {
		t.Fatalf("PDF page: %q", got)
	}
	node.receive(teamNode, "?toc")
	got = node.replies()
	if len(got) == 0 || !strings.Contains(got[0], "0 Summary\n1 p.1 Emergency Water Treatment\n2 p.2 Boiling") {
		t.Fatalf("PDF toc: %q", got)
	}
	node.receive(teamNode, "?go 4")
	got = node.replies()
	if len(got) == 0 || !strings.Contains(got[0], "§4 p.4\nStorage") {
		t.Fatalf("PDF ?go 4: %q", got)
	}
}
