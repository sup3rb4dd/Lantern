package main

// Lantern Relay: answers questions that arrive over Meshtastic with short
// answers from the offline content on this drive.
//
// A Meshtastic node (the 🤖 gateway node) is plugged into this computer by USB,
// or reached over WiFi. Team members send it a direct message such as
// "?s water purification"; Relay searches the drive with Kiwix and replies in
// a few short messages, with a pause between them so the mesh isn't flooded.
// Only nodes on the allowed list get an answer. The message formats are
// documented in RELAY.md, and FieldMap's Ask Lantern screen depends on them.

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"
)

type relayConfig struct {
	Enabled          bool     `json:"enabled"`
	Port             string   `json:"port"`             // "auto", "COM5", "/dev/ttyACM0" or "tcp:192.168.1.40"
	Channel          int      `json:"channel"`          // channel index replies go out on (0 = primary)
	AllowedNodes     []string `json:"allowedNodes"`     // node IDs like "!a1b2c3d4"
	ChunkBytes       int      `json:"chunkBytes"`       // most bytes in one message
	PauseSeconds     float64  `json:"pauseSeconds"`     // wait between messages
	MessagesPerReply int      `json:"messagesPerReply"` // messages sent before waiting for ?more
}

func defaultRelayConfig() relayConfig {
	return relayConfig{Port: "auto", ChunkBytes: 200, PauseSeconds: 5, MessagesPerReply: 3, AllowedNodes: []string{}}
}

func (c *relayConfig) normalize() {
	c.Port = strings.TrimSpace(c.Port)
	if c.Port == "" {
		c.Port = "auto"
	}
	if strings.HasPrefix(c.Port, "tcp:") {
		c.Port = "tcp:" + cleanNodeAddress(strings.TrimPrefix(c.Port, "tcp:"))
	}
	c.ChunkBytes = clampInt(c.ChunkBytes, 80, 228)
	if c.PauseSeconds < 1 {
		c.PauseSeconds = 1
	}
	if c.PauseSeconds > 60 {
		c.PauseSeconds = 60
	}
	c.MessagesPerReply = clampInt(c.MessagesPerReply, 1, 10)
	c.Channel = clampInt(c.Channel, 0, 7)
	seen := map[string]bool{}
	out := []string{}
	for _, id := range c.AllowedNodes {
		id = normalizeNodeID(id)
		if id != "" && !seen[id] {
			seen[id] = true
			out = append(out, id)
		}
	}
	c.AllowedNodes = out
}

// cleanNodeAddress turns what people paste, like "http://192.168.1.13/", into "192.168.1.13".
func cleanNodeAddress(a string) string {
	a = strings.TrimSpace(a)
	for _, p := range []string{"tcp://", "http://", "https://", "tcp:"} {
		if len(a) >= len(p) && strings.EqualFold(a[:len(p)], p) {
			a = a[len(p):]
		}
	}
	if i := strings.IndexAny(a, "/?#"); i >= 0 {
		a = a[:i]
	}
	return strings.TrimSpace(a)
}

// normalizeNodeID accepts "!a1b2c3d4", "a1b2c3d4" or a decimal node number.
func normalizeNodeID(s string) string {
	s = strings.ToLower(strings.TrimSpace(s))
	s = strings.TrimPrefix(s, "!")
	if s == "" {
		return ""
	}
	if v, err := strconv.ParseUint(s, 16, 32); err == nil && len(s) == 8 {
		return nodeID(uint32(v))
	}
	if v, err := strconv.ParseUint(s, 10, 32); err == nil {
		return nodeID(uint32(v))
	}
	return ""
}

func clampInt(v, lo, hi int) int {
	if v < lo {
		return lo
	}
	if v > hi {
		return hi
	}
	return v
}

type relayLogEntry struct {
	Time int64  `json:"time"`
	Node string `json:"node"`
	Name string `json:"name"`
	Text string `json:"text"`
	Info string `json:"info"`
}

