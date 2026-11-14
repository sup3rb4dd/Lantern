package main

import (
	"context"
	"encoding/json"
	"encoding/xml"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"
)

// Faster ZIM downloads: Kiwix publishes a .meta4 (Metalink) file next to every
// ZIM listing all the mirror servers that carry it. We speed-test several
// mirrors, keep the fast ones, and download pieces of the file from them over
// several connections at once. Finished pieces are recorded in a small
// ".chunks" file so an interrupted download resumes where it stopped.

const (
	parallelConns = 8
	probeBytes    = 2 << 20 // 2 MB test download per mirror
	maxProbe      = 18      // mirrors to test
	maxUseMirrors = 4       // mirrors to download from
)

type metalink struct {
	Files []struct {
		Name string `xml:"name,attr"`
		Size int64  `xml:"size"`
		URLs []struct {
			Priority string `xml:"priority,attr"`
			Location string `xml:"location,attr"`
			Value    string `xml:",chardata"`
		} `xml:"url"`
	} `xml:"file"`
}

// knownKiwixMirrors are public mirrors that copy the whole download.kiwix.org
// tree. Any that are missing a file or carry a different version are dropped
// by the speed test, so an outdated entry here does no harm.
var knownKiwixMirrors = []string{
	"https://mirror.download.kiwix.org/",
	"https://ftp.nluug.nl/kiwix/",
	"https://ftp5.de.freebsd.org/kiwix/",
	"https://mirrors.dotsrc.org/kiwix/",
	"https://ftp.fau.de/kiwix/",
	"https://mirror.accum.se/mirror/kiwix.org/",
	"https://ftp.acc.umu.se/mirror/kiwix.org/",
	"https://ftpmirror.your.org/pub/kiwix/",
	"https://mirrors.ocf.berkeley.edu/kiwix/",
	"https://md.mirrors.hacktegic.com/kiwix-md/",
}

// kiwixRelPath returns the path inside the Kiwix download tree for a link to
// download.kiwix.org or one of its mirrors, e.g. "zim/wikipedia/x.zim".
func kiwixRelPath(u string) string {
	pu, err := url.Parse(u)
	if err != nil {
		return ""
	}
	p := pu.Path
	for _, top := range []string{"/zim/", "/release/", "/nightly/", "/other/"} {
		if i := strings.Index(p, top); i >= 0 {
			if pu.Host == "download.kiwix.org" || strings.HasSuffix(pu.Host, ".kiwix.org") || i > 0 {
				return strings.TrimPrefix(p[i:], "/")
			}
		}
	}
	return ""
}

// mirrorsFor returns mirror URLs for a ZIM, best-priority first, ending with
// the original link as a fallback.
func (d *Downloader) mirrorsFor(ctx context.Context, zimURL string) []string {
	var out []string
	seen := map[string]bool{}
	add := func(u string) {
		u = strings.TrimSpace(u)
		if (strings.HasPrefix(u, "https://") || strings.HasPrefix(u, "http://")) && !seen[u] {
			seen[u] = true
			out = append(out, u)
		}
	}
	mctx, cancel := context.WithTimeout(ctx, 20*time.Second)
	defer cancel()
	if body, err := d.getBytes(mctx, zimURL+".meta4"); err == nil {
		var ml metalink
		if xml.Unmarshal(body, &ml) == nil && len(ml.Files) > 0 {
			urls := ml.Files[0].URLs
			sort.SliceStable(urls, func(a, b int) bool {
				pa, _ := strconv.Atoi(urls[a].Priority)
				pb, _ := strconv.Atoi(urls[b].Priority)
				if pa == 0 {
					pa = 999
				}
				if pb == 0 {
					pb = 999
				}
				return pa < pb
			})
			for _, u := range urls {
				add(u.Value)
			}
		}
	}
	add(zimURL)
	if rel := kiwixRelPath(zimURL); rel != "" && strings.Contains(zimURL, "kiwix.org") {
		for _, base := range knownKiwixMirrors {
			add(base + rel)
		}
	}
	return out
}

type mirrorSpeed struct {
	URL string
	BPS float64
}

