package main

// Lantern Deck: a small touchscreen (an ESP32 "Cheap Yellow Display") plugged into
// the Lantern PC by USB. It shows Relay's status, the mesh's messages and the nodes
// it has heard, and its quick-reply buttons send short messages on the team channel.
//
// The link is JSON, one object per line, at 115200 baud:
//   Deck → Lantern: {"t":"hello","fw":"lantern-deck","v":"1.0.0"} every few seconds,
//                   {"t":"send","id":3,"text":"ACK"} when a quick reply is tapped
//   Lantern → Deck: {"t":"state",...} every 2 seconds, {"t":"ack","id":3,"ok":true}
// No WiFi is used. Lantern finds the Deck by listening for its hello line.

import (
	"bufio"
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"strings"
	"sync"
	"time"
)

type deckStatus struct {
	State   string `json:"state"` // off, searching, connected
	Port    string `json:"port"`
	Version string `json:"version"`
}

type deckLink struct {
	rl       *Relay
	mu       sync.Mutex
	state    string
	port     string
	version  string
	lastPort string               // where the Deck was last found
	probed   map[string]time.Time // when each port was last listened to
	stop     chan struct{}
	done     chan struct{}
}

func newDeckLink(rl *Relay) *deckLink {
	d := &deckLink{rl: rl, state: "off"}
	rl.mu.Lock()
	rl.deck = d
	rl.mu.Unlock()
	return d
}

func (d *deckLink) Status() deckStatus {
	d.mu.Lock()
	defer d.mu.Unlock()
	return deckStatus{State: d.state, Port: d.port, Version: d.version}
}

func (d *deckLink) set(state, port, version string) {
	d.mu.Lock()
	changed := d.state != state
	d.state, d.port, d.version = state, port, version
	d.mu.Unlock()
	if changed && state == "connected" {
		fmt.Println("Lantern Deck: connected on", port)
	}
}

func (d *deckLink) Start() {
	d.mu.Lock()
	if d.stop != nil {
		d.mu.Unlock()
		return
	}
	d.stop, d.done = make(chan struct{}), make(chan struct{})
	stop, done := d.stop, d.done
	d.mu.Unlock()
	go d.run(stop, done)
}

func (d *deckLink) Stop() {
	d.mu.Lock()
	stop, done := d.stop, d.done
	d.stop, d.done = nil, nil
	d.mu.Unlock()
	if stop == nil {
		return
	}
	close(stop)
	<-done
}

func (d *deckLink) run(stop, done chan struct{}) {
	defer close(done)
	wait := func(t time.Duration) bool {
		select {
		case <-stop:
			return false
		case <-time.After(t):
			return true
		}
	}
	for {
		cfg := d.rl.Config()
		if cfg.Deck == "off" || (cfg.Deck == "auto" && !cfg.Enabled) {
			d.set("off", "", "")
			if !wait(5 * time.Second) {
				return
			}
			continue
		}
		d.set("searching", "", "")
		rw, port, version := d.find(cfg, stop)
		if rw == nil {
			if !wait(10 * time.Second) {
				return
			}
			continue
		}
		d.set("connected", port, version)
		d.serve(rw, stop)
		rw.Close()
		select {
		case <-stop:
			d.set("off", "", "")
			return
		default:
		}
		d.set("searching", "", "")
		if !wait(3 * time.Second) {
			return
		}
	}
}

// find opens the Deck's port: the one set in the settings, or else USB serial ports
// Relay isn't using, listening briefly on each for the Deck's hello line. To leave
// other devices alone, a port is only tried when it has just appeared (the Deck was
// plugged in), or every few minutes; and while Relay isn't connected (it may still be
// looking for its node) only the port the Deck was last found on is tried.
func (d *deckLink) find(cfg relayConfig, stop chan struct{}) (io.ReadWriteCloser, string, string) {
	var ports []string
	if cfg.Deck != "auto" {
		ports = []string{cfg.Deck}
	} else {
		st := d.rl.Status()
		all := listSerialPorts()
		now := time.Now()
		d.mu.Lock()
		if d.probed == nil {
			d.probed = map[string]time.Time{}
		}
		present := map[string]bool{}
		for _, p := range all {
			present[p] = true
		}
		for p := range d.probed {
			if !present[p] {
				delete(d.probed, p) // unplugged: try it straight away when it comes back
			}
		}
		for _, p := range all {
			if p == st.Port || (cfg.Port != "auto" && p == cfg.Port) {
				continue
			}
			if st.State != "connected" {
				if p == d.lastPort {
					ports = append(ports, p)
				}
				continue
			}
			if t, ok := d.probed[p]; ok && now.Sub(t) < 3*time.Minute && p != d.lastPort {
				continue
			}
			d.probed[p] = now
			ports = append(ports, p)
		}
		d.mu.Unlock()
	}
	for _, p := range ports {
		select {
		case <-stop:
			return nil, "", ""
		default:
		}
		s, err := openSerial(p)
		if err != nil {
			continue
		}
		var rw io.ReadWriteCloser = s
		lines := readLines(rw)
		deadline := time.After(3500 * time.Millisecond)
	listen:
		for {
			select {
			case l, ok := <-lines:
				if !ok {
					break listen
				}
				if v, ok := deckHello(l); ok {
					d.mu.Lock()
					d.lastPort = p
					d.mu.Unlock()
					return &deckPort{rw: rw, lines: lines}, p, v
				}
			case <-deadline:
				break listen
			}
		}
		rw.Close()
	}
	return nil, "", ""
}