const (
	modeNone = iota
	modeSearch
	modeArticle
	modeToc
)

type relaySession struct {
	query   string
	results []searchHit
	total   int
	mode    int
	art     *article
	sec     int
	chunks  []string // messages for the current section or table of contents
	pos     int
	gen     uint64 // bumped on every new command, so stale queued replies are dropped
}

type relayOut struct {
	to   uint32
	text string
	gen  uint64
}

type relayIn struct {
	from uint32
	text string
}

type Relay struct {
	p        Paths
	ks       *kiwixSearcher
	cfgPath  string
	mu       sync.Mutex
	cfg      relayConfig
	state    string // off, connecting, connected, error
	detail   string
	portName string
	conn     *meshConn
	myNode   uint32
	nodes    map[uint32]*meshNode
	ignored  map[uint32]int64
	sessions map[uint32]*relaySession
	logs     []relayLogEntry
	acks     map[uint32]chan uint32
	stop     chan struct{}
	done     chan struct{}
	in       chan relayIn
	out      chan relayOut

	// for tests
	openFn func(port string) (io.ReadWriteCloser, string, error)
	sleep  func(time.Duration)
}

func newRelay(p Paths, kx *Kiwix) *Relay {
	r := newRelayWith(newKiwixSearcher(kx), filepath.Join(p.Data, "relay.json"))
	r.p = p
	return r
}

func newRelayWith(ks *kiwixSearcher, cfgPath string) *Relay {
	r := &Relay{
		ks:       ks,
		cfgPath:  cfgPath,
		cfg:      defaultRelayConfig(),
		state:    "off",
		nodes:    map[uint32]*meshNode{},
		ignored:  map[uint32]int64{},
		sessions: map[uint32]*relaySession{},
		acks:     map[uint32]chan uint32{},
		in:       make(chan relayIn, 32),
		out:      make(chan relayOut, 256),
		sleep:    time.Sleep,
	}
	r.openFn = r.openPort
	if b, err := os.ReadFile(r.cfgPath); err == nil {
		json.Unmarshal(b, &r.cfg)
	}
	r.cfg.normalize()
	go r.worker()
	go r.sender()
	return r
}

// ---------- settings ----------

func (r *Relay) Config() relayConfig {
	r.mu.Lock()
	defer r.mu.Unlock()
	c := r.cfg
	c.AllowedNodes = append([]string{}, r.cfg.AllowedNodes...)
	return c
}

// SetConfig saves new settings and reconnects if the connection settings changed.
func (r *Relay) SetConfig(c relayConfig) error {
	c.normalize()
	b, _ := json.MarshalIndent(c, "", "  ")
	if err := os.WriteFile(r.cfgPath, b, 0o644); err != nil {
		return err
	}
	r.mu.Lock()
	old := r.cfg
	r.cfg = c
	running := r.stop != nil
	r.mu.Unlock()
	switch {
	case !c.Enabled && running:
		r.Stop()
	case c.Enabled && !running:
		r.Start()
	case c.Enabled && running && (old.Port != c.Port):
		r.Stop()
		r.Start()
	}
	return nil
}

func (r *Relay) allowed(num uint32) bool {
	id := nodeID(num)
	for _, a := range r.cfg.AllowedNodes {
		if a == id {
			return true
		}
	}
	return false
}

// ---------- connection ----------

// Start connects to the node (if enabled) and keeps reconnecting until Stop.
func (r *Relay) Start() {
	r.mu.Lock()
	if r.stop != nil || !r.cfg.Enabled {
		r.mu.Unlock()
		return
	}
	stop, done := make(chan struct{}), make(chan struct{})
	r.stop, r.done = stop, done
	r.mu.Unlock()
	go r.run(stop, done)
}

func (r *Relay) Stop() {
	r.mu.Lock()
	stop, done := r.stop, r.done
	r.stop, r.done = nil, nil
	r.mu.Unlock()
	if stop == nil {
		return
	}
	close(stop)
	<-done
	r.setState("off", "")
}

