package main

import (
	"bytes"
	"crypto/md5"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"encoding/xml"
	"fmt"
	"io"
	"net"
	"net/http"
	"regexp"
	"strings"
	"time"
)

type searchReq struct {
	Host string
	User string
	Pass string
}

type clip struct {
	Track int    `json:"track"`
	Start string `json:"start"` // NVR-local "2026-11-11T09:00:00"
	End   string `json:"end"`
	Type  string `json:"type"`
	URI   string `json:"uri"` // RTSP path+query without host/credentials
}

type cmResult struct {
	Status  string `xml:"responseStatusStrg"`
	Matches []struct {
		TrackID  int `xml:"trackID"`
		TimeSpan struct {
			Start string `xml:"startTime"`
			End   string `xml:"endTime"`
		} `xml:"timeSpan"`
		Media struct {
			URI string `xml:"playbackURI"`
		} `xml:"mediaSegmentDescriptor"`
		Meta struct {
			Desc string `xml:"metadataDescriptor"`
		} `xml:"metadataMatches"`
	} `xml:"matchList>searchMatchItem"`
}

func readPost(w http.ResponseWriter, r *http.Request, v any) (searchReq, bool) {
	if r.Method != http.MethodPost || !sameOrigin(r) {
		http.Error(w, "POST from the viewer page only", http.StatusMethodNotAllowed)
		return searchReq{}, false
	}
	if err := json.NewDecoder(io.LimitReader(r.Body, 1<<16)).Decode(v); err != nil {
		http.Error(w, "bad request", 400)
		return searchReq{}, false
	}
	lg := nvrLogin()
	if lg.User == "" || lg.Pass == "" {
		writeJSON(w, 409, map[string]string{"error": "Enter the NVR login in Settings"})
		return lg, false
	}
	if !privateHost(lg.Host) {
		writeJSON(w, 400, map[string]string{"error": "NVR address must be on your local network"})
		return lg, false
	}
	return lg, true
}

func handleSearch(w http.ResponseWriter, r *http.Request) {
	var q struct {
		Tracks []int  `json:"tracks"`
		Start  string `json:"start"`
		End    string `json:"end"`
	}
	lg, ok := readPost(w, r, &q)
	if !ok {
		return
	}
	all := []clip{}
	for _, t := range q.Tracks {
		clips, err := searchTrack(lg, t, q.Start, q.End)
		if err != nil {
			writeJSON(w, 502, map[string]string{"error": err.Error()})
			return
		}
		all = append(all, clips...)
	}
	writeJSON(w, 200, map[string]any{"clips": all})
}

func handleMonth(w http.ResponseWriter, r *http.Request) {
	var q struct {
		Tracks []int `json:"tracks"`
		Year   int   `json:"year"`
		Month  int   `json:"month"`
	}
	lg, ok := readPost(w, r, &q)
	if !ok {
		return
	}
	if q.Month < 1 || q.Month > 12 || q.Year < 2000 || q.Year > 2100 {
		http.Error(w, "bad request", 400)
		return
	}
	days := map[int][]int{} // day of month -> tracks with recordings
	for _, t := range q.Tracks {
		body := fmt.Sprintf(`<?xml version="1.0" encoding="UTF-8"?><trackDailyParam><year>%d</year><monthOfYear>%d</monthOfYear></trackDailyParam>`, q.Year, q.Month)
		b, err := digestPost(lg, fmt.Sprintf("/ISAPI/ContentMgmt/record/tracks/%d/dailyDistribution", t), []byte(body))
		if err != nil {
			writeJSON(w, 502, map[string]string{"error": err.Error()})
			return
		}
		var res struct {
			Days []struct {
				Day    int    `xml:"dayOfMonth"`
				Record string `xml:"record"`
			} `xml:"dayList>day"`
		}
		xml.Unmarshal(b, &res)
		for _, d := range res.Days {
			if strings.EqualFold(strings.TrimSpace(d.Record), "true") {
				days[d.Day] = append(days[d.Day], t)
			}
		}
	}
	writeJSON(w, 200, map[string]any{"days": days})
}

var reHost = regexp.MustCompile(`^[a-zA-Z0-9.\-]+(:\d+)?$`)

func privateHost(h string) bool {
	if !reHost.MatchString(h) {
		return false
	}
	host := h
	if hh, _, err := net.SplitHostPort(h); err == nil {
		host = hh
	}
	ips, err := net.LookupIP(host)
	if err != nil || len(ips) == 0 {
		return false
	}
	for _, ip := range ips {
		if !(ip.IsPrivate() || ip.IsLoopback() || ip.IsLinkLocalUnicast()) {
			return false
		}
	}
	return true
}