// probeMirrors downloads a small piece from each mirror at the same time and
// returns the ones that work, fastest first.
func (d *Downloader) probeMirrors(ctx context.Context, urls []string, size int64) []mirrorSpeed {
	if len(urls) > maxProbe {
		urls = urls[:maxProbe]
	}
	n := int64(probeBytes)
	if size > 0 && size < n {
		n = size
	}
	var mu sync.Mutex
	var res []mirrorSpeed
	var wg sync.WaitGroup
	for _, u := range urls {
		wg.Add(1)
		go func(u string) {
			defer wg.Done()
			// Measure for at most 6 seconds; slow mirrors are judged on what arrived.
			pctx, cancel := context.WithTimeout(ctx, 6*time.Second)
			defer cancel()
			req, err := http.NewRequestWithContext(pctx, "GET", u, nil)
			if err != nil {
				return
			}
			req.Header.Set("User-Agent", userAgent)
			req.Header.Set("Range", fmt.Sprintf("bytes=0-%d", n-1))
			start := time.Now()
			resp, err := d.client.Do(req)
			if err != nil {
				return
			}
			defer resp.Body.Close()
			if resp.StatusCode != http.StatusPartialContent {
				return
			}
			if size > 0 && contentRangeTotal(resp.Header.Get("Content-Range")) != size {
				return // wrong or outdated file on this mirror
			}
			got, _ := io.Copy(io.Discard, resp.Body)
			el := time.Since(start).Seconds()
			if got <= 0 || el <= 0 {
				return
			}
			mu.Lock()
			res = append(res, mirrorSpeed{u, float64(got) / el})
			mu.Unlock()
		}(u)
	}
	wg.Wait()
	sort.Slice(res, func(a, b int) bool { return res[a].BPS > res[b].BPS })
	// Drop mirrors far slower than the best, so a slow one can't hold up the end.
	var keep []mirrorSpeed
	for _, m := range res {
		if len(keep) < maxUseMirrors && m.BPS >= res[0].BPS*0.25 {
			keep = append(keep, m)
		}
	}
	return keep
}

func contentRangeTotal(cr string) int64 {
	i := strings.LastIndex(cr, "/")
	if i < 0 {
		return -1
	}
	n, err := strconv.ParseInt(strings.TrimSpace(cr[i+1:]), 10, 64)
	if err != nil {
		return -1
	}
	return n
}