func (r *Relay) setState(s, detail string) {
	r.mu.Lock()
	r.state, r.detail = s, detail
	r.mu.Unlock()
	if s == "error" || s == "connected" {
		fmt.Println("Lantern Relay:", s, detail)
	}
}

func (r *Relay) run(stop, done chan struct{}) {
	defer close(done)
	for {
		r.setState("connecting", r.Config().Port)
		rw, name, err := r.openFn(r.Config().Port)
		if err == nil {
			r.mu.Lock()
			r.portName = name
			r.mu.Unlock()
			err = r.serve(newMeshConn(rw), stop)
		}
		select {
		case <-stop:
			return
		default:
		}
		msg := "connection lost"
		if err != nil {
			msg = err.Error()
		}
		r.setState("error", msg+" (retrying in 10 seconds)")
		select {
		case <-stop:
			return
		case <-time.After(10 * time.Second):
		}
	}
}

func (r *Relay) openPort(port string) (io.ReadWriteCloser, string, error) {
	if strings.HasPrefix(port, "tcp:") {
		addr := strings.TrimPrefix(port, "tcp:")
		if _, _, err := net.SplitHostPort(addr); err != nil {
			addr = net.JoinHostPort(addr, "4403")
		}
		c, err := net.DialTimeout("tcp", addr, 10*time.Second)
		if err != nil {
			return nil, "", err
		}
		return c, addr, nil
	}
	if port != "auto" {
		s, err := openSerial(port)
		if err != nil {
			return nil, "", err
		}
		return s, port, nil
	}
	ports := listSerialPorts()
	if len(ports) == 0 {
		return nil, "", errors.New("no USB serial ports found: is the node plugged in?")
	}
	var lastErr error
	for _, p := range ports {
		s, err := openSerial(p)
		if err != nil {
			lastErr = err
			continue
		}
		if probeMeshtastic(s) {
			return s, p, nil
		}
		s.Close()
	}
	if lastErr != nil {
		return nil, "", fmt.Errorf("no Meshtastic node answered on %s (%v)", strings.Join(ports, ", "), lastErr)
	}
	return nil, "", fmt.Errorf("no Meshtastic node answered on %s", strings.Join(ports, ", "))
}

// probeMeshtastic checks that a port has a Meshtastic node on it.
func probeMeshtastic(rw io.ReadWriteCloser) bool {
	c := newMeshConn(rw)
	if c.wake() != nil {
		return false
	}
	time.Sleep(100 * time.Millisecond)
	if c.send(toRadioWantConfig(randomPacketID())) != nil {
		return false
	}
	got := make(chan bool, 1)
	go func() {
		_, err := c.readFrame()
		got <- err == nil
	}()
	select {
	case ok := <-got:
		return ok
	case <-time.After(4 * time.Second):
		return false
	}
}

func (r *Relay) serve(c *meshConn, stop chan struct{}) error {
	defer c.Close()
	if err := c.wake(); err != nil {
		return err
	}
	time.Sleep(100 * time.Millisecond)
	if err := c.send(toRadioWantConfig(randomPacketID())); err != nil {
		return err
	}
	frames := make(chan []byte, 64)
	readErr := make(chan error, 1)
	go func() {
		for {
			f, err := c.readFrame()
			if err != nil {
				readErr <- err
				return
			}
			frames <- f
		}
	}()
	r.mu.Lock()
	r.conn = c
	r.mu.Unlock()
	defer func() {
		r.mu.Lock()
		if r.conn == c {
			r.conn = nil
		}
		r.mu.Unlock()
	}()
	heartbeat := time.NewTicker(5 * time.Minute)
	defer heartbeat.Stop()
	for {
		select {
		case <-stop:
			return nil
		case err := <-readErr:
			return err
		case <-heartbeat.C:
			if err := c.send(toRadioHeartbeat()); err != nil {
				return err
			}
		case f := <-frames:
			r.handleFrame(f)
		}
	}
}

