package main

// /fast: replay a recording at any speed.
// The NVR's ISAPI download sends a clip faster than real time. We demux Hikvision's
// MPEG-PS, and pass raw H.264/H.265 to go2rtc paced at the chosen speed; go2rtc stamps
// frames as they arrive, so the browser plays them at exactly that pace.

import (
	"bufio"
	"bytes"
	"errors"
	"fmt"
	"io"
	"log"
	"net/http"
	"regexp"
	"strconv"
	"time"
)

var (
	reURI   = regexp.MustCompile(`^/Streaming/tracks/\d+/?\?[\w=&%.:-]*$`)
	reStart = regexp.MustCompile(`starttime=\d{8}T\d{6}Z`)
	reFrom  = regexp.MustCompile(`^\d{8}T\d{6}$`)
)

var dlClient = &http.Client{} // no timeout: streams for the length of the clip

type ctxClient struct {
	c *http.Client
	r *http.Request
}

func (cc ctxClient) Do(req *http.Request) (*http.Response, error) {
	return cc.c.Do(req.WithContext(cc.r.Context()))
}

func handleFast(w http.ResponseWriter, r *http.Request) {
	if !isLoopback(r) || r.Header.Get("Origin") != "" { // only go2rtc calls this
		http.Error(w, "forbidden", http.StatusForbidden)
		return
	}
	q := r.URL.Query()
	uri := q.Get("uri")
	speed, _ := strconv.ParseFloat(q.Get("speed"), 64)
	if !reURI.MatchString(uri) || speed < 0.1 || speed > 64 {
		http.Error(w, "bad request", 400)
		return
	}
	if from := q.Get("from"); reFrom.MatchString(from) { // start part-way through the clip
		uri = reStart.ReplaceAllString(uri, "starttime="+from+"Z")
	}
	secs, _ := strconv.Atoi(q.Get("secs"))
	limit := int64(secs+1) * 90000
	keyOnly := speed >= 8 // too many frames to decode at high speed: show keyframes only
	lg := nvrLogin()
	body := `<?xml version="1.0" encoding="UTF-8"?><downloadRequest version="1.0" xmlns="http://www.isapi.org/ver20/XMLSchema"><playbackURI>` +
		xmlEscape("rtsp://"+lg.Host+uri) + `</playbackURI></downloadRequest>`
	resp, err := digestDo(ctxClient{dlClient, r}, lg, "GET", "/ISAPI/ContentMgmt/download", []byte(body))
	if err != nil {
		http.Error(w, err.Error(), 502)
		return
	}
	defer resp.Body.Close()
	if resp.StatusCode != 200 {
		b, _ := io.ReadAll(io.LimitReader(resp.Body, 400))
		log.Printf("fast playback: NVR download failed (%d): %s", resp.StatusCode, b)
		http.Error(w, fmt.Sprintf("NVR download failed (%d)", resp.StatusCode), 502)
		return
	}

	w.Header().Set("Content-Type", "application/octet-stream")
	fl, _ := w.(http.Flusher)
	var (
		started bool
		pts0    int64
		t0      time.Time
	)
	err = demuxPS(resp.Body, func(pts int64, au []byte, key bool) error {
		if !started {
			if !key {
				return nil
			}
			if au = fromParamSets(au); au == nil {
				return nil
			}
			started, pts0, t0 = true, pts, time.Now()
			w.WriteHeader(200)
		} else if secs > 0 && pts-pts0 > limit {
			return io.EOF // reached the end of the clip
		} else if keyOnly && !key {
			return nil
		}
		due := t0.Add(time.Duration(float64(pts-pts0) / 90000 / speed * float64(time.Second)))
		if d := time.Until(due); d > 0 {
			select {
			case <-time.After(d):
			case <-r.Context().Done():
				return r.Context().Err()
			}
		}
		if _, err := w.Write(au); err != nil {
			return err
		}
		if fl != nil {
			fl.Flush()
		}
		return nil
	})
	if !started {
		http.Error(w, "no video found in recording", 502)
	}
	if err != nil && !errors.Is(err, io.EOF) && r.Context().Err() == nil {
		log.Println("fast playback:", err)
	}
}

