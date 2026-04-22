package mobilebridge

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

func TestStartAttachedServerWithADBPort(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `[{"id":"X","webSocketDebuggerUrl":"ws://127.0.0.1:9999/devtools/page/X"}]`)
	}))
	defer upstream.Close()

	upHost := strings.TrimPrefix(upstream.URL, "http://")
	upPort := 0
	if _, err := fmtSscanfPort(upHost, &upPort); err != nil {
		t.Fatalf("parse upstream port: %v", err)
	}
	serverPort, err := freeTCPPort()
	if err != nil {
		t.Fatalf("free server port: %v", err)
	}

	oldNewProxy := newProxyForAttachedServer
	defer func() { newProxyForAttachedServer = oldNewProxy }()
	newProxyForAttachedServer = func(ctx context.Context, serial string, localPort int) (*Proxy, error) {
		if serial != "fake-serial" {
			t.Fatalf("serial = %q, want fake-serial", serial)
		}
		if localPort != 4567 {
			t.Fatalf("adb port = %d, want 4567", localPort)
		}
		return &Proxy{
			localPort: upPort,
			closed:    make(chan struct{}),
			done:      make(chan struct{}),
		}, nil
	}

	addr := fmt.Sprintf("127.0.0.1:%d", serverPort)
	session, err := StartAttachedServerWithADBPort(context.Background(), "fake-serial", 4567, addr)
	if err != nil {
		t.Fatalf("start attached server: %v", err)
	}
	if session.Endpoint != "http://"+addr {
		t.Fatalf("endpoint = %q, want http://%s", session.Endpoint, addr)
	}

	resp, err := http.Get(session.Endpoint + "/json/list")
	if err != nil {
		t.Fatalf("get list: %v", err)
	}
	_, _ = io.ReadAll(resp.Body)
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200", resp.StatusCode)
	}

	if err := session.Close(); err != nil {
		t.Fatalf("close: %v", err)
	}
	select {
	case <-session.Done():
	default:
		t.Fatalf("session done channel was not closed")
	}
}

// TestJsonListRewrite spins up a fake upstream Chrome that serves a
// synthetic /json/list, wires it into a Server via RunWithProxy, and
// verifies the proxied response has its webSocketDebuggerUrl and
// devtoolsFrontendUrl rewritten to point at the server's public host.
func TestJsonListRewrite(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `[{
			"id": "ABC123",
			"title": "Example",
			"type": "page",
			"url": "https://example.com/",
			"webSocketDebuggerUrl": "ws://127.0.0.1:9999/devtools/page/ABC123",
			"devtoolsFrontendUrl": "/devtools/inspector.html?ws=127.0.0.1:9999/devtools/page/ABC123"
		}]`)
	}))
	defer upstream.Close()

	// Parse upstream port so the Proxy struct thinks that's the adb-forwarded port.
	upHost := strings.TrimPrefix(upstream.URL, "http://")
	upPort := 0
	if _, err := fmtSscanfPort(upHost, &upPort); err != nil {
		t.Fatalf("parse upstream port: %v", err)
	}

	p := &Proxy{localPort: upPort}
	serverPort, err := freeTCPPort()
	if err != nil {
		t.Fatalf("free server port: %v", err)
	}
	serverAddr := fmt.Sprintf("127.0.0.1:%d", serverPort)
	baseURL := "http://" + serverAddr
	s := NewServer("fake-serial", serverAddr)
	if err := s.Start(); err != nil {
		t.Fatalf("start server: %v", err)
	}
	defer s.Stop()
	if err := s.RunWithProxy(p); err != nil {
		t.Fatalf("run with proxy: %v", err)
	}

	resp, err := http.Get(baseURL + "/json/list")
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)

	var entries []map[string]interface{}
	if err := json.Unmarshal(body, &entries); err != nil {
		t.Fatalf("unmarshal: %v body=%s", err, body)
	}
	if len(entries) != 1 {
		t.Fatalf("want 1 entry, got %d", len(entries))
	}
	ws, _ := entries[0]["webSocketDebuggerUrl"].(string)
	if ws != fmt.Sprintf("ws://%s/devtools/page/ABC123", serverAddr) {
		t.Errorf("webSocketDebuggerUrl not rewritten: %q", ws)
	}
	front, _ := entries[0]["devtoolsFrontendUrl"].(string)
	if !strings.Contains(front, "ws="+serverAddr+"/devtools/page/ABC123") {
		t.Errorf("devtoolsFrontendUrl not rewritten: %q", front)
	}
}

