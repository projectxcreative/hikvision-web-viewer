// cctv-server: the front door for hikvision-web-viewer.
//
// Copyright (C) 2026 Project X Creative
// Licensed under the GNU General Public License v3.0 or later. See LICENSE.
//
//   - serves the viewer page (www/)
//   - keeps the NVR login on the server (data/settings.json), never in the browser
//   - proxies the video API to go2rtc, turning "nvr:/Streaming/..." stream names into
//     full RTSP URLs with the login, and blocking every other go2rtc API
//   - answers clip searches / calendar lookups by talking ISAPI to the NVR
//   - optional viewer password (HTTP basic auth)
//
// Settings come from environment variables:
//
//	LISTEN           address to serve on        (default 127.0.0.1:1984)
//	GO2RTC           go2rtc API base URL         (default http://127.0.0.1:1986)
//	WWW_DIR          viewer page folder          (default ./www)
//	DATA_DIR         where settings are saved    (default ./data)
//	VIEWER_PASSWORD  if set, viewers must log in (user name "viewer")
package main

import (
	"crypto/subtle"
	"encoding/json"
	"io"
	"log"
	"net"
	"net/http"
	"net/http/httputil"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"sync"
)

var (
	listenAddr = env("LISTEN", "127.0.0.1:1984")
	go2rtcURL  = env("GO2RTC", "http://127.0.0.1:1986")
	wwwDir     = env("WWW_DIR", "www")
	dataDir    = env("DATA_DIR", "data")
	viewerPass = os.Getenv("VIEWER_PASSWORD")
)

func env(k, def string) string {
	if v := os.Getenv(k); v != "" {
		return v
	}
	return def
}

// ---- settings ----

type Camera struct {
	Name string `json:"name"`
	Ch   int    `json:"ch"`
}

type Settings struct {
	Host string   `json:"host"`
	Port int      `json:"port"` // RTSP
	User string   `json:"user"`
	Pass string   `json:"pass,omitempty"`
	Cams []Camera `json:"cams"`
}

var (
	setMu sync.RWMutex
	set   = Settings{Port: 554, Cams: []Camera{
		{"Camera 1", 1}, {"Camera 2", 2}, {"Camera 3", 3}, {"Camera 4", 4}}}
)

func settingsPath() string { return filepath.Join(dataDir, "settings.json") }

func loadSettings() {
	b, err := os.ReadFile(settingsPath())
	if err != nil {
		return
	}
	var s Settings
	if json.Unmarshal(b, &s) == nil && len(s.Cams) > 0 {
		set = s
	}
}

func saveSettings(s Settings) error {
	os.MkdirAll(dataDir, 0o700)
	b, _ := json.MarshalIndent(s, "", "  ")
	tmp := settingsPath() + ".tmp"
	if err := os.WriteFile(tmp, b, 0o600); err != nil {
		return err
	}
	return os.Rename(tmp, settingsPath())
}

func getSettings() Settings {
	setMu.RLock()
	defer setMu.RUnlock()
	return set
}

func nvrLogin() searchReq {
	s := getSettings()
	return searchReq{Host: s.Host, User: s.User, Pass: s.Pass}
}

func handleCfg(w http.ResponseWriter, r *http.Request) {
	switch r.Method {
	case http.MethodGet:
		s := getSettings()
		writeJSON(w, 200, map[string]any{
			"host": s.Host, "port": s.Port, "user": s.User, "cams": s.Cams,
			"hasLogin": s.User != "" && s.Pass != "",
		})
	case http.MethodPost:
		if !sameOrigin(r) {
			http.Error(w, "forbidden", 403)
			return
		}
		var in Settings
		if err := json.NewDecoder(io.LimitReader(r.Body, 1<<16)).Decode(&in); err != nil {
			http.Error(w, "bad request", 400)
			return
		}
		in.Host = strings.TrimSpace(in.Host)
		if !privateHost(in.Host) {
			writeJSON(w, 400, map[string]string{"error": "NVR address must be on your local network"})
			return
		}
		if in.Port <= 0 || in.Port > 65535 {
			in.Port = 554
		}
		setMu.Lock()
		if in.Pass == "" { // blank = keep the saved password
			in.Pass = set.Pass
		}
		if len(in.Cams) == 0 {
			in.Cams = set.Cams
		}
		err := saveSettings(in)
		if err == nil {
			set = in
		}
		setMu.Unlock()
		if err != nil {
			writeJSON(w, 500, map[string]string{"error": err.Error()})
			return
		}
		writeJSON(w, 200, map[string]bool{"ok": true})
	default:
		http.Error(w, "method not allowed", 405)
	}
}