func xmlEscape(s string) string {
	var b bytes.Buffer
	for _, c := range []byte(s) {
		switch c {
		case '&':
			b.WriteString("&amp;")
		case '<':
			b.WriteString("&lt;")
		case '>':
			b.WriteString("&gt;")
		default:
			b.WriteByte(c)
		}
	}
	return b.String()
}

// ---- MPEG-PS demux ----

// demuxPS calls onAU for every video access unit with its 90 kHz PTS (unwrapped),
// the frame as Annex-B with 4-byte start codes, and whether it's a keyframe.
func demuxPS(src io.Reader, onAU func(pts int64, au []byte, key bool) error) error {
	r := bufio.NewReaderSize(src, 1<<20)
	es := &esParser{onAU: onAU, codec: -1, lastPTS: -1}
	var (
		lastRaw int64 = -1
		wrap    int64
		synced  bool
	)
	for {
		id, err := nextStartCode(r)
		if err != nil {
			if err == io.EOF || err == io.ErrUnexpectedEOF {
				return es.finish()
			}
			return err
		}
		if !synced { // skip Hikvision's file header etc. until the first pack header
			if id != 0xBA {
				continue
			}
			synced = true
		}
		switch {
		case id == 0xBA: // pack header
			b, err := r.Peek(10)
			if err != nil {
				return es.finish()
			}
			if b[0]&0xC0 == 0x40 { // MPEG-2
				r.Discard(10 + int(b[9]&7))
			} else { // MPEG-1
				r.Discard(8)
			}
		case id == 0xB9: // end code
		case id >= 0xBB: // anything with a length field
			lb := make([]byte, 2)
			if _, err := io.ReadFull(r, lb); err != nil {
				return es.finish()
			}
			pl := make([]byte, int(lb[0])<<8|int(lb[1]))
			if _, err := io.ReadFull(r, pl); err != nil {
				return es.finish()
			}
			if id < 0xE0 || id > 0xEF || len(pl) < 3 || pl[0]&0xC0 != 0x80 {
				continue // not MPEG-2 video PES
			}
			hl := int(pl[2])
			if 3+hl > len(pl) {
				continue
			}
			pts := int64(-1)
			if pl[1]&0x80 != 0 && hl >= 5 {
				raw := int64(pl[3]&0x0E)<<29 | int64(pl[4])<<22 | int64(pl[5]&0xFE)<<14 | int64(pl[6])<<7 | int64(pl[7])>>1
				if lastRaw >= 0 && raw < lastRaw-(1<<32) { // 33-bit wrap
					wrap += 1 << 33
				}
				lastRaw = raw
				pts = raw + wrap
			}
			if err := es.write(pl[3+hl:], pts); err != nil {
				return err
			}
		}
	}
}

// esParser splits the video elementary stream into NAL units and groups them into
// access units (frames), matching each frame with the PTS of the PES it started in.
type esParser struct {
	onAU     func(pts int64, au []byte, key bool) error
	buf      []byte
	base     int64 // stream offset of buf[0]
	marks    []ptsMark
	codec    int // -1 unknown, 0 H.264, 1 H.265
	au       []byte
	auOff    int64
	auKey    bool
	sawSlice bool
	lastPTS  int64
	lastDur  int64
}

type ptsMark struct{ off, pts int64 }

func (p *esParser) write(data []byte, pts int64) error {
	if pts >= 0 {
		p.marks = append(p.marks, ptsMark{p.base + int64(len(p.buf)), pts})
	}
	p.buf = append(p.buf, data...)
	if p.codec < 0 {
		switch {
		case bytes.Contains(p.buf, []byte{0, 0, 1, 0x40, 0x01}):
			p.codec = 1
		case bytes.Contains(p.buf, []byte{0, 0, 1, 0x67}), bytes.Contains(p.buf, []byte{0, 0, 1, 0x27}), bytes.Contains(p.buf, []byte{0, 0, 1, 0x47}):
			p.codec = 0
		case len(p.buf) > 4<<20:
			return errors.New("unrecognised video codec")
		default:
			return nil
		}
	}
	return p.scan(false)
}