func (r *Relay) handleFrame(f []byte) {
	e := parseFromRadio(f)
	switch e.kind {
	case evMyInfo:
		r.mu.Lock()
		r.myNode = e.myNode
		r.mu.Unlock()
	case evNodeInfo, evUser:
		r.rememberNode(e.node)
	case evConfigDone:
		r.setState("connected", r.portName)
	case evRebooted:
		// the node restarted: ask for its info again
		r.mu.Lock()
		c := r.conn
		r.mu.Unlock()
		if c != nil {
			c.send(toRadioWantConfig(randomPacketID()))
		}
	case evRouting:
		r.mu.Lock()
		ch := r.acks[e.requestID]
		r.mu.Unlock()
		if ch != nil {
			select {
			case ch <- e.routingError:
			default:
			}
		}
	case evText:
		r.mu.Lock()
		my := r.myNode
		if n := r.nodes[e.from]; n != nil {
			n.LastHeard = time.Now().Unix()
		}
		r.mu.Unlock()
		if my == 0 || e.to != my || e.from == my {
			return // only direct messages to the gateway node
		}
		r.in <- relayIn{from: e.from, text: e.text}
	}
}

func (r *Relay) rememberNode(n *meshNode) {
	if n == nil || n.Num == 0 {
		return
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	old := r.nodes[n.Num]
	if old == nil {
		r.nodes[n.Num] = n
		return
	}
	if n.LongName != "" {
		old.LongName = n.LongName
	}
	if n.ShortName != "" {
		old.ShortName = n.ShortName
	}
	if n.LastHeard > old.LastHeard {
		old.LastHeard = n.LastHeard
	}
}

func (r *Relay) nodeName(num uint32) string {
	r.mu.Lock()
	defer r.mu.Unlock()
	if n := r.nodes[num]; n != nil {
		if n.LongName != "" {
			return n.LongName
		}
		return n.ShortName
	}
	return ""
}

func (r *Relay) addLog(num uint32, text, info string) {
	name := r.nodeName(num)
	r.mu.Lock()
	defer r.mu.Unlock()
	r.logs = append(r.logs, relayLogEntry{Time: time.Now().Unix(), Node: nodeID(num), Name: name, Text: text, Info: info})
	if len(r.logs) > 100 {
		r.logs = r.logs[len(r.logs)-100:]
	}
}

// ---------- answering ----------

func (r *Relay) worker() {
	for req := range r.in {
		r.mu.Lock()
		ok := r.allowed(req.from)
		if !ok {
			r.ignored[req.from] = time.Now().Unix()
		}
		r.mu.Unlock()
		if !ok {
			r.addLog(req.from, req.text, "ignored: not on the allowed list")
			continue
		}
		r.answer(req.from, req.text)
	}
}

// answer runs one command and queues the reply messages.
func (r *Relay) answer(from uint32, text string) {
	r.mu.Lock()
	s := r.sessions[from]
	if s == nil {
		s = &relaySession{}
		r.sessions[from] = s
	}
	s.gen++
	gen := s.gen
	cfg := r.cfg
	r.mu.Unlock()

	msgs, info := r.runCommand(s, cfg, text)
	r.addLog(from, text, info)
	for _, m := range msgs {
		r.out <- relayOut{to: from, text: m, gen: gen}
	}
}

func notice(s string) []string { return []string{"Lantern: " + s} }

const relayHelp = "Lantern Relay: ?s words = search. ?1 ?2 ?3 = read a result. ?more = keep going. ?toc = sections. ?go N = jump to section N."

func (r *Relay) runCommand(s *relaySession, cfg relayConfig, text string) ([]string, string) {
	text = strings.TrimSpace(text)
	if text == "" {
		return nil, "empty"
	}
	if !strings.HasPrefix(text, "?") {
		text = "?s " + text // plain words are a search
	}
	cmd, arg, _ := strings.Cut(text, " ")
	cmd = strings.ToLower(cmd)
	arg = strings.TrimSpace(arg)
	ctx, cancel := context.WithTimeout(context.Background(), 90*time.Second)
	defer cancel()

	switch cmd {
	case "?", "?help", "?h":
		return []string{relayHelp}, "help"
	case "?s", "?search":
		if arg == "" {
			return notice("send ?s and some words, like ?s water purification"), "empty search"
		}
		s.query, s.results, s.total = arg, nil, 0
		return r.searchPage(ctx, s, cfg)
	case "?more", "?m", "?next":
		switch s.mode {
		case modeSearch:
			return r.searchPage(ctx, s, cfg)
		case modeArticle:
			return r.articleMore(s, cfg)
		case modeToc:
			return r.sendChunks(s, cfg, "Lantern: that's all the sections. ?go N to read one.")
		}
		return notice("nothing to continue. ?s words to search."), "nothing to continue"
	case "?toc", "?sections":
		if s.art == nil {
			return notice("open a result first (?1, ?2 or ?3)."), "no article"
		}
		lines := make([]string, len(s.art.Sections))
		for i, sec := range s.art.Sections {
			lines[i] = fmt.Sprintf("%d %s", i, truncBytes(sec.Title, 48))
		}
		s.mode, s.chunks, s.pos = modeToc, packLines(lines, cfg.ChunkBytes-12), 0
		for i := range s.chunks {
			s.chunks[i] = fmt.Sprintf("☰ %d/%d\n%s", i+1, len(s.chunks), s.chunks[i])
		}
		return r.sendChunks(s, cfg, "")
	case "?go":
		if s.art == nil {
			return notice("open a result first (?1, ?2 or ?3)."), "no article"
		}
		n, err := strconv.Atoi(arg)
		if err != nil || n < 0 || n >= len(s.art.Sections) {
			return notice(fmt.Sprintf("pick a section from 0 to %d (?toc lists them).", len(s.art.Sections)-1)), "bad section"
		}
		r.openSection(s, cfg, n)
		return r.sendChunks(s, cfg, "")
	}
	// ?1, ?2, ?3 … pick a search result
	if n, err := strconv.Atoi(strings.TrimPrefix(cmd, "?")); err == nil {
		if len(s.results) == 0 {
			return notice("search first: ?s words"), "no results to open"
		}
		if n < 1 || n > len(s.results) {
			return notice(fmt.Sprintf("pick a result from 1 to %d.", len(s.results))), "bad result number"
		}
		h := s.results[n-1]
		a, err := r.ks.Fetch(ctx, h)
		if err != nil {
			return notice("couldn't open that page (" + err.Error() + ")"), "fetch failed: " + err.Error()
		}
		s.art = a
		r.openSection(s, cfg, 0)
		msgs, _ := r.sendChunks(s, cfg, "")
		return msgs, fmt.Sprintf("opened %q", h.Title)
	}
	return notice("unknown command. ?help lists them."), "unknown command"
}

func (r *Relay) searchPage(ctx context.Context, s *relaySession, cfg relayConfig) ([]string, string) {
	const perPage = 3
	start := len(s.results)
	if start > 0 && start >= s.total {
		return notice("no more results for \"" + truncBytes(s.query, 40) + "\"."), "no more results"
	}
	hits, total, err := r.ks.Search(ctx, s.query, start, perPage)
	if err != nil {
		return notice("search isn't working (" + err.Error() + ")"), "search failed: " + err.Error()
	}
	s.mode = modeSearch
	s.total = total
	if total < start+len(hits) {
		s.total = start + len(hits)
	}
	header := "🔎 " + truncBytes(s.query, 40)
	if len(hits) == 0 {
		if start == 0 {
			return []string{header + "\nNo matches. Try fewer or different words."}, "0 results"
		}
		s.total = start
		return notice("no more results."), "no more results"
	}
	s.results = append(s.results, hits...)
	room := cfg.ChunkBytes - len(header) - 1
	lineMax := room/len(hits) - 1
	if lineMax < 40 {
		lineMax = 40
	}
	lines := make([]string, len(hits))
	for i, h := range hits {
		tag := " [" + h.Source + "]"
		num := fmt.Sprintf("%d ", start+i+1)
		lines[i] = num + truncBytes(h.Title, lineMax-len(num)-len(tag)) + tag
	}
	msgs := packLines(lines, cfg.ChunkBytes-len(header)-1)
	for i := range msgs {
		msgs[i] = header + "\n" + msgs[i]
	}
	return msgs, fmt.Sprintf("%d results (%d total)", len(hits), total)
}

func (r *Relay) openSection(s *relaySession, cfg relayConfig, n int) {
	sec := s.art.Sections[n]
	head := "§ " + sec.Title
	if n == 0 {
		head = s.art.Title + " [" + s.art.Source + "]"
	}
	body := chunkText(head+"\n"+sec.Text, cfg.ChunkBytes-12)
	for i := range body {
		body[i] = fmt.Sprintf("[%d/%d] %s", i+1, len(body), body[i])
	}
	s.mode, s.sec, s.chunks, s.pos = modeArticle, n, body, 0
}

func (r *Relay) articleMore(s *relaySession, cfg relayConfig) ([]string, string) {
	if s.pos >= len(s.chunks) {
		if s.sec+1 >= len(s.art.Sections) {
			return notice("end of \"" + truncBytes(s.art.Title, 60) + "\". ?toc for sections, ?s to search."), "end of article"
		}
		r.openSection(s, cfg, s.sec+1)
	}
	return r.sendChunks(s, cfg, "")
}

// sendChunks takes the next batch of prepared messages.
func (r *Relay) sendChunks(s *relaySession, cfg relayConfig, whenDone string) ([]string, string) {
	if s.pos >= len(s.chunks) {
		if whenDone != "" {
			return []string{whenDone}, "done"
		}
		return nil, "done"
	}
	end := s.pos + cfg.MessagesPerReply
	if end > len(s.chunks) {
		end = len(s.chunks)
	}
	out := append([]string{}, s.chunks[s.pos:end]...)
	info := fmt.Sprintf("sent %d-%d of %d", s.pos+1, end, len(s.chunks))
	s.pos = end
	return out, info
}

// packLines joins lines into as few messages of at most max bytes as possible.
func packLines(lines []string, max int) []string {
	var out []string
	cur := ""
	for _, l := range lines {
		l = truncBytes(l, max)
		if cur == "" {
			cur = l
		} else if len(cur)+1+len(l) <= max {
			cur += "\n" + l
		} else {
			out = append(out, cur)
			cur = l
		}
	}
	if cur != "" {
		out = append(out, cur)
	}
	return out
}

// SendTest broadcasts a short message on a channel, to check that the gateway
// node can transmit. If any nearby node repeats it, the node reports that back
// (an implicit acknowledgement), and the result is written to the activity log.
func (r *Relay) SendTest(channel int) error {
	r.mu.Lock()
	c, my := r.conn, r.myNode
	r.mu.Unlock()
	if c == nil || r.Status().State != "connected" {
		return errors.New("Relay isn't connected to the node")
	}
	channel = clampInt(channel, 0, 7)
	id := randomPacketID()
	ack := make(chan uint32, 1)
	r.mu.Lock()
	r.acks[id] = ack
	r.mu.Unlock()
	text := "Lantern Relay test " + time.Now().Format("15:04:05")
	if err := c.send(toRadioText(meshBroadcast, uint32(channel), text, id)); err != nil {
		r.mu.Lock()
		delete(r.acks, id)
		r.mu.Unlock()
		return err
	}
	r.addLog(my, text, fmt.Sprintf("test sent on channel %d, waiting up to 60 s for a nearby node to repeat it", channel))
	go func() {
		defer func() {
			r.mu.Lock()
			delete(r.acks, id)
			r.mu.Unlock()
		}()
		select {
		case code := <-ack:
			if code == 0 {
				r.addLog(my, text, "test: a nearby node repeated it, so the gateway can transmit")
			} else {
				r.addLog(my, text, fmt.Sprintf("test: no node repeated it (mesh error %d); the gateway may not be transmitting", code))
			}
		case <-time.After(60 * time.Second):
			r.addLog(my, text, "test: no node repeated it within 60 s; the gateway may not be transmitting")
		}
	}()
	return nil
}

// sender sends queued replies one at a time: it waits for the mesh to
// acknowledge each message (or 30 seconds), then pauses before the next.
func (r *Relay) sender() {
	for m := range r.out {
		r.mu.Lock()
		s := r.sessions[m.to]
		stale := s != nil && s.gen != m.gen
		c := r.conn
		cfg := r.cfg
		r.mu.Unlock()
		if stale {
			continue // a newer command from this node replaced this reply
		}
		if c == nil {
			continue
		}
		id := randomPacketID()
		ack := make(chan uint32, 1)
		r.mu.Lock()
		r.acks[id] = ack
		r.mu.Unlock()
		if err := c.send(toRadioText(m.to, uint32(cfg.Channel), m.text, id)); err == nil {
			select {
			case code := <-ack:
				if code != 0 {
					r.addLog(m.to, "", fmt.Sprintf("a reply wasn't delivered (mesh error %d)", code))
				}
			case <-time.After(30 * time.Second):
			}
		}
		r.mu.Lock()
		delete(r.acks, id)
		r.mu.Unlock()
		r.sleep(time.Duration(cfg.PauseSeconds * float64(time.Second)))
	}
}

// ---------- status for the Relay page ----------

type relayStatus struct {
	State   string          `json:"state"`
	Detail  string          `json:"detail"`
	Port    string          `json:"port"`
	MyNode  *meshNode       `json:"myNode"`
	Config  relayConfig     `json:"config"`
	Ports   []string        `json:"ports"`
	Nodes   []meshNode      `json:"nodes"`
	Ignored []meshNode      `json:"ignored"`
	Log     []relayLogEntry `json:"log"`
	Search  bool            `json:"searchReady"`
}

func (r *Relay) Status() relayStatus {
	ports := listSerialPorts()
	searchReady := r.ks.base() != "" && len(r.ks.books()) > 0
	r.mu.Lock()
	defer r.mu.Unlock()
	st := relayStatus{State: r.state, Detail: r.detail, Port: r.portName, Config: r.cfg, Ports: ports, Search: searchReady}
	if st.Ports == nil {
		st.Ports = []string{}
	}
	if r.state != "connected" {
		st.Port = ""
	}
	if r.myNode != 0 {
		if n := r.nodes[r.myNode]; n != nil {
			c := *n
			st.MyNode = &c
		} else {
			st.MyNode = &meshNode{Num: r.myNode, ID: nodeID(r.myNode)}
		}
	}
	for num, n := range r.nodes {
		if num != r.myNode {
			st.Nodes = append(st.Nodes, *n)
		}
	}
	sort.Slice(st.Nodes, func(a, b int) bool { return st.Nodes[a].LastHeard > st.Nodes[b].LastHeard })
	if len(st.Nodes) > 60 {
		st.Nodes = st.Nodes[:60]
	}
	for num, t := range r.ignored {
		n := meshNode{Num: num, ID: nodeID(num), LastHeard: t}
		if k := r.nodes[num]; k != nil {
			n.LongName, n.ShortName = k.LongName, k.ShortName
		}
		st.Ignored = append(st.Ignored, n)
	}
	sort.Slice(st.Ignored, func(a, b int) bool { return st.Ignored[a].LastHeard > st.Ignored[b].LastHeard })
	st.Log = append([]relayLogEntry{}, r.logs...)
	for i, j := 0, len(st.Log)-1; i < j; i, j = i+1, j-1 {
		st.Log[i], st.Log[j] = st.Log[j], st.Log[i]
	}
	if st.Nodes == nil {
		st.Nodes = []meshNode{}
	}
	if st.Ignored == nil {
		st.Ignored = []meshNode{}
	}
	return st
}
