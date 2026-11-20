package main

// Meshtastic phone API over a byte stream (USB serial or TCP port 4403).
// Each protobuf is framed as 0x94 0xC3 <len MSB> <len LSB> <bytes>.
// Anything outside a frame is the node's debug log and is ignored.
// Field numbers come from github.com/meshtastic/protobufs (mesh.proto, portnums.proto).

import (
	"bufio"
	"errors"
	"fmt"
	"io"
	"math/rand"
	"sync"
)

const (
	meshBroadcast = 0xFFFFFFFF
	portText      = 1
	portNodeInfo  = 4
	portRouting   = 5
	maxFrame      = 512
)

type meshKind int

const (
	evOther meshKind = iota
	evMyInfo
	evNodeInfo
	evConfigDone
	evText
	evUser
	evRouting
	evRebooted
)

type meshNode struct {
	Num       uint32 `json:"num"`
	ID        string `json:"id"`
	LongName  string `json:"longName"`
	ShortName string `json:"shortName"`
	LastHeard int64  `json:"lastHeard"`
}

type meshEvent struct {
	kind         meshKind
	myNode       uint32
	node         *meshNode
	from, to     uint32
	channel      uint32
	packetID     uint32
	text         string
	requestID    uint32 // routing: the packet this answers
	routingError uint32 // routing: 0 means delivered
}

func nodeID(num uint32) string { return fmt.Sprintf("!%08x", num) }

// ---------- outgoing ToRadio messages ----------

func toRadioWantConfig(id uint32) []byte {
	return (&pbWriter{}).varint(3, uint64(id)).b
}

func toRadioHeartbeat() []byte {
	return (&pbWriter{}).message(7, &pbWriter{}).b
}

func toRadioText(to uint32, channel uint32, text string, packetID uint32) []byte {
	data := (&pbWriter{}).varint(1, portText).bytes(2, []byte(text))
	mp := (&pbWriter{}).fixed32(2, to).varint(3, uint64(channel)).message(4, data).
		fixed32(6, packetID).boolean(10, true)
	return (&pbWriter{}).message(1, mp).b
}

// ---------- incoming FromRadio messages ----------

func parseFromRadio(b []byte) meshEvent {
	var e meshEvent
	r := pbReader{b: b}
	for r.next() {
		switch r.field {
		case 2:
			parseMeshPacket(&e, r.data)
		case 3:
			e.kind = evMyInfo
			m := pbReader{b: r.data}
			for m.next() {
				if m.field == 1 {
					e.myNode = m.u32()
				}
			}
		case 4:
			e.kind = evNodeInfo
			e.node = parseNodeInfo(r.data)
		case 7:
			e.kind = evConfigDone
		case 8:
			e.kind = evRebooted
		}
	}
	return e
}

func parseNodeInfo(b []byte) *meshNode {
	n := &meshNode{}
	r := pbReader{b: b}
	for r.next() {
		switch r.field {
		case 1:
			n.Num = r.u32()
		case 2:
			parseUser(n, r.data)
		case 5:
			n.LastHeard = int64(r.u32())
		}
	}
	if n.ID == "" {
		n.ID = nodeID(n.Num)
	}
	return n
}

func parseUser(n *meshNode, b []byte) {
	r := pbReader{b: b}
	for r.next() {
		switch r.field {
		case 1:
			n.ID = string(r.data)
		case 2:
			n.LongName = string(r.data)
		case 3:
			n.ShortName = string(r.data)
		}
	}
}

func parseMeshPacket(e *meshEvent, b []byte) {
	r := pbReader{b: b}
	var decoded []byte
	for r.next() {
		switch r.field {
		case 1:
			e.from = r.u32()
		case 2:
			e.to = r.u32()
		case 3:
			e.channel = r.u32()
		case 4:
			decoded = r.data
		case 6:
			e.packetID = r.u32()
		}
	}
	if decoded == nil {
		return // still encrypted: not on one of our channels
	}
	var port uint32
	var payload []byte
	d := pbReader{b: decoded}
	for d.next() {
		switch d.field {
		case 1:
			port = d.u32()
		case 2:
			payload = d.data
		case 6:
			e.requestID = d.u32()
		}
	}
	switch port {
	case portText:
		e.kind = evText
		e.text = string(payload)
	case portNodeInfo:
		e.kind = evUser
		e.node = &meshNode{Num: e.from}
		parseUser(e.node, payload)
		if e.node.ID == "" {
			e.node.ID = nodeID(e.from)
		}
	case portRouting:
		e.kind = evRouting
		rr := pbReader{b: payload}
		for rr.next() {
			if rr.field == 3 {
				e.routingError = rr.u32()
			}
		}
	}
}

// ---------- framing ----------

// meshConn wraps a serial port or TCP connection to a Meshtastic node.
type meshConn struct {
	rw  io.ReadWriteCloser
	br  *bufio.Reader
	wmu sync.Mutex
}

func newMeshConn(rw io.ReadWriteCloser) *meshConn {
	return &meshConn{rw: rw, br: bufio.NewReaderSize(rw, 4096)}
}

// wake sends the bytes Meshtastic clients send first so the node switches
// its serial port from debug log output to API frames.
func (c *meshConn) wake() error {
	buf := make([]byte, 32)
	for i := range buf {
		buf[i] = 0xC3
	}
	c.wmu.Lock()
	defer c.wmu.Unlock()
	_, err := c.rw.Write(buf)
	return err
}

func (c *meshConn) send(toRadio []byte) error {
	if len(toRadio) > maxFrame {
		return errors.New("message too large for one frame")
	}
	frame := append([]byte{0x94, 0xC3, byte(len(toRadio) >> 8), byte(len(toRadio))}, toRadio...)
	c.wmu.Lock()
	defer c.wmu.Unlock()
	_, err := c.rw.Write(frame)
	return err
}

// readFrame returns the next FromRadio protobuf, skipping log text.
// Serial reads time out regularly; those come back as errTimeout and are retried.
func (c *meshConn) readFrame() ([]byte, error) {
	for {
		b, err := c.readByte()
		if err != nil {
			return nil, err
		}
		if b != 0x94 {
			continue
		}
		b, err = c.readByte()
		if err != nil {
			return nil, err
		}
		if b != 0xC3 {
			if b == 0x94 {
				c.br.UnreadByte()
			}
			continue
		}
		hi, err := c.readByte()
		if err != nil {
			return nil, err
		}
		lo, err := c.readByte()
		if err != nil {
			return nil, err
		}
		n := int(hi)<<8 | int(lo)
		if n > maxFrame {
			continue // corrupt length: resync
		}
		buf := make([]byte, n)
		for got := 0; got < n; {
			m, err := c.br.Read(buf[got:])
			got += m
			if err != nil && !errors.Is(err, errTimeout) {
				return nil, err
			}
		}
		return buf, nil
	}
}

func (c *meshConn) readByte() (byte, error) {
	for {
		b, err := c.br.ReadByte()
		if err == nil {
			return b, nil
		}
		if !errors.Is(err, errTimeout) {
			return 0, err
		}
	}
}

func (c *meshConn) Close() error { return c.rw.Close() }

var errTimeout = errors.New("read timeout")

func randomPacketID() uint32 {
	for {
		if v := rand.Uint32(); v != 0 {
			return v
		}
	}
}