// fastDownload fetches a file of known size in pieces from several mirrors.
func (d *Downloader) fastDownload(ctx context.Context, j *Job, mirrors []mirrorSpeed, dest string, size int64) error {
	part := filepath.Join(d.p.Temp, filepath.Base(dest)+".part")
	statePath := part + ".chunks"

	chunk := int64(8 << 20)
	if size < 64<<20 {
		chunk = 1 << 20
	}
	nChunks := int((size + chunk - 1) / chunk)
	chunkLen := func(i int) int64 {
		if i == nChunks-1 {
			return size - int64(i)*chunk
		}
		return chunk
	}

	// Load progress: the .chunks file, or an older single-connection .part file.
	done := make([]bool, nChunks)
	if b, err := os.ReadFile(statePath); err == nil {
		var st struct {
			Size  int64
			Chunk int64
			Done  []int
		}
		if json.Unmarshal(b, &st) == nil && st.Size == size && st.Chunk == chunk {
			for _, i := range st.Done {
				if i >= 0 && i < nChunks {
					done[i] = true
				}
			}
		}
	} else if fi, err := os.Stat(part); err == nil {
		full := int(fi.Size() / chunk)
		for i := 0; i < full && i < nChunks; i++ {
			done[i] = true
		}
	}
	var have int64
	for i, ok := range done {
		if ok {
			have += chunkLen(i)
		}
	}

	if free, _ := freeSpace(d.p.Root); free > 0 && uint64(size-have) > free {
		return fmt.Errorf("not enough space on the drive: need %s more, only %s free",
			humanBytes(size-have), humanBytes(int64(free)))
	}

	f, err := os.OpenFile(part, os.O_CREATE|os.O_RDWR, 0o644)
	if err != nil {
		return err
	}
	if fi, _ := f.Stat(); fi != nil && fi.Size() < size {
		if err := f.Truncate(size); err != nil {
			f.Close()
			return err
		}
	}

	var stateMu sync.Mutex
	saveState := func() {
		var list []int
		for i, ok := range done {
			if ok {
				list = append(list, i)
			}
		}
		b, _ := json.Marshal(map[string]any{"Size": size, "Chunk": chunk, "Done": list})
		tmp := statePath + ".tmp"
		if os.WriteFile(tmp, b, 0o644) == nil {
			os.Remove(statePath)
			os.Rename(tmp, statePath)
		}
	}
	stateMu.Lock()
	saveState()
	stateMu.Unlock()

	queue := make(chan int, nChunks)
	for i, ok := range done {
		if !ok {
			queue <- i
		}
	}
	close(queue)

	var doneBytes atomic.Int64
	doneBytes.Store(have)
	var inflight atomic.Int64
	urls := make([]string, len(mirrors))
	for i, m := range mirrors {
		urls[i] = m.URL
	}
	host := hostOf(urls[0])
	d.setStep(j, fmt.Sprintf("Downloading over %d connections from %d mirror(s), fastest: %s", parallelConns, len(urls), host))

	wctx, cancel := context.WithCancel(ctx)
	defer cancel()
	var firstErr error
	var errOnce sync.Once
	fail := func(e error) {
		errOnce.Do(func() { firstErr = e; cancel() })
	}

	// Each mirror gets a strike on failure; one with too many strikes is skipped.
	var strikeMu sync.Mutex
	strikes := make([]int, len(urls))
	pickMirror := func(start int) int {
		strikeMu.Lock()
		defer strikeMu.Unlock()
		for k := 0; k < len(urls); k++ {
			m := (start + k) % len(urls)
			if strikes[m] < 6 {
				return m
			}
		}
		return -1
	}

	var wg sync.WaitGroup
	for w := 0; w < parallelConns; w++ {
		wg.Add(1)
		go func(w int) {
			defer wg.Done()
			buf := make([]byte, 128<<10)
			mirror := w % len(urls)
			for idx := range queue {
				if wctx.Err() != nil {
					return
				}
				off, ln := int64(idx)*chunk, chunkLen(idx)
				var attempt int
				for {
					m := pickMirror(mirror)
					if m < 0 {
						fail(errors.New("every mirror failed — check your internet connection and try again"))
						return
					}
					mirror = m
					got, err := d.fetchRange(wctx, urls[m], f, off, ln, size, buf, &inflight)
					inflight.Add(-got)
					if err == nil {
						doneBytes.Add(ln)
						stateMu.Lock()
						done[idx] = true
						saveState()
						stateMu.Unlock()
						break
					}
					if wctx.Err() != nil {
						return
					}
					strikeMu.Lock()
					strikes[m]++
					strikeMu.Unlock()
					mirror = m + 1 // try another mirror for this piece
					attempt++
					if attempt > 3*len(urls)+3 {
						fail(fmt.Errorf("download keeps failing: %v", err))
						return
					}
					select {
					case <-time.After(time.Duration(attempt) * time.Second):
					case <-wctx.Done():
						return
					}
				}
			}
		}(w)
	}

	// Progress reporting.
	stop := make(chan struct{})
	go func() {
		t := time.NewTicker(700 * time.Millisecond)
		defer t.Stop()
		last, lastT, speed := doneBytes.Load()+inflight.Load(), time.Now(), 0.0
		for {
			select {
			case <-stop:
				return
			case <-t.C:
				cur := doneBytes.Load() + inflight.Load()
				inst := float64(cur-last) / time.Since(lastT).Seconds()
				if speed == 0 {
					speed = inst
				} else {
					speed = speed*0.8 + inst*0.2
				}
				last, lastT = cur, time.Now()
				d.setProgress(j, cur, size, speed)
			}
		}
	}()

	wg.Wait()
	close(stop)
	syncErr := f.Sync()
	f.Close()
	if ctx.Err() != nil {
		return ctx.Err()
	}
	if firstErr != nil {
		return firstErr
	}
	if syncErr != nil {
		return syncErr
	}
	for _, ok := range done {
		if !ok {
			return errors.New("download incomplete, press Download to continue")
		}
	}
	d.setProgress(j, size, size, 0)
	os.Remove(statePath)
	os.MkdirAll(filepath.Dir(dest), 0o755)
	os.Remove(dest)
	return os.Rename(part, dest)
}

