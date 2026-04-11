package mobilebridge

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/gorilla/websocket"
)

// upstreamRecorder is a minimal WS server used by proxy tests. It upgrades
// any request, accepts one client, and records every text message it
// receives. It never writes anything back unless Reply is set.
type upstreamRecorder struct {
	mu     sync.Mutex
	got    [][]byte
	upg    websocket.Upgrader
	server *httptest.Server
}

func newUpstreamRecorder() *upstreamRecorder {
	u := &upstreamRecorder{
		upg: websocket.Upgrader{CheckOrigin: func(*http.Request) bool { return true }},
	}
	u.server = httptest.NewServer(http.HandlerFunc(u.handle))
	return u
}

func (u *upstreamRecorder) handle(w http.ResponseWriter, r *http.Request) {
	conn, err := u.upg.Upgrade(w, r, nil)
	if err != nil {
		return
	}
	defer conn.Close()
	for {
		_, data, err := conn.ReadMessage()
		if err != nil {
			return
		}
		u.mu.Lock()
		u.got = append(u.got, append([]byte(nil), data...))
		u.mu.Unlock()
	}
}

func (u *upstreamRecorder) WSURL() string {
	return "ws" + strings.TrimPrefix(u.server.URL, "http") + "/"
}

func (u *upstreamRecorder) Received() [][]byte {
	u.mu.Lock()
	defer u.mu.Unlock()
	out := make([][]byte, len(u.got))
	for i, b := range u.got {
		out[i] = append([]byte(nil), b...)
	}
	return out
}

func (u *upstreamRecorder) Close() { u.server.Close() }

// dialUpstream connects to the recorder as a websocket client.
func dialUpstream(t *testing.T, rawURL string) *websocket.Conn {
	t.Helper()
	u, err := url.Parse(rawURL)
	if err != nil {
		t.Fatalf("parse ws url: %v", err)
	}
	conn, _, err := websocket.DefaultDialer.Dial(u.String(), nil)
	if err != nil {
		t.Fatalf("dial upstream: %v", err)
	}
	return conn
}

// TestProxyInterceptsMobileBridgeTap verifies that when a downstream client
// sends a MobileBridge.tap CDP message, the Proxy intercepts it, never
// forwards it to the real upstream Chrome, and writes a synthetic CDP
// response back with matching id.
func TestProxyInterceptsMobileBridgeTap(t *testing.T) {
	rec := newUpstreamRecorder()
	defer rec.Close()

	upConn := dialUpstream(t, rec.WSURL())
	p := &Proxy{upstream: upConn, closed: make(chan struct{})}
	p.registerDefaultMethodHandlers()
	defer func() { _ = upConn.Close() }()

	// Stand up a downstream WS server whose handler hands the incoming
	// connection to p.Serve.
	upg := websocket.Upgrader{CheckOrigin: func(*http.Request) bool { return true }}
	serveErr := make(chan error, 1)
	ds := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		ws, err := upg.Upgrade(w, r, nil)
		if err != nil {
			serveErr <- err
			return
		}
		serveErr <- p.Serve(ws)
	}))
	defer ds.Close()

	client, _, err := websocket.DefaultDialer.Dial("ws"+strings.TrimPrefix(ds.URL, "http"), nil)
	if err != nil {
		t.Fatalf("dial downstream: %v", err)
	}
	defer client.Close()

	// Send MobileBridge.tap with id=42.
	msg := map[string]interface{}{
		"id":     42,
		"method": "MobileBridge.tap",
		"params": map[string]interface{}{"x": 100, "y": 200},
	}
	raw, _ := json.Marshal(msg)
	if err := client.WriteMessage(websocket.TextMessage, raw); err != nil {
		t.Fatalf("write: %v", err)
	}

	// Read the synthetic CDP response.
	_ = client.SetReadDeadline(time.Now().Add(2 * time.Second))
	_, respRaw, err := client.ReadMessage()
	if err != nil {
		t.Fatalf("read response: %v", err)
	}
	var resp cdpResponse
	if err := json.Unmarshal(respRaw, &resp); err != nil {
		t.Fatalf("unmarshal response: %v body=%s", err, respRaw)
	}
	if resp.ID != 42 {
		t.Errorf("response id = %d, want 42", resp.ID)
	}
	if resp.Error != nil {
		t.Errorf("unexpected error in response: %+v", resp.Error)
	}

	// Give the upstream goroutine a tick to drain; it should have received
	// two Input.dispatchTouchEvent frames (from Tap) but NOT the original
	// MobileBridge.tap message itself.
	time.Sleep(50 * time.Millisecond)
	got := rec.Received()
	for _, m := range got {
		if strings.Contains(string(m), "MobileBridge.tap") {
			t.Errorf("upstream saw the synthetic method: %s", m)
		}
	}
	// Should see touchStart + touchEnd dispatched by Tap().
	var touchEvents int
	for _, m := range got {
		if strings.Contains(string(m), "Input.dispatchTouchEvent") {
			touchEvents++
		}
	}
	if touchEvents != 2 {
		t.Errorf("want 2 Input.dispatchTouchEvent frames on upstream, got %d: %s", touchEvents, got)
	}

	// Close the client so Serve returns cleanly.
	_ = client.Close()
	select {
	case <-serveErr:
	case <-time.After(2 * time.Second):
		t.Error("Serve did not return after client close")
	}
}

// TestProxyUnknownMobileBridgeMethod verifies that an unregistered
// MobileBridge.* method returns a CDP error response rather than forwarding.
func TestProxyUnknownMobileBridgeMethod(t *testing.T) {
	rec := newUpstreamRecorder()
	defer rec.Close()

	upConn := dialUpstream(t, rec.WSURL())
	p := &Proxy{upstream: upConn, closed: make(chan struct{})}
	p.registerDefaultMethodHandlers()
	defer func() { _ = upConn.Close() }()

	upg := websocket.Upgrader{CheckOrigin: func(*http.Request) bool { return true }}
	serveErr := make(chan error, 1)
	ds := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		ws, err := upg.Upgrade(w, r, nil)
		if err != nil {
			serveErr <- err
			return
		}
		serveErr <- p.Serve(ws)
	}))
	defer ds.Close()

	client, _, err := websocket.DefaultDialer.Dial("ws"+strings.TrimPrefix(ds.URL, "http"), nil)
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	defer client.Close()

	raw, _ := json.Marshal(map[string]interface{}{
		"id":     7,
		"method": "MobileBridge.nope",
		"params": map[string]interface{}{},
	})
	if err := client.WriteMessage(websocket.TextMessage, raw); err != nil {
		t.Fatalf("write: %v", err)
	}
	_ = client.SetReadDeadline(time.Now().Add(2 * time.Second))
	_, respRaw, err := client.ReadMessage()
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	var resp cdpResponse
	if err := json.Unmarshal(respRaw, &resp); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if resp.Error == nil {
		t.Fatalf("want error response, got %+v", resp)
	}
	if resp.ID != 7 {
		t.Errorf("id = %d, want 7", resp.ID)
	}
}