// ---- go2rtc proxy ----

var (
	reNvrPath  = regexp.MustCompile(`^/Streaming/(Channels|tracks)/\d+/?(\?[\w=&%.:-]*)?$`)
	allowedAPI = map[string]bool{"/api": true, "/api/ws": true, "/api/stream.mp4": true}
)

// rewriteSrc turns the page's stream names into real sources. Anything else is refused,
// so go2rtc can't be used to open arbitrary URLs.
func rewriteSrc(src string) (string, bool) {
	s := getSettings()
	switch {
	case strings.HasPrefix(src, "nvr:"):
		p := strings.TrimPrefix(src, "nvr:")
		if !reNvrPath.MatchString(p) || s.User == "" {
			return "", false
		}
		u := url.URL{Scheme: "rtsp", User: url.UserPassword(s.User, s.Pass), Host: net.JoinHostPort(s.Host, itoa(s.Port))}
		return u.String() + p, true
	case strings.HasPrefix(src, "fast:"):
		q, err := url.ParseQuery(strings.TrimPrefix(src, "fast:"))
		if err != nil {
			return "", false
		}
		return "http://" + loopbackAddr() + "/fast?" + q.Encode(), true
	}
	return "", false
}

func loopbackAddr() string {
	_, port, _ := net.SplitHostPort(listenAddr)
	return "127.0.0.1:" + port
}

func newProxy() http.Handler {
	target, err := url.Parse(go2rtcURL)
	if err != nil {
		log.Fatal(err)
	}
	rp := httputil.NewSingleHostReverseProxy(target)
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !allowedAPI[r.URL.Path] || r.Method != http.MethodGet {
			http.Error(w, "not available", 404)
			return
		}
		q := r.URL.Query()
		if src := q.Get("src"); src != "" || r.URL.Path != "/api" {
			real, ok := rewriteSrc(src)
			if !ok {
				http.Error(w, "unknown stream", 400)
				return
			}
			q.Set("src", real)
			r.URL.RawQuery = q.Encode()
		}
		r.Header.Del("Authorization")
		rp.ServeHTTP(w, r)
	})
}

// ---- auth & helpers ----

func withAuth(h http.Handler) http.Handler {
	if viewerPass == "" {
		return h
	}
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// go2rtc (on this machine) fetches /fast without a login
		if r.URL.Path == "/fast" && isLoopback(r) {
			h.ServeHTTP(w, r)
			return
		}
		u, p, ok := r.BasicAuth()
		if !ok || subtle.ConstantTimeCompare([]byte(u), []byte("viewer")) != 1 ||
			subtle.ConstantTimeCompare([]byte(p), []byte(viewerPass)) != 1 {
			w.Header().Set("WWW-Authenticate", `Basic realm="CCTV Viewer", charset="UTF-8"`)
			http.Error(w, "login required", 401)
			return
		}
		h.ServeHTTP(w, r)
	})
}

func isLoopback(r *http.Request) bool {
	host, _, _ := net.SplitHostPort(r.RemoteAddr)
	ip := net.ParseIP(host)
	return ip != nil && ip.IsLoopback()
}

// POSTs must come from our own page (blocks other websites poking the server)
func sameOrigin(r *http.Request) bool {
	o := r.Header.Get("Origin")
	if o == "" {
		return false
	}
	u, err := url.Parse(o)
	return err == nil && u.Host == r.Host
}

func writeJSON(w http.ResponseWriter, code int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Cache-Control", "no-store")
	w.WriteHeader(code)
	json.NewEncoder(w).Encode(v)
}

func itoa(i int) string { return strconv.Itoa(i) }

func main() {
	loadSettings()
	if viewerPass == "change-me" {
		log.Fatal("VIEWER_PASSWORD is still 'change-me' - set a real password (or remove it to run without a login)")
	}
	mux := http.NewServeMux()
	mux.HandleFunc("/cfg", handleCfg)
	mux.HandleFunc("/search", handleSearch)
	mux.HandleFunc("/month", handleMonth)
	mux.HandleFunc("/fast", handleFast)
	mux.HandleFunc("/download", handleDownload)
	mux.Handle("/api", newProxy())
	mux.Handle("/api/", newProxy())
	fs := http.FileServer(http.Dir(wwwDir))
	mux.Handle("/", http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Cache-Control", "no-cache")
		fs.ServeHTTP(w, r)
	}))
	log.Printf("CCTV viewer on http://%s  (go2rtc %s, login %v)", listenAddr, go2rtcURL, viewerPass != "")
	log.Fatal(http.ListenAndServe(listenAddr, withAuth(mux)))
}
