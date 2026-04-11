package mobilebridge

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

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
	s := NewServer("fake-serial", "127.0.0.1:9222")
	if err := s.Start(); err != nil {
		t.Fatalf("start server: %v", err)
	}
	defer s.Stop()
	if err := s.RunWithProxy(p); err != nil {
		t.Fatalf("run with proxy: %v", err)
	}

	resp, err := http.Get("http://127.0.0.1:9222/json/list")
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
	if ws != "ws://127.0.0.1:9222/devtools/page/ABC123" {
		t.Errorf("webSocketDebuggerUrl not rewritten: %q", ws)
	}
	front, _ := entries[0]["devtoolsFrontendUrl"].(string)
	if !strings.Contains(front, "ws=127.0.0.1:9222/devtools/page/ABC123") {
		t.Errorf("devtoolsFrontendUrl not rewritten: %q", front)
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
	s := NewServer("fake-serial", "127.0.0.1:9223")
	if err := s.Start(); err != nil {
		t.Fatalf("start: %v", err)
	}
	defer s.Stop()
	if err := s.RunWithProxy(p); err != nil {
		t.Fatalf("wire: %v", err)
	}

	// Hammer /json/list 20 times within the TTL window.
	for i := 0; i < 20; i++ {
		resp, err := http.Get("http://127.0.0.1:9223/json/list")
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
	resp, err := http.Get("http://127.0.0.1:9223/json/list")
	if err != nil {
		t.Fatalf("get after ttl: %v", err)
	}
	_, _ = io.ReadAll(resp.Body)
	resp.Body.Close()
	if got := atomic.LoadInt32(&hits); got != 2 {
		t.Errorf("upstream hits after ttl = %d, want 2", got)
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