// TestRewriteFrontendURL_OnlyMatchesQueryParam ensures the `ws=` substring
// match is scoped to a real query parameter. A `ws=` substring appearing in
// the path must not get rewritten, and a `ws=` that sits in the query
// amongst other params must still be targeted.
func TestRewriteFrontendURL_OnlyMatchesQueryParam(t *testing.T) {
	// Positive: ws= is a genuine query param after other params.
	in := "/devtools/inspector.html?foo=bar&ws=oldhost:9999/devtools/page/X"
	out := rewriteFrontendURL(in, "127.0.0.1:9222")
	want := "/devtools/inspector.html?foo=bar&ws=127.0.0.1:9222/devtools/page/X"
	if out != want {
		t.Errorf("multi-param rewrite: got %q want %q", out, want)
	}

	// Negative: `ws=` in the path must not get rewritten. With no query
	// present, the function should leave the URL alone.
	in2 := "/devtools/ws=test/inspector.html"
	out2 := rewriteFrontendURL(in2, "127.0.0.1:9222")
	if out2 != in2 {
		t.Errorf("path-only ws= was mangled: got %q want %q", out2, in2)
	}

	// Negative: `ws=` in the path with an unrelated query param must not
	// rewrite the path occurrence.
	in3 := "/devtools/ws=test/inspector.html?other=thing"
	out3 := rewriteFrontendURL(in3, "127.0.0.1:9222")
	if out3 != in3 {
		t.Errorf("path ws= rewritten despite unrelated query: got %q", out3)
	}

	// Positive: ws= is the only param.
	in4 := "/devtools/inspector.html?ws=upstream:9999/devtools/page/Y"
	out4 := rewriteFrontendURL(in4, "host:1")
	want4 := "/devtools/inspector.html?ws=host:1/devtools/page/Y"
	if out4 != want4 {
		t.Errorf("single-param rewrite: got %q want %q", out4, want4)
	}
}

// TestRewriteWSURL_PreservesWss verifies that a wss:// upstream URL is NOT
// silently downgraded to ws:// when rewritten against a plaintext HTTP
// listener. The correct behavior is to return the URL unchanged so TLS
// clients fail loudly rather than accidentally handshaking against a
// plaintext server.
func TestRewriteWSURL_PreservesWss(t *testing.T) {
	in := "wss://upstream.example.com:443/devtools/page/ABC"
	out := rewriteWSURL(in, "127.0.0.1:9222")
	if out != in {
		t.Errorf("wss:// URL was rewritten (possible downgrade): got %q want %q", out, in)
	}
	// Sanity: plain ws:// still gets rewritten.
	out = rewriteWSURL("ws://upstream:9999/devtools/page/ABC", "127.0.0.1:9222")
	if out != "ws://127.0.0.1:9222/devtools/page/ABC" {
		t.Errorf("ws:// URL not rewritten: got %q", out)
	}
}

func TestRewriteDevtoolsJSONUnknownBody(t *testing.T) {
	body := []byte("not json at all")
	if out := rewriteDevtoolsJSON(body, "127.0.0.1:9222"); string(out) != "not json at all" {
		t.Errorf("non-JSON body got mutated: %q", out)
	}
}

func TestRewriteDevtoolsJSONVersion(t *testing.T) {
	body := []byte(`{"Browser":"Chrome/130","webSocketDebuggerUrl":"ws://127.0.0.1:9999/devtools/browser/XYZ"}`)
	out := rewriteDevtoolsJSON(body, "127.0.0.1:9222")
	var obj map[string]interface{}
	if err := json.Unmarshal(out, &obj); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if obj["webSocketDebuggerUrl"] != "ws://127.0.0.1:9222/devtools/browser/XYZ" {
		t.Errorf("got %v", obj["webSocketDebuggerUrl"])
	}
}

// TestJsonListCache_HitsWithin500ms verifies the /json/list cache coalesces
// repeated polls down to a single upstream GET within the TTL window.
func TestJsonListCache_HitsWithin500ms(t *testing.T) {
	var hits int32
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt32(&hits, 1)
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `[{"id":"X","title":"t","webSocketDebuggerUrl":"ws://127.0.0.1:9999/devtools/page/X"}]`)
	}))
	defer upstream.Close()

	upHost := strings.TrimPrefix(upstream.URL, "http://")
	upPort := 0
	if _, err := fmtSscanfPort(upHost, &upPort); err != nil {
		t.Fatalf("parse upstream port: %v", err)
	}
	p := &Proxy{localPort: upPort}
	serverPort, err := freeTCPPort()
	if err != nil {
		t.Fatalf("free server port: %v", err)
	}
	baseURL := fmt.Sprintf("http://127.0.0.1:%d", serverPort)
	s := NewServer("fake-serial", strings.TrimPrefix(baseURL, "http://"))
	if err := s.Start(); err != nil {
		t.Fatalf("start: %v", err)
	}
	defer s.Stop()
	if err := s.RunWithProxy(p); err != nil {
		t.Fatalf("wire: %v", err)
	}

	// Hammer /json/list 20 times within the TTL window.
	for i := 0; i < 20; i++ {
		resp, err := http.Get(baseURL + "/json/list")
		if err != nil {
			t.Fatalf("get %d: %v", i, err)
		}
		_, _ = io.ReadAll(resp.Body)
		resp.Body.Close()
	}
	if got := atomic.LoadInt32(&hits); got != 1 {
		t.Errorf("upstream hits = %d, want 1 (cache should coalesce)", got)
	}

	// After the TTL, a fresh request should hit upstream again.
	time.Sleep(jsonListCacheTTL + 50*time.Millisecond)
	resp, err := http.Get(baseURL + "/json/list")
	if err != nil {
		t.Fatalf("get after ttl: %v", err)
	}
	_, _ = io.ReadAll(resp.Body)
	resp.Body.Close()
	if got := atomic.LoadInt32(&hits); got != 2 {
		t.Errorf("upstream hits after ttl = %d, want 2", got)
	}
}