// deckPort keeps the line reader that was started while looking for the Deck.
type deckPort struct {
	rw    io.ReadWriteCloser
	lines chan []byte
}

func (p *deckPort) Read(b []byte) (int, error)  { return 0, errors.New("use lines") }
func (p *deckPort) Write(b []byte) (int, error) { return p.rw.Write(b) }
func (p *deckPort) Close() error                { return p.rw.Close() }

func deckHello(line []byte) (string, bool) {
	var m struct {
		T, Fw, V string
	}
	if json.Unmarshal(line, &m) != nil || m.T != "hello" || m.Fw != "lantern-deck" {
		return "", false
	}
	return m.V, true
}

// readLines reads newline-ended lines (up to 1 KB each) until the port fails or closes.
func readLines(rw io.Reader) chan []byte {
	out := make(chan []byte, 16)
	go func() {
		defer close(out)
		buf := make([]byte, 256)
		var line []byte
		for {
			n, err := rw.Read(buf)
			for _, c := range buf[:n] {
				if c == '\n' {
					if l := bytes.TrimSpace(line); len(l) > 0 && l[0] == '{' {
						select {
						case out <- append([]byte{}, l...):
						default: // nobody listening (or too far behind): drop it
						}
					}
					line = line[:0]
				} else if len(line) < 1024 {
					line = append(line, c)
				}
			}
			if err != nil && !errors.Is(err, errTimeout) {
				return
			}
		}
	}()
	return out
}

func (d *deckLink) serve(rw io.ReadWriteCloser, stop chan struct{}) {
	dp := rw.(*deckPort)
	w := bufio.NewWriter(dp)
	send := func(v any) error {
		b, _ := json.Marshal(v)
		w.Write(b)
		w.WriteByte('\n')
		return w.Flush()
	}
	if send(d.rl.deckState(time.Now())) != nil {
		return
	}
	tick := time.NewTicker(2 * time.Second)
	defer tick.Stop()
	lastSeen := time.Now()
	for {
		select {
		case <-stop:
			return
		case l, ok := <-dp.lines:
			if !ok {
				return
			}
			lastSeen = time.Now()
			var m struct {
				T    string
				ID   int
				Text string
			}
			if json.Unmarshal(l, &m) != nil || m.T != "send" {
				continue
			}
			ack := map[string]any{"t": "ack", "id": m.ID, "ok": true}
			if err := d.rl.SendChat(m.Text); err != nil {
				ack["ok"], ack["err"] = false, deckText(err.Error(), 60)
			}
			if send(ack) != nil || send(d.rl.deckState(time.Now())) != nil {
				return
			}
		case <-tick.C:
			if time.Since(lastSeen) > 20*time.Second {
				return // the Deck stopped saying hello: unplugged or restarted
			}
			if d.rl.Config().Deck == "off" {
				return
			}
			if send(d.rl.deckState(time.Now())) != nil {
				return
			}
		}
	}
}

// ---------- what the Deck shows ----------

type deckNode struct {
	N   string `json:"n"`
	Ago int64  `json:"ago"` // seconds since heard
	SNR string `json:"snr"`
	Bat string `json:"bat"`
}

type deckMsg struct {
	F   string `json:"f"`   // who sent it
	Ago int64  `json:"ago"` // seconds ago
	X   string `json:"x"`   // text
	K   string `json:"k"`   // c chat, d direct to Relay, r Relay's action, m sent from Lantern
}

type deckStateMsg struct {
	T       string     `json:"t"`
	Clock   string     `json:"clock"`
	Relay   string     `json:"relay"` // on, connecting, problem, off
	Detail  string     `json:"detail"`
	GW      string     `json:"gw"`
	Port    string     `json:"port"`
	Ch      int        `json:"ch"`
	Up      int        `json:"up"` // nodes heard in the last 15 minutes
	Nodes   []deckNode `json:"nodes"`
	Msgs    []deckMsg  `json:"msgs"`
	Q       int        `json:"q"` // questions answered today
	LastQ   string     `json:"lastq"`
	LastAgo int64      `json:"lastqAgo"`
	AI      string     `json:"ai"`
	Search  bool       `json:"search"`
	Replies []string   `json:"replies"`
}

