package main

import (
	"bufio"
	"encoding/json"
	"net"
	"strings"
	"testing"
	"time"
)

func TestDeckText(t *testing.T) {
	cases := map[string]string{
		"🔎 water purification\n1 Water [Wiki]": "water purification 1 Water [Wiki]",
		"Lantern Relay 🤖":                      "Lantern Relay",
		"“Café” — ok…":                         `"Cafe" - ok...`,
		"  lots   of\tspace ":                  "lots of space",
	}
	for in, want := range cases {
		if got := deckText(in, 100); got != want {
			t.Errorf("deckText(%q) = %q, want %q", in, got, want)
		}
	}
	if got := deckText("abcdefghij", 4); got != "abcd" {
		t.Errorf("shortened: %q", got)
	}
}

func TestDeckConfigDefaults(t *testing.T) {
	c := relayConfig{DeckReplies: []string{" ack 🤖 ", "", "a very long quick reply that will not fit", "3", "4", "5"}}
	c.normalize()
	if c.Deck != "auto" {
		t.Errorf("deck %q", c.Deck)
	}
	if len(c.DeckReplies) != 4 || c.DeckReplies[0] != "ack" || len(c.DeckReplies[1]) > 24 {
		t.Errorf("replies %q", c.DeckReplies)
	}
	c = relayConfig{}
	c.normalize()
	if strings.Join(c.DeckReplies, ",") != "ACK,EN ROUTE,ALL CLEAR,HOLD" {
		t.Errorf("default replies %q", c.DeckReplies)
	}
}

// The Deck sees channel chat and Relay's answers, and its quick replies go out on the mesh.
func TestDeckLink(t *testing.T) {
	r, node := startRelay(t, "", nil)

	// team chat on the channel, then a question to Relay
	data := (&pbWriter{}).varint(1, portText).bytes(2, []byte("Radio check, how copy?"))
	mp := (&pbWriter{}).fixed32(1, teamNode).fixed32(2, meshBroadcast).message(4, data).
		fixed32(6, randomPacketID()).fixed32(8, 0x40D00000) // rx_snr 6.5
	node.mc.send(fromRadio(2, mp))
	node.receive(teamNode, "?help")
	node.replies()

	deckSide, pcSide := net.Pipe()
	d := &deckLink{rl: r}
	stop := make(chan struct{})
	done := make(chan struct{})
	go func() {
		d.serve(&deckPort{rw: pcSide, lines: readLines(pcSide)}, stop)
		close(done)
	}()
	defer func() { close(stop); deckSide.Close(); <-done }()

	in := bufio.NewReader(deckSide)
	readState := func() deckStateMsg {
		t.Helper()
		for {
			deckSide.SetReadDeadline(time.Now().Add(5 * time.Second))
			l, err := in.ReadBytes('\n')
			if err != nil {
				t.Fatal(err)
			}
			var m deckStateMsg
			if json.Unmarshal(l, &m) == nil && m.T == "state" {
				return m
			}
		}
	}
	st := readState()
	if st.Relay != "on" || st.GW != "Lantern Relay" {
		t.Errorf("relay %q gw %q", st.Relay, st.GW)
	}
	if st.Q != 1 || !strings.Contains(st.LastQ, "?help") {
		t.Errorf("questions %d, last %q", st.Q, st.LastQ)
	}
	var kinds []string
	for _, m := range st.Msgs {
		kinds = append(kinds, m.K+":"+m.F+":"+m.X)
	}
	got := strings.Join(kinds, " | ")
	for _, want := range []string{"c:Elijah:Radio check, how copy?", "d:Elijah:?help", "r:RELAY > Elijah:help"} {
		if !strings.Contains(got, want) {
			t.Errorf("messages %q missing %q", got, want)
		}
	}
	var elijah *deckNode
	for i := range st.Nodes {
		if st.Nodes[i].N == "Elijah" {
			elijah = &st.Nodes[i]
		}
	}
	if elijah == nil || elijah.SNR != "6.5" || elijah.Ago > 5 {
		t.Errorf("nodes %+v", st.Nodes)
	}

	// a quick reply from the Deck
	deckSide.SetWriteDeadline(time.Now().Add(5 * time.Second))
	deckSide.Write([]byte(`{"t":"send","id":7,"text":"ACK"}` + "\n"))
	select {
	case m := <-node.sent:
		if m.to != meshBroadcast || m.text != "ACK" {
			t.Errorf("sent %+v", m)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("quick reply never reached the node")
	}
	for {
		deckSide.SetReadDeadline(time.Now().Add(5 * time.Second))
		l, err := in.ReadBytes('\n')
		if err != nil {
			t.Fatal(err)
		}
		if strings.Contains(string(l), `"t":"ack"`) {
			if !strings.Contains(string(l), `"id":7`) || !strings.Contains(string(l), `"ok":true`) {
				t.Errorf("ack %s", l)
			}
			break
		}
	}
	st = readState()
	if last := st.Msgs[len(st.Msgs)-1]; last.K != "m" || last.X != "ACK" {
		t.Errorf("last message %+v", last)
	}
}

func TestDeckHello(t *testing.T) {
	if v, ok := deckHello([]byte(`{"t":"hello","fw":"lantern-deck","v":"1.0.0"}`)); !ok || v != "1.0.0" {
		t.Error("hello not recognised")
	}
	if _, ok := deckHello([]byte(`{"t":"hello","fw":"something-else"}`)); ok {
		t.Error("other firmware taken for a Deck")
	}
}