// TestJsonListCache_DoesNotCacheErrors verifies a non-200 upstream response
// is not cached: a follow-up request must refetch upstream rather than
// serving the cached error for up to jsonListCacheTTL.
func TestJsonListCache_DoesNotCacheErrors(t *testing.T) {
	var hits int32
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt32(&hits, 1)
		http.Error(w, "upstream down", http.StatusServiceUnavailable)
	}))
	defer upstream.Close()

	upHost := strings.TrimPrefix(upstream.URL, "http://")
	upPort := 0
	if _, err := fmtSscanfPort(upHost, &upPort); err != nil {
		t.Fatalf("parse upstream port: %v", err)
	}
	p := &Proxy{localPort: upPort}
	serverPort, err := freeTCPPort()
	if err != nil {
		t.Fatalf("free server port: %v", err)
	}
	baseURL := fmt.Sprintf("http://127.0.0.1:%d", serverPort)
	s := NewServer("fake-serial", strings.TrimPrefix(baseURL, "http://"))
	if err := s.Start(); err != nil {
		t.Fatalf("start: %v", err)
	}
	defer s.Stop()
	if err := s.RunWithProxy(p); err != nil {
		t.Fatalf("wire: %v", err)
	}

	// Three hits; each should miss cache because 503 is not cacheable.
	for i := 0; i < 3; i++ {
		resp, err := http.Get(baseURL + "/json/list")
		if err != nil {
			t.Fatalf("get %d: %v", i, err)
		}
		_, _ = io.ReadAll(resp.Body)
		resp.Body.Close()
	}
	if got := atomic.LoadInt32(&hits); got != 3 {
		t.Errorf("error responses were cached: upstream hits = %d, want 3", got)
	}

	// Cache buffer must remain empty.
	s.listCacheMu.Lock()
	cached := s.listCacheBuf
	s.listCacheMu.Unlock()
	if cached != nil {
		t.Errorf("cache buffer populated with error body: %q", cached)
	}
}

// TestJsonListCache_InvalidatedOnDeviceChange ensures RunWithProxy wipes
// the /json/list cache so the first request after attaching a new device's
// proxy always refetches upstream instead of serving the previous device's
// tab list for up to jsonListCacheTTL.
func TestJsonListCache_InvalidatedOnDeviceChange(t *testing.T) {
	var hits int32
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt32(&hits, 1)
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `[{"id":"X","title":"t","webSocketDebuggerUrl":"ws://127.0.0.1:9999/devtools/page/X"}]`)
	}))
	defer upstream.Close()

	upHost := strings.TrimPrefix(upstream.URL, "http://")
	upPort := 0
	if _, err := fmtSscanfPort(upHost, &upPort); err != nil {
		t.Fatalf("parse upstream port: %v", err)
	}
	p := &Proxy{localPort: upPort}
	serverPort, err := freeTCPPort()
	if err != nil {
		t.Fatalf("free server port: %v", err)
	}
	baseURL := fmt.Sprintf("http://127.0.0.1:%d", serverPort)
	s := NewServer("fake-serial", strings.TrimPrefix(baseURL, "http://"))
	if err := s.Start(); err != nil {
		t.Fatalf("start: %v", err)
	}
	defer s.Stop()
	if err := s.RunWithProxy(p); err != nil {
		t.Fatalf("wire: %v", err)
	}

	// Populate the cache.
	resp, err := http.Get(baseURL + "/json/list")
	if err != nil {
		t.Fatalf("get 1: %v", err)
	}
	_, _ = io.ReadAll(resp.Body)
	resp.Body.Close()
	if got := atomic.LoadInt32(&hits); got != 1 {
		t.Fatalf("first fetch should miss: hits=%d", got)
	}

	// A follow-up within the TTL should hit the cache (no new upstream hit).
	resp, err = http.Get(baseURL + "/json/list")
	if err != nil {
		t.Fatalf("get 2: %v", err)
	}
	_, _ = io.ReadAll(resp.Body)
	resp.Body.Close()
	if got := atomic.LoadInt32(&hits); got != 1 {
		t.Fatalf("second fetch should be cached: hits=%d", got)
	}

	// Attach a new proxy (device swap) — cache must be invalidated.
	p2 := &Proxy{localPort: upPort}
	if err := s.RunWithProxy(p2); err != nil {
		t.Fatalf("re-wire: %v", err)
	}
	resp, err = http.Get(baseURL + "/json/list")
	if err != nil {
		t.Fatalf("get 3: %v", err)
	}
	_, _ = io.ReadAll(resp.Body)
	resp.Body.Close()
	if got := atomic.LoadInt32(&hits); got != 2 {
		t.Errorf("fetch after RunWithProxy should miss cache: hits=%d want 2", got)
	}

	// Direct invalidateListCache call (as WatchDevices would do) forces
	// another miss.
	s.invalidateListCache()
	resp, err = http.Get(baseURL + "/json/list")
	if err != nil {
		t.Fatalf("get 4: %v", err)
	}
	_, _ = io.ReadAll(resp.Body)
	resp.Body.Close()
	if got := atomic.LoadInt32(&hits); got != 3 {
		t.Errorf("fetch after invalidate should miss cache: hits=%d want 3", got)
	}
}

