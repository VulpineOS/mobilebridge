package mobilebridge

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"sync"
	"time"

	"github.com/gorilla/websocket"
)

// Server exposes Chrome-compatible /json endpoints and a /devtools/page/<id>
// WebSocket endpoint that proxies to an Android device's Chrome over ADB.
type Server struct {
	serial string
	addr   string

	mu       sync.Mutex
	httpSrv  *http.Server
	upgrader websocket.Upgrader
}

// NewServer constructs a Server bound to addr (e.g. "127.0.0.1:9222") that
// will proxy CDP traffic to the named device.
func NewServer(serial string, addr string) *Server {
	return &Server{
		serial:   serial,
		addr:     addr,
		upgrader: websocket.Upgrader{CheckOrigin: func(*http.Request) bool { return true }},
	}
}

// Start begins listening. It returns once the listener is accepting connections
// (or immediately on bind failure).
func (s *Server) Start() error {
	mux := http.NewServeMux()
	mux.HandleFunc("/json/version", s.handleVersion)
	mux.HandleFunc("/json/list", s.handleList)
	mux.HandleFunc("/json", s.handleList)
	mux.HandleFunc("/json/new", s.handleNew)
	mux.HandleFunc("/devtools/page/", s.handleWebSocket)
	mux.HandleFunc("/devtools/browser", s.handleWebSocket)
	mux.HandleFunc("/health", func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.WriteString(w, "ok")
	})

	ln, err := net.Listen("tcp", s.addr)
	if err != nil {
		return err
	}

	srv := &http.Server{
		Handler:           mux,
		ReadHeaderTimeout: 10 * time.Second,
	}
	s.mu.Lock()
	s.httpSrv = srv
	s.mu.Unlock()

	go func() {
		_ = srv.Serve(ln)
	}()
	return nil
}

// Stop shuts the HTTP server down with a short grace period.
func (s *Server) Stop() error {
	s.mu.Lock()
	srv := s.httpSrv
	s.mu.Unlock()
	if srv == nil {
		return nil
	}
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	return srv.Shutdown(ctx)
}

// handleVersion proxies Chrome's /json/version from the device via the adb
// forward, so clients see a real browser descriptor.
func (s *Server) handleVersion(w http.ResponseWriter, r *http.Request) {
	s.proxyJSON(w, "/json/version")
}

func (s *Server) handleList(w http.ResponseWriter, r *http.Request) {
	s.proxyJSON(w, "/json/list")
}

func (s *Server) handleNew(w http.ResponseWriter, r *http.Request) {
	url := "/json/new"
	if q := r.URL.Query().Get("url"); q != "" {
		url += "?" + r.URL.RawQuery
	}
	s.proxyJSON(w, url)
}

// proxyJSON forwards a GET to the local ADB-forwarded port (set up by the
// caller, typically via NewProxy). This is a best-effort helper: if no proxy
// has been wired up yet it returns 503.
func (s *Server) proxyJSON(w http.ResponseWriter, path string) {
	_, port, err := net.SplitHostPort(s.addr)
	if err != nil {
		http.Error(w, "bad server addr: "+err.Error(), http.StatusInternalServerError)
		return
	}
	// NOTE: by default we assume the adb-forwarded Chrome lives on the same
	// port as we serve on, which will not be true in production usage. A
	// real caller wires a Proxy into the server (see RunWithProxy) to expose
	// the forwarded port directly. The server's /json endpoints in that
	// case are provided by the downstream Chrome itself.
	_ = port
	w.Header().Set("Content-Type", "application/json")
	resp := map[string]string{
		"Browser": "mobilebridge (not yet attached)",
		"error":   "proxy not attached; call RunWithProxy",
	}
	b, _ := json.Marshal(resp)
	w.WriteHeader(http.StatusServiceUnavailable)
	_, _ = w.Write(b)
}

func (s *Server) handleWebSocket(w http.ResponseWriter, r *http.Request) {
	http.Error(w, "mobilebridge: websocket endpoint requires RunWithProxy wiring", http.StatusServiceUnavailable)
}