func searchTrack(q searchReq, track int, start, end string) ([]clip, error) {
	var out []clip
	id := newUUID()
	for pos := 0; pos < 4000; {
		body := fmt.Sprintf(`<?xml version="1.0" encoding="UTF-8"?>
<CMSearchDescription>
<searchID>%s</searchID>
<trackList><trackID>%d</trackID></trackList>
<timeSpanList><timeSpan><startTime>%sZ</startTime><endTime>%sZ</endTime></timeSpan></timeSpanList>
<maxResults>100</maxResults>
<searchResultPostion>%d</searchResultPostion>
<metadataList><metadataDescriptor>//recordType.meta.std-cgi.com</metadataDescriptor></metadataList>
</CMSearchDescription>`, id, track, clean(start), clean(end), pos)
		resp, err := digestPost(q, "/ISAPI/ContentMgmt/search", []byte(body))
		if err != nil {
			return nil, err
		}
		var res cmResult
		if err := xml.Unmarshal(resp, &res); err != nil {
			return nil, fmt.Errorf("unexpected reply from NVR: %.200s", string(resp))
		}
		for _, m := range res.Matches {
			c := clip{
				Track: track,
				Start: strings.TrimSuffix(m.TimeSpan.Start, "Z"),
				End:   strings.TrimSuffix(m.TimeSpan.End, "Z"),
				Type:  recType(m.Meta.Desc),
			}
			if i := strings.Index(m.Media.URI, "/Streaming/"); i >= 0 {
				c.URI = m.Media.URI[i:]
			}
			out = append(out, c)
		}
		pos += len(res.Matches)
		if res.Status != "MORE" || len(res.Matches) == 0 {
			break
		}
	}
	return out, nil
}

func recType(d string) string {
	d = strings.ToLower(d)
	if i := strings.LastIndex(d, "/"); i >= 0 {
		d = d[i+1:]
	}
	if d == "" {
		return "recording"
	}
	return d
}

var reTime = regexp.MustCompile(`^\d{4}-\d{2}-\d{2}T\d{2}:\d{2}:\d{2}$`)

func clean(t string) string {
	if reTime.MatchString(t) {
		return t
	}
	return time.Now().Format("2006-01-02T15:04:05")
}

func newUUID() string {
	b := make([]byte, 16)
	rand.Read(b)
	b[6] = b[6]&0x0f | 0x40
	b[8] = b[8]&0x3f | 0x80
	return fmt.Sprintf("%X-%X-%X-%X-%X", b[0:4], b[4:6], b[6:8], b[8:10], b[10:])
}

// ---- HTTP digest (falls back to basic) ----

var client = &http.Client{Timeout: 20 * time.Second}

type doer interface {
	Do(*http.Request) (*http.Response, error)
}

func digestPost(q searchReq, path string, body []byte) ([]byte, error) {
	resp, err := digestDo(client, q, "POST", path, body)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(io.LimitReader(resp.Body, 8<<20))
	if resp.StatusCode == 401 {
		return nil, fmt.Errorf("NVR rejected the username or password")
	}
	if resp.StatusCode != 200 {
		return nil, fmt.Errorf("NVR returned %d: %.200s", resp.StatusCode, string(b))
	}
	return b, nil
}

// digestDo sends a request, answering a digest (or basic) challenge. Caller closes the body.
func digestDo(c doer, q searchReq, method, path string, body []byte) (*http.Response, error) {
	u := "http://" + q.Host + path
	req, _ := http.NewRequest(method, u, bytes.NewReader(body))
	req.Header.Set("Content-Type", "application/xml")
	resp, err := c.Do(req)
	if err != nil {
		return nil, fmt.Errorf("cannot reach NVR: %v", err)
	}
	if resp.StatusCode != 401 {
		return resp, nil
	}
	io.Copy(io.Discard, resp.Body)
	resp.Body.Close()
	auth := ""
	for _, ch := range resp.Header.Values("WWW-Authenticate") {
		if strings.HasPrefix(strings.ToLower(ch), "digest") {
			auth = digestHeader(ch, q.User, q.Pass, method, path)
			break
		}
	}
	req, _ = http.NewRequest(method, u, bytes.NewReader(body))
	req.Header.Set("Content-Type", "application/xml")
	if auth != "" {
		req.Header.Set("Authorization", auth)
	} else {
		req.SetBasicAuth(q.User, q.Pass)
	}
	resp, err = c.Do(req)
	if err != nil {
		return nil, fmt.Errorf("cannot reach NVR: %v", err)
	}
	return resp, nil
}

var reParam = regexp.MustCompile(`(\w+)=("([^"]*)"|[^,\s]*)`)

func digestHeader(chal, user, pass, method, uri string) string {
	p := map[string]string{}
	for _, m := range reParam.FindAllStringSubmatch(chal, -1) {
		v := m[2]
		if strings.HasPrefix(v, `"`) {
			v = m[3]
		}
		p[strings.ToLower(m[1])] = v
	}
	h := func(s string) string { x := md5.Sum([]byte(s)); return hex.EncodeToString(x[:]) }
	ha1 := h(user + ":" + p["realm"] + ":" + pass)
	ha2 := h(method + ":" + uri)
	cb := make([]byte, 8)
	rand.Read(cb)
	cnonce := hex.EncodeToString(cb)
	nc := "00000001"
	var resp, qopPart string
	if strings.Contains(p["qop"], "auth") {
		resp = h(ha1 + ":" + p["nonce"] + ":" + nc + ":" + cnonce + ":auth:" + ha2)
		qopPart = fmt.Sprintf(`, qop=auth, nc=%s, cnonce="%s"`, nc, cnonce)
	} else {
		resp = h(ha1 + ":" + p["nonce"] + ":" + ha2)
	}
	s := fmt.Sprintf(`Digest username="%s", realm="%s", nonce="%s", uri="%s", response="%s"%s`,
		user, p["realm"], p["nonce"], uri, resp, qopPart)
	if p["opaque"] != "" {
		s += fmt.Sprintf(`, opaque="%s"`, p["opaque"])
	}
	return s
}