// TestRewriteDevtoolsJSON_IPv6 exercises the IPv6 literal path. The naive
// string splice in the old rewriteWSURL broke because `[::1]` contains
// colons and the function used `strings.Index(rest, "/")` on a host slice
// that still needed bracket-aware parsing.
func TestRewriteDevtoolsJSON_IPv6(t *testing.T) {
	body := []byte(`[{
		"id":"ABC",
		"webSocketDebuggerUrl":"ws://[::1]:9999/devtools/page/ABC",
		"devtoolsFrontendUrl":"/devtools/inspector.html?ws=[::1]:9999/devtools/page/ABC"
	}]`)
	out := rewriteDevtoolsJSON(body, "[::1]:9222")
	var entries []map[string]interface{}
	if err := json.Unmarshal(out, &entries); err != nil {
		t.Fatalf("unmarshal: %v body=%s", err, out)
	}
	if len(entries) != 1 {
		t.Fatalf("want 1 entry, got %d", len(entries))
	}
	if ws, _ := entries[0]["webSocketDebuggerUrl"].(string); ws != "ws://[::1]:9222/devtools/page/ABC" {
		t.Errorf("webSocketDebuggerUrl not rewritten: %q", ws)
	}
	if front, _ := entries[0]["devtoolsFrontendUrl"].(string); !strings.Contains(front, "ws=[::1]:9222/devtools/page/ABC") {
		t.Errorf("devtoolsFrontendUrl not rewritten: %q", front)
	}
}

func TestRewriteDevtoolsJSON_EmptyArray(t *testing.T) {
	out := rewriteDevtoolsJSON([]byte(`[]`), "127.0.0.1:9222")
	if string(out) != `[]` {
		t.Errorf("empty array mutated: %q", out)
	}
}

func TestRewriteDevtoolsJSON_DuplicateUrls(t *testing.T) {
	body := []byte(`[
		{"id":"A","webSocketDebuggerUrl":"ws://127.0.0.1:9999/devtools/page/A"},
		{"id":"B","webSocketDebuggerUrl":"ws://127.0.0.1:9999/devtools/page/A"}
	]`)
	out := rewriteDevtoolsJSON(body, "127.0.0.1:9222")
	var entries []map[string]interface{}
	if err := json.Unmarshal(out, &entries); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	for _, e := range entries {
		if ws, _ := e["webSocketDebuggerUrl"].(string); ws != "ws://127.0.0.1:9222/devtools/page/A" {
			t.Errorf("entry not rewritten: %q", ws)
		}
	}
}

func TestRewriteDevtoolsJSON_MalformedJSON(t *testing.T) {
	body := []byte(`[{"webSocketDebuggerUrl": not-valid-json}`)
	out := rewriteDevtoolsJSON(body, "127.0.0.1:9222")
	if string(out) != string(body) {
		t.Errorf("malformed body should pass through, got %q", out)
	}
}

// fmtSscanfPort extracts a port from "host:port".
func fmtSscanfPort(hostport string, out *int) (int, error) {
	i := strings.LastIndex(hostport, ":")
	if i < 0 {
		return 0, io.EOF
	}
	p := hostport[i+1:]
	n := 0
	for _, c := range p {
		if c < '0' || c > '9' {
			return 0, io.EOF
		}
		n = n*10 + int(c-'0')
	}
	*out = n
	return 1, nil
}
