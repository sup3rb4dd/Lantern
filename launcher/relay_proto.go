package main

// Minimal protobuf encoding and decoding: just enough for the Meshtastic
// phone API used by Lantern Relay. Ported from FieldMap's Proto.java.

import "encoding/binary"

type pbWriter struct{ b []byte }

func (w *pbWriter) tag(field, wire int) { w.uvarint(uint64(field<<3 | wire)) }

func (w *pbWriter) uvarint(v uint64) {
	for v >= 0x80 {
		w.b = append(w.b, byte(v)|0x80)
		v >>= 7
	}
	w.b = append(w.b, byte(v))
}

func (w *pbWriter) varint(field int, v uint64) *pbWriter {
	w.tag(field, 0)
	w.uvarint(v)
	return w
}

func (w *pbWriter) boolean(field int, v bool) *pbWriter {
	if v {
		w.varint(field, 1)
	}
	return w
}

func (w *pbWriter) fixed32(field int, v uint32) *pbWriter {
	w.tag(field, 5)
	w.b = binary.LittleEndian.AppendUint32(w.b, v)
	return w
}

func (w *pbWriter) bytes(field int, v []byte) *pbWriter {
	w.tag(field, 2)
	w.uvarint(uint64(len(v)))
	w.b = append(w.b, v...)
	return w
}

func (w *pbWriter) message(field int, m *pbWriter) *pbWriter { return w.bytes(field, m.b) }

// pbReader walks the fields of one protobuf message.
type pbReader struct {
	b     []byte
	i     int
	field int
	wire  int
	val   uint64 // varint / fixed value
	data  []byte // length-delimited value
}

func (r *pbReader) readUvarint() (uint64, bool) {
	var v uint64
	for s := 0; s < 64; s += 7 {
		if r.i >= len(r.b) {
			return 0, false
		}
		c := r.b[r.i]
		r.i++
		v |= uint64(c&0x7f) << s
		if c < 0x80 {
			return v, true
		}
	}
	return 0, false
}

// next advances to the next field; false at the end or on malformed input.
func (r *pbReader) next() bool {
	if r.i >= len(r.b) {
		return false
	}
	t, ok := r.readUvarint()
	if !ok {
		return false
	}
	r.field, r.wire = int(t>>3), int(t&7)
	r.data = nil
	switch r.wire {
	case 0:
		r.val, ok = r.readUvarint()
		return ok
	case 1:
		if r.i+8 > len(r.b) {
			return false
		}
		r.val = binary.LittleEndian.Uint64(r.b[r.i:])
		r.i += 8
	case 2:
		n, ok := r.readUvarint()
		if !ok || n > uint64(len(r.b)-r.i) {
			return false
		}
		r.data = r.b[r.i : r.i+int(n)]
		r.i += int(n)
	case 5:
		if r.i+4 > len(r.b) {
			return false
		}
		r.val = uint64(binary.LittleEndian.Uint32(r.b[r.i:]))
		r.i += 4
	default:
		return false
	}
	return true
}

func (r *pbReader) u32() uint32 { return uint32(r.val) }