func (p *esParser) finish() error {
	if p.codec < 0 {
		return nil
	}
	if err := p.scan(true); err != nil {
		return err
	}
	return p.flushAU()
}

func (p *esParser) scan(final bool) error {
	var starts []int
	for i := 0; i+2 < len(p.buf); i++ {
		if p.buf[i] == 0 && p.buf[i+1] == 0 && p.buf[i+2] == 1 {
			starts = append(starts, i)
			i += 2
		}
	}
	if final {
		starts = append(starts, len(p.buf))
	}
	if len(starts) < 2 {
		return nil
	}
	for k := 0; k+1 < len(starts); k++ {
		s, e := starts[k]+3, starts[k+1]
		for e > s && p.buf[e-1] == 0 {
			e--
		}
		if e > s {
			if err := p.nal(p.buf[s:e], p.base+int64(starts[k])); err != nil {
				return err
			}
		}
	}
	last := starts[len(starts)-1]
	if final {
		last = len(p.buf)
	}
	p.buf = append(p.buf[:0:0], p.buf[last:]...)
	p.base += int64(last)
	return nil
}

func (p *esParser) nal(n []byte, off int64) error {
	var slice, auStart, key, skip bool
	if p.codec == 1 {
		t := (n[0] >> 1) & 0x3F
		slice = t <= 31
		key = (t >= 16 && t <= 21) || t == 32 || t == 33
		auStart = t == 32 || t == 33 || t == 34 || t == 35 || t == 39 || (slice && len(n) > 2 && n[2]&0x80 != 0)
		skip = t == 35
	} else {
		t := n[0] & 0x1F
		slice = t >= 1 && t <= 5
		key = t == 5 || t == 7
		auStart = t == 6 || t == 7 || t == 8 || t == 9 || (slice && len(n) > 1 && n[1]&0x80 != 0)
		skip = t == 9
	}
	if auStart && p.sawSlice {
		if err := p.flushAU(); err != nil {
			return err
		}
	}
	if len(p.au) == 0 {
		p.auOff = off
	}
	if !skip {
		p.au = append(p.au, 0, 0, 0, 1)
		p.au = append(p.au, n...)
	}
	p.auKey = p.auKey || key
	p.sawSlice = p.sawSlice || slice
	return nil
}

func (p *esParser) flushAU() error {
	if len(p.au) == 0 || !p.sawSlice {
		return nil
	}
	// PTS of the last PES that started at or before this frame, unless already used
	pts := int64(-1)
	for len(p.marks) > 0 && p.marks[0].off <= p.auOff {
		if p.marks[0].pts > p.lastPTS {
			pts = p.marks[0].pts
		}
		p.marks = p.marks[1:]
	}
	if pts < 0 {
		d := p.lastDur
		if d <= 0 {
			d = 3600 // 25 fps
		}
		pts = p.lastPTS + d
	} else if p.lastPTS >= 0 && pts > p.lastPTS {
		p.lastDur = pts - p.lastPTS
	}
	p.lastPTS = pts
	au, key := p.au, p.auKey
	p.au, p.auKey, p.sawSlice = nil, false, false
	return p.onAU(pts, au, key)
}

// nextStartCode advances past the next 00 00 01 xx and returns xx.
func nextStartCode(r *bufio.Reader) (byte, error) {
	zeros := 0
	for {
		c, err := r.ReadByte()
		if err != nil {
			return 0, err
		}
		switch {
		case c == 0:
			zeros++
		case c == 1 && zeros >= 2:
			return r.ReadByte()
		default:
			zeros = 0
		}
	}
}

// fromParamSets trims a keyframe so it starts at the VPS (H.265) or SPS (H.264),
// which is what go2rtc needs to recognise the stream.
func fromParamSets(au []byte) []byte {
	for i := 0; i+5 < len(au); i++ {
		if au[i] == 0 && au[i+1] == 0 && au[i+2] == 0 && au[i+3] == 1 {
			n := au[i+4]
			if (n == 0x40 && au[i+5] == 0x01) || (n&0x1F == 7 && n&0x80 == 0) {
				return au[i:]
			}
		}
	}
	return nil
}