// RunWithProxy is a convenience that ties together a Proxy (which holds the
// upstream Chrome connection) and a Server's HTTP surface. It rewires the
// /json and /devtools handlers to talk to the given proxy's local forward
// port and upstream WebSocket.
//
// Call Start before RunWithProxy.
func (s *Server) RunWithProxy(p *Proxy) error {
	if p == nil {
		return errors.New("mobilebridge: nil proxy")
	}
	s.mu.Lock()
	srv := s.httpSrv
	s.mu.Unlock()
	if srv == nil {
		return errors.New("mobilebridge: server not started")
	}

	mux := http.NewServeMux()
	base := fmt.Sprintf("http://127.0.0.1:%d", p.localPort)

	// publicHost is the host:port clients see. If s.addr binds 0.0.0.0 or
	// omits a host, we default to 127.0.0.1 so rewritten URLs are usable.
	publicHost := s.addr
	if host, port, err := net.SplitHostPort(s.addr); err == nil {
		if host == "" || host == "0.0.0.0" || host == "::" {
			host = "127.0.0.1"
		}
		publicHost = net.JoinHostPort(host, port)
	}

	forwardJSON := func(path string) http.HandlerFunc {
		return func(w http.ResponseWriter, r *http.Request) {
			u := base + path
			if r.URL.RawQuery != "" {
				u += "?" + r.URL.RawQuery
			}
			client := &http.Client{Timeout: 5 * time.Second}
			resp, err := client.Get(u)
			if err != nil {
				http.Error(w, err.Error(), http.StatusBadGateway)
				return
			}
			defer resp.Body.Close()
			body, err := io.ReadAll(resp.Body)
			if err != nil {
				http.Error(w, err.Error(), http.StatusBadGateway)
				return
			}
			rewritten := rewriteDevtoolsJSON(body, publicHost)
			for k, vv := range resp.Header {
				if k == "Content-Length" {
					continue
				}
				for _, v := range vv {
					w.Header().Add(k, v)
				}
			}
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(resp.StatusCode)
			_, _ = w.Write(rewritten)
		}
	}

	mux.HandleFunc("/json/version", forwardJSON("/json/version"))
	mux.HandleFunc("/json/list", forwardJSON("/json/list"))
	mux.HandleFunc("/json", forwardJSON("/json"))
	mux.HandleFunc("/json/new", forwardJSON("/json/new"))
	mux.HandleFunc("/health", func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.WriteString(w, "ok")
	})

	// WebSocket: accept a downstream client, then hand to proxy.Serve. Note
	// that each inbound websocket consumes the single upstream owned by the
	// proxy; concurrent clients are not supported in this MVP.
	mux.HandleFunc("/devtools/page/", func(w http.ResponseWriter, r *http.Request) {
		ws, err := s.upgrader.Upgrade(w, r, nil)
		if err != nil {
			return
		}
		defer ws.Close()
		_ = p.Serve(ws)
	})
	mux.HandleFunc("/devtools/browser", func(w http.ResponseWriter, r *http.Request) {
		ws, err := s.upgrader.Upgrade(w, r, nil)
		if err != nil {
			return
		}
		defer ws.Close()
		_ = p.Serve(ws)
	})

	srv.Handler = mux
	return nil
}

// rewriteDevtoolsJSON takes a raw /json/version or /json/list body from the
// upstream Chrome (served via the adb forward on 127.0.0.1:<localPort>) and
// rewrites any webSocketDebuggerUrl / devtoolsFrontendUrl fields so they
// point back at this server's publicHost. That ensures CDP clients which
// follow those URLs (Puppeteer, chrome-remote-interface) keep their traffic
// going through mobilebridge instead of bypassing it.
//
// If the body isn't JSON we recognise, it's returned unchanged.
func rewriteDevtoolsJSON(body []byte, publicHost string) []byte {
	trimmed := bytesTrimSpace(body)
	if len(trimmed) == 0 {
		return body
	}
	switch trimmed[0] {
	case '[':
		var arr []map[string]interface{}
		if err := json.Unmarshal(body, &arr); err != nil {
			return body
		}
		for _, entry := range arr {
			rewriteEntry(entry, publicHost)
		}
		out, err := json.Marshal(arr)
		if err != nil {
			return body
		}
		return out
	case '{':
		var obj map[string]interface{}
		if err := json.Unmarshal(body, &obj); err != nil {
			return body
		}
		rewriteEntry(obj, publicHost)
		out, err := json.Marshal(obj)
		if err != nil {
			return body
		}
		return out
	}
	return body
}

func rewriteEntry(entry map[string]interface{}, publicHost string) {
	if v, ok := entry["webSocketDebuggerUrl"].(string); ok {
		entry["webSocketDebuggerUrl"] = rewriteWSURL(v, publicHost)
	}
	if v, ok := entry["devtoolsFrontendUrl"].(string); ok {
		entry["devtoolsFrontendUrl"] = rewriteFrontendURL(v, publicHost)
	}
}

// rewriteWSURL replaces the host component of a ws:// URL with publicHost,
// leaving scheme/path intact.
func rewriteWSURL(raw, publicHost string) string {
	idx := indexOf(raw, "://")
	if idx < 0 {
		return raw
	}
	scheme := raw[:idx]
	rest := raw[idx+3:]
	slash := indexOfByte(rest, '/')
	if slash < 0 {
		return scheme + "://" + publicHost
	}
	return scheme + "://" + publicHost + rest[slash:]
}

// rewriteFrontendURL rewrites the `ws=host:port` query parameter Chrome
// embeds in devtoolsFrontendUrl so opening the inspector routes through us.
func rewriteFrontendURL(raw, publicHost string) string {
	i := indexOf(raw, "ws=")
	if i < 0 {
		return raw
	}
	prefix := raw[:i+3]
	rest := raw[i+3:]
	end := indexOfByte(rest, '/')
	amp := indexOfByte(rest, '&')
	cut := end
	if amp >= 0 && (cut < 0 || amp < cut) {
		cut = amp
	}
	if cut < 0 {
		return prefix + publicHost
	}
	return prefix + publicHost + rest[cut:]
}

func indexOf(s, sub string) int {
	for i := 0; i+len(sub) <= len(s); i++ {
		if s[i:i+len(sub)] == sub {
			return i
		}
	}
	return -1
}

func indexOfByte(s string, b byte) int {
	for i := 0; i < len(s); i++ {
		if s[i] == b {
			return i
		}
	}
	return -1
}

func bytesTrimSpace(b []byte) []byte {
	i := 0
	for i < len(b) && (b[i] == ' ' || b[i] == '\t' || b[i] == '\n' || b[i] == '\r') {
		i++
	}
	return b[i:]
}