func (r *Relay) deckState(now time.Time) deckStateMsg {
	st := r.Status()
	m := deckStateMsg{T: "state", Clock: now.Format("15:04"), Detail: deckText(st.Detail, 60),
		Port: st.Port, Ch: st.Config.Channel, Search: st.Search, Replies: st.Config.DeckReplies}
	m.Relay = map[string]string{"connected": "on", "connecting": "connecting", "error": "problem"}[st.State]
	if m.Relay == "" {
		m.Relay = "off"
	}
	if st.MyNode != nil {
		m.GW = deckName(*st.MyNode, 24)
	}
	for _, n := range st.Nodes {
		ago := now.Unix() - n.LastHeard
		if n.LastHeard > 0 && ago < 15*60 {
			m.Up++
		}
		if len(m.Nodes) < 8 {
			dn := deckNode{N: deckName(n, 12), Ago: ago, SNR: "--", Bat: "--"}
			if n.LastHeard == 0 {
				dn.Ago = -1
			}
			if n.SNR != nil {
				dn.SNR = fmt.Sprintf("%.1f", *n.SNR)
			}
			if n.Battery > 100 {
				dn.Bat = "USB"
			} else if n.Battery > 0 {
				dn.Bat = fmt.Sprintf("%d%%", n.Battery)
			}
			m.Nodes = append(m.Nodes, dn)
		}
	}
	y, mo, d := now.Date()
	midnight := time.Date(y, mo, d, 0, 0, 0, 0, now.Location()).Unix()
	for _, e := range st.Log { // newest first
		if e.Text == "" || strings.HasPrefix(e.Info, "ignored") || strings.HasPrefix(e.Text, "Lantern Relay test") {
			continue
		}
		if e.Time >= midnight {
			m.Q++
		}
		if m.LastQ == "" {
			who := deckText(e.Name, 12)
			if who == "" {
				who = e.Node
			}
			m.LastQ = deckText(who+": "+e.Text, 60)
			m.LastAgo = now.Unix() - e.Time
		}
	}
	m.AI = "OFF"
	if st.AI != nil && st.Config.AI != aiModeOff {
		switch {
		case !st.AI.Installed && st.AI.External == "":
			m.AI = "NOT SET UP"
		case st.AI.State == "ready":
			m.AI = "READY"
		case st.AI.State == "starting":
			m.AI = "LOADING"
		case st.AI.State == "error":
			m.AI = "ERROR"
		default:
			m.AI = "STANDBY"
		}
	}
	r.mu.Lock()
	chat := append([]chatMsg{}, r.chat...)
	r.mu.Unlock()
	if len(chat) > 10 {
		chat = chat[len(chat)-10:]
	}
	for _, c := range chat {
		name := deckText(c.Name, 12)
		if name == "" {
			name = nodeID(c.From)
		}
		dm := deckMsg{F: name, Ago: now.Unix() - c.Time, X: deckText(c.Text, 160)}
		switch c.Kind {
		case "dm":
			dm.K = "d"
		case "relay":
			dm.K, dm.F = "r", "RELAY > "+name
		case "mine":
			dm.K, dm.F = "m", "LANTERN"
		default:
			dm.K = "c"
		}
		m.Msgs = append(m.Msgs, dm)
	}
	if m.Nodes == nil {
		m.Nodes = []deckNode{}
	}
	if m.Msgs == nil {
		m.Msgs = []deckMsg{}
	}
	return m
}

func deckName(n meshNode, max int) string {
	for _, s := range []string{n.LongName, n.ShortName} {
		if t := deckText(s, max); t != "" {
			return t
		}
	}
	return n.ID
}

var deckSwaps = map[rune]string{
	'‘': "'", '’': "'", '“': `"`, '”': `"`, '–': "-", '—': "-", '…': "...",
	'•': "*", '°': "deg", '×': "x", '→': "->", '§': "S", '☰': "=", '🔎': "", '🤖': "",
}

// deckText makes text safe for the Deck's built-in fonts (plain ASCII), on one
// line or several, shortened to max bytes.
func deckText(s string, max int) string {
	var b strings.Builder
	space := false
	for _, c := range s {
		if c == '\n' || c == '\r' || c == '\t' {
			c = ' '
		}
		var piece string
		switch {
		case c == ' ':
			if !space && b.Len() > 0 {
				piece = " "
			}
			space = true
		case c > 32 && c < 127:
			piece = string(c)
			space = false
		default:
			if r, ok := deckSwaps[c]; ok && r != "" {
				piece = r
				space = false
			} else if c >= 0xC0 && c < 0x250 {
				piece = asciiFold(c)
				space = false
			}
		}
		if b.Len()+len(piece) > max {
			break
		}
		b.WriteString(piece)
	}
	return strings.TrimSpace(b.String())
}

// asciiFold turns common accented Latin letters into plain ones (é → e).
func asciiFold(c rune) string {
	const from = "ÀÁÂÃÄÅàáâãäåÈÉÊËèéêëÌÍÎÏìíîïÒÓÔÕÖØòóôõöøÙÚÛÜùúûüÑñÇçÝýÿ"
	const to = "AAAAAAaaaaaaEEEEeeeeIIIIiiiiOOOOOOooooooUUUUuuuuNnCcYyy"
	i := 0
	for _, f := range from {
		if f == c {
			return to[i : i+1]
		}
		i++
	}
	return ""
}
