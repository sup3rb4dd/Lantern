package main

// Reads a ZIM file's directory (its list of entries) without decompressing
// anything, to find the PDFs inside book collections. The directory and the
// MIME type list are stored uncompressed, so this needs only Go's standard
// library. Format: https://wiki.openzim.org/wiki/ZIM_file_format

import (
	"bufio"
	"encoding/binary"
	"errors"
	"io"
	"os"
	"sort"
)

const zimMagic = 72173914

type zimEntry struct {
	Path  string // path inside the ZIM, without namespace for new-style ZIMs
	Title string
}

// zimListByMime returns the entries of one MIME type (e.g. application/pdf).
// It returns quickly with nothing when the ZIM has no entry of that type.
// limit stops the scan once that many matches are found (0 = no limit); the
// second result is true if the limit was hit.
func zimListByMime(file, mime string, limit int) ([]zimEntry, bool, error) {
	f, err := os.Open(file)
	if err != nil {
		return nil, false, err
	}
	defer f.Close()
	var h [80]byte
	if _, err := io.ReadFull(f, h[:]); err != nil {
		return nil, false, err
	}
	if binary.LittleEndian.Uint32(h[0:]) != zimMagic {
		return nil, false, errors.New("not a ZIM file")
	}
	entryCount := binary.LittleEndian.Uint32(h[24:])
	urlPtrPos := int64(binary.LittleEndian.Uint64(h[32:]))
	mimeListPos := int64(binary.LittleEndian.Uint64(h[56:]))

	// MIME type list: null-terminated strings, ending with an empty one.
	mr := bufio.NewReader(io.NewSectionReader(f, mimeListPos, 1<<20))
	want := -1
	for i := 0; ; i++ {
		s, err := mr.ReadString(0)
		if err != nil {
			return nil, false, err
		}
		s = s[:len(s)-1]
		if s == "" {
			break
		}
		if s == mime {
			want = i
		}
	}
	if want < 0 {
		return nil, false, nil
	}

	// Pointers to every directory entry, then the entries themselves (read in
	// file order through a block cache so it stays fast on USB drives).
	ptrs := make([]uint64, entryCount)
	pr := bufio.NewReaderSize(io.NewSectionReader(f, urlPtrPos, int64(entryCount)*8), 1<<20)
	var b8 [8]byte
	for i := range ptrs {
		if _, err := io.ReadFull(pr, b8[:]); err != nil {
			return nil, false, err
		}
		ptrs[i] = binary.LittleEndian.Uint64(b8[:])
	}
	sort.Slice(ptrs, func(a, b int) bool { return ptrs[a] < ptrs[b] })
	bc := &blockCache{r: f}
	var out []zimEntry
	for _, p := range ptrs {
		head, err := bc.read(int64(p), 16)
		if err != nil {
			return out, false, err
		}
		mt := binary.LittleEndian.Uint16(head[0:])
		if int(mt) != want {
			continue
		}
		ns := head[3]
		// content entry: mime(2) paramLen(1) ns(1) rev(4) cluster(4) blob(4) url\0 title\0
		strs, err := bc.read(int64(p)+16, 4096)
		if err != nil && len(strs) == 0 {
			return out, false, err
		}
		url, rest := cutNull(strs)
		title, _ := cutNull(rest)
		if title == "" {
			title = url
		}
		path := url
		if ns != 'C' {
			path = string(ns) + "/" + url // old-style ZIMs keep the namespace in the path
		}
		out = append(out, zimEntry{Path: path, Title: title})
		if limit > 0 && len(out) >= limit {
			return out, true, nil
		}
	}
	sort.Slice(out, func(a, b int) bool { return out[a].Path < out[b].Path })
	return out, false, nil
}

func cutNull(b []byte) (string, []byte) {
	for i, c := range b {
		if c == 0 {
			return string(b[:i]), b[i+1:]
		}
	}
	return string(b), nil
}

// blockCache serves small reads from 1 MB blocks.
type blockCache struct {
	r     io.ReaderAt
	start int64
	buf   []byte
}

func (c *blockCache) read(off int64, n int) ([]byte, error) {
	if c.buf == nil || off < c.start || off+int64(n) > c.start+int64(len(c.buf)) {
		c.start = off
		if cap(c.buf) < 1<<20 {
			c.buf = make([]byte, 1<<20)
		}
		c.buf = c.buf[:1<<20]
		m, err := c.r.ReadAt(c.buf, off)
		c.buf = c.buf[:m]
		if m == 0 {
			return nil, err
		}
	}
	end := off - c.start + int64(n)
	if end > int64(len(c.buf)) {
		end = int64(len(c.buf))
	}
	return c.buf[off-c.start : end], nil
}