// fetchRange downloads one piece and writes it at its place in the file.
func (d *Downloader) fetchRange(ctx context.Context, u string, f *os.File, off, ln, size int64, buf []byte, inflight *atomic.Int64) (int64, error) {
	rctx, cancel := context.WithCancel(ctx)
	defer cancel()
	req, err := http.NewRequestWithContext(rctx, "GET", u, nil)
	if err != nil {
		return 0, err
	}
	req.Header.Set("User-Agent", userAgent)
	req.Header.Set("Range", fmt.Sprintf("bytes=%d-%d", off, off+ln-1))
	resp, err := d.client.Do(req)
	if err != nil {
		return 0, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusPartialContent {
		return 0, fmt.Errorf("%s returned %s", hostOf(u), resp.Status)
	}
	if t := contentRangeTotal(resp.Header.Get("Content-Range")); t != size {
		return 0, fmt.Errorf("%s has a different version of the file", hostOf(u))
	}

	// Abort a stalled connection so the piece can move to another mirror.
	var got int64
	lastProgress := time.Now()
	var lpMu sync.Mutex
	watchdog := time.NewTicker(5 * time.Second)
	defer watchdog.Stop()
	go func() {
		for {
			select {
			case <-rctx.Done():
				return
			case <-watchdog.C:
				lpMu.Lock()
				stalled := time.Since(lastProgress) > 45*time.Second
				lpMu.Unlock()
				if stalled {
					cancel()
					return
				}
			}
		}
	}()

	for got < ln {
		n, rerr := resp.Body.Read(buf)
		if n > 0 {
			if int64(n) > ln-got {
				n = int(ln - got)
			}
			if _, werr := f.WriteAt(buf[:n], off+got); werr != nil {
				return got, werr
			}
			got += int64(n)
			inflight.Add(int64(n))
			lpMu.Lock()
			lastProgress = time.Now()
			lpMu.Unlock()
		}
		if rerr == io.EOF {
			break
		}
		if rerr != nil {
			return got, rerr
		}
	}
	if got < ln {
		return got, errors.New("connection closed early")
	}
	return got, nil
}

// smartDownload uses mirrors and parallel connections when the server offers
// a mirror list (Kiwix does for every file), and falls back to a plain
// single-connection download otherwise.
func (d *Downloader) smartDownload(ctx context.Context, j *Job, u, dest string, size int64) error {
	d.setStep(j, "Finding the fastest download mirrors")
	if size <= 0 {
		size = d.metaSize(ctx, u)
	}
	if size > 0 {
		fast := d.probeMirrors(ctx, d.mirrorsFor(ctx, u), size)
		if ctx.Err() != nil {
			return ctx.Err()
		}
		if len(fast) > 0 {
			return d.fastDownload(ctx, j, fast, dest, size)
		}
	}
	return d.downloadFile(ctx, j, u, dest, size)
}

// metaSize reads the file size from the mirror list, or from a HEAD request.
func (d *Downloader) metaSize(ctx context.Context, u string) int64 {
	mctx, cancel := context.WithTimeout(ctx, 20*time.Second)
	defer cancel()
	if body, err := d.getBytes(mctx, u+".meta4"); err == nil {
		var ml metalink
		if xml.Unmarshal(body, &ml) == nil && len(ml.Files) > 0 && ml.Files[0].Size > 0 {
			return ml.Files[0].Size
		}
	}
	req, err := http.NewRequestWithContext(mctx, "HEAD", u, nil)
	if err != nil {
		return 0
	}
	req.Header.Set("User-Agent", userAgent)
	resp, err := d.client.Do(req)
	if err != nil {
		return 0
	}
	resp.Body.Close()
	if resp.StatusCode == 200 && resp.ContentLength > 0 {
		return resp.ContentLength
	}
	return 0
}
