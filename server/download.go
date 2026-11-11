package main

// /download: save a recorded clip as MP4 at full network speed.
// Downloads the clip from the NVR (ISAPI), demuxes the MPEG-PS and re-wraps the
// video as fragmented MP4 with the original timing - no re-encoding.
// If the NVR refuses the download, falls back to recording the RTSP playback in real time.

import (
	"fmt"
	"io"
	"log"
	"net/http"
	"net/url"
	"regexp"
	"strconv"

	"github.com/AlexxIT/go2rtc/pkg/core"
	"github.com/AlexxIT/go2rtc/pkg/h264"
	"github.com/AlexxIT/go2rtc/pkg/h264/annexb"
	"github.com/AlexxIT/go2rtc/pkg/h265"
	"github.com/AlexxIT/go2rtc/pkg/mp4"
	"github.com/pion/rtp"
)

var (
	reTrack = regexp.MustCompile(`^/Streaming/tracks/(\d+)`)
	reFile  = regexp.MustCompile(`[^\w.\-]+`)
)

func handleDownload(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	uri := q.Get("uri")
	from, to := q.Get("from"), q.Get("to") // YYYYMMDDTHHMMSS, NVR-local
	secs, _ := strconv.Atoi(q.Get("secs"))
	name := reFile.ReplaceAllString(q.Get("name"), "_")
	if name == "" {
		name = "clip.mp4"
	}
	m := reTrack.FindStringSubmatch(uri)
	if !reURI.MatchString(uri) || m == nil || !reFrom.MatchString(from) || !reFrom.MatchString(to) || secs <= 0 || secs > 4*3600 {
		http.Error(w, "bad request", 400)
		return
	}
	uri = reStart.ReplaceAllString(uri, "starttime="+from+"Z")
	lg := nvrLogin()

	fallback := func(why string) {
		log.Printf("download: %s - falling back to real-time RTSP", why)
		src := fmt.Sprintf("nvr:/Streaming/tracks/%s?starttime=%sZ&endtime=%sZ", m[1], from, to)
		http.Redirect(w, r, "/api/stream.mp4?"+url.Values{
			"src": {src}, "duration": {strconv.Itoa(secs)}, "filename": {name},
		}.Encode(), http.StatusFound)
	}

	body := `<?xml version="1.0" encoding="UTF-8"?><downloadRequest version="1.0" xmlns="http://www.isapi.org/ver20/XMLSchema"><playbackURI>` +
		xmlEscape("rtsp://"+lg.Host+uri) + `</playbackURI></downloadRequest>`
	resp, err := digestDo(ctxClient{dlClient, r}, lg, "GET", "/ISAPI/ContentMgmt/download", []byte(body))
	if err != nil {
		fallback(err.Error())
		return
	}
	defer resp.Body.Close()
	if resp.StatusCode != 200 {
		b, _ := io.ReadAll(io.LimitReader(resp.Body, 300))
		fallback(fmt.Sprintf("NVR answered %d: %s", resp.StatusCode, b))
		return
	}

	var (
		mux     *mp4.Muxer
		pts0    int64
		limit   = int64(secs+1) * 90000
		started bool
	)
	err = demuxPS(resp.Body, func(pts int64, au []byte, key bool) error {
		if !started {
			if !key {
				return nil
			}
			if au = fromParamSets(au); au == nil {
				return nil
			}
			avcc := annexb.EncodeToAVCC(au)
			var codec *core.Codec
			if h265.NALUType(avcc) == h265.NALUTypeVPS {
				codec = h265.AVCCToCodec(avcc)
			} else {
				codec = h264.AVCCToCodec(avcc)
			}
			mux = &mp4.Muxer{}
			mux.AddTrack(codec)
			init, err := mux.GetInit()
			if err != nil {
				return err
			}
			w.Header().Set("Content-Type", "video/mp4")
			w.Header().Set("Content-Disposition", `attachment; filename="`+name+`"`)
			w.WriteHeader(200)
			if _, err := w.Write(init); err != nil {
				return err
			}
			started, pts0 = true, pts
		}
		if pts-pts0 > limit {
			return io.EOF // reached the end of the clip
		}
		pkt := &rtp.Packet{Header: rtp.Header{Timestamp: uint32(pts - pts0)}, Payload: annexb.EncodeToAVCC(au)}
		_, err := w.Write(mux.GetPayload(0, pkt))
		return err
	})
	if !started {
		fallback("no video in NVR download")
		return
	}
	if err != nil && err != io.EOF && r.Context().Err() == nil {
		log.Println("download:", err)
	}
}
