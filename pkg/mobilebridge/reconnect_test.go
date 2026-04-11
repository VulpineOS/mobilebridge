package mobilebridge

import (
	"context"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/gorilla/websocket"
)

// TestProxyReconnect_BackoffSequence drives (*Proxy).reconnect through a
// failure-then-success scenario and verifies:
//
//  1. Forward() is invoked on each attempt (via a stub commandRunner).
//  2. The /json/version endpoint fails N times before succeeding.
//  3. reconnect() returns nil once the upstream comes back.
//  4. Each attempt honors the backoff delay from reconnectBackoff.
//  5. reconnect() gives up after exhausting the backoff schedule.
func TestProxyReconnect_BackoffSequence(t *testing.T) {
	// Short backoffs so the test finishes quickly. Save + restore.
	origBackoff := reconnectBackoff
	reconnectBackoff = []time.Duration{
		5 * time.Millisecond,
		10 * time.Millisecond,
		20 * time.Millisecond,
		40 * time.Millisecond,
	}
	t.Cleanup(func() { reconnectBackoff = origBackoff })

	// Stub commandRunner so Forward() doesn't shell out to real adb.
	origRunner := commandRunner
	var forwardCalls int32
	commandRunner = func(ctx context.Context, name string, args ...string) ([]byte, error) {
		if len(args) >= 2 && args[2] == "forward" {
			atomic.AddInt32(&forwardCalls, 1)
		}
		return nil, nil
	}
	t.Cleanup(func() { commandRunner = origRunner })

	// Stand up a real upstream WS server for the "succeed after N failures"
	// case to dial into.
	wsRec := newUpstreamRecorder()
	defer wsRec.Close()
	wsURL := wsRec.WSURL()

	// Fake /json/version endpoint that returns 503 for the first 2 hits,
	// then returns a valid body pointing at wsURL.
	var hits int32
	jsonSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		n := atomic.AddInt32(&hits, 1)
		if n <= 2 {
			http.Error(w, "not ready", http.StatusServiceUnavailable)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprintf(w, `{"Browser":"Chrome/stub","webSocketDebuggerUrl":%q}`, wsURL)
	}))
	defer jsonSrv.Close()

	// Extract the port so reconnect() can use p.localPort against
	// http://127.0.0.1:<port>.
	_, portStr, err := net.SplitHostPort(strings.TrimPrefix(jsonSrv.URL, "http://"))
	if err != nil {
		t.Fatalf("split host port: %v", err)
	}
	var port int
	if _, err := fmt.Sscanf(portStr, "%d", &port); err != nil {
		t.Fatalf("parse port: %v", err)
	}
	// jsonSrv binds 127.0.0.1:<port> by default so the reconnect URL lines up.

	p := &Proxy{
		serial:     "STUB-SERIAL",
		localPort:  port,
		remoteSock: "chrome_devtools_remote",
	}

	start := time.Now()
	if err := p.reconnect(); err != nil {
		t.Fatalf("reconnect: %v", err)
	}
	elapsed := time.Since(start)

	// First 2 attempts sleep 5ms and 10ms (total ~15ms) before the 3rd
	// succeeds after sleeping 20ms. So elapsed should be >= 5+10+20 = 35ms.
	if elapsed < 30*time.Millisecond {
		t.Errorf("reconnect returned too fast: %v — backoff not honored", elapsed)
	}
	// And well under the full 5+10+20+40 = 75ms budget.
	if elapsed > 500*time.Millisecond {
		t.Errorf("reconnect took too long: %v", elapsed)
	}
	if got := atomic.LoadInt32(&hits); got != 3 {
		t.Errorf("want 3 /json/version hits, got %d", got)
	}
	if got := atomic.LoadInt32(&forwardCalls); got != 3 {
		t.Errorf("want 3 Forward() calls, got %d", got)
	}
	if p.upstream == nil {
		t.Error("expected p.upstream to be non-nil after successful reconnect")
	}
	if p.upstream != nil {
		_ = p.upstream.Close()
	}
}

// TestProxyReconnect_GivesUp verifies reconnect returns the last error after
// the backoff schedule is exhausted without a successful dial.
func TestProxyReconnect_GivesUp(t *testing.T) {
	origBackoff := reconnectBackoff
	reconnectBackoff = []time.Duration{
		1 * time.Millisecond,
		2 * time.Millisecond,
		3 * time.Millisecond,
	}
	t.Cleanup(func() { reconnectBackoff = origBackoff })

	origRunner := commandRunner
	commandRunner = func(ctx context.Context, name string, args ...string) ([]byte, error) {
		return nil, nil
	}
	t.Cleanup(func() { commandRunner = origRunner })

	// /json/version always 503s.
	var hits int32
	jsonSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt32(&hits, 1)
		w.WriteHeader(http.StatusServiceUnavailable)
		_, _ = io.WriteString(w, "not ready")
	}))
	defer jsonSrv.Close()

	_, portStr, _ := net.SplitHostPort(strings.TrimPrefix(jsonSrv.URL, "http://"))
	var port int
	fmt.Sscanf(portStr, "%d", &port)

	p := &Proxy{
		serial:     "STUB-SERIAL",
		localPort:  port,
		remoteSock: "chrome_devtools_remote",
	}
	if err := p.reconnect(); err == nil {
		t.Fatal("expected reconnect to give up, got nil")
	}
	if got := atomic.LoadInt32(&hits); int(got) != len(reconnectBackoff) {
		t.Errorf("hit count = %d, want %d (one per backoff tick)", got, len(reconnectBackoff))
	}
}

// TestProxyDoneClosedAfterReconnectGivesUp forces reconnect to fail every
// attempt and asserts the proxy's Done() channel is closed so CLI callers
// can notice the permanent loss without polling Upstream().
func TestProxyDoneClosedAfterReconnectGivesUp(t *testing.T) {
	origBackoff := reconnectBackoff
	reconnectBackoff = []time.Duration{
		1 * time.Millisecond,
		1 * time.Millisecond,
		1 * time.Millisecond,
		1 * time.Millisecond,
	}
	t.Cleanup(func() { reconnectBackoff = origBackoff })

	origRunner := commandRunner
	commandRunner = func(ctx context.Context, name string, args ...string) ([]byte, error) {
		return nil, nil
	}
	t.Cleanup(func() { commandRunner = origRunner })

	// /json/version always 503s so Dial never succeeds.
	jsonSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusServiceUnavailable)
	}))
	defer jsonSrv.Close()
	_, portStr, _ := net.SplitHostPort(strings.TrimPrefix(jsonSrv.URL, "http://"))
	var port int
	fmt.Sscanf(portStr, "%d", &port)

	p := &Proxy{
		serial:     "STUB",
		localPort:  port,
		remoteSock: "chrome_devtools_remote",
		closed:     make(chan struct{}),
	}
	// Prime Done() before reconnect so we exercise the lazy-init path.
	done := p.Done()

	if err := p.reconnect(); err == nil {
		t.Fatal("expected reconnect to give up, got nil")
	}
	select {
	case <-done:
		// expected
	case <-time.After(500 * time.Millisecond):
		t.Fatal("Done() channel not closed after reconnect gave up")
	}
}

// TestProxyReconnect_ReaderResumes verifies that when reconnect() swaps in a
// fresh upstream, the Serve reader goroutine picks up the new connection
// instead of staying blocked on the old (closed) one. The bug before M2 was
// that the reader held a snapshot of the original p.upstream and never saw
// the swap.
func TestProxyReconnect_ReaderResumes(t *testing.T) {
	origBackoff := reconnectBackoff
	reconnectBackoff = []time.Duration{1 * time.Millisecond}
	t.Cleanup(func() { reconnectBackoff = origBackoff })

	origRunner := commandRunner
	commandRunner = func(ctx context.Context, name string, args ...string) ([]byte, error) {
		return nil, nil
	}
	t.Cleanup(func() { commandRunner = origRunner })

	// Upstream #2 is what reconnect() dials after we kill #1.
	up2 := newUpstreamRecorder()
	defer up2.Close()

	// /json/version hands out up2's WS URL so reconnect lands on it.
	jsonSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprintf(w, `{"Browser":"Chrome/stub","webSocketDebuggerUrl":%q}`, up2.WSURL())
	}))
	defer jsonSrv.Close()
	_, portStr, _ := net.SplitHostPort(strings.TrimPrefix(jsonSrv.URL, "http://"))
	var port int
	fmt.Sscanf(portStr, "%d", &port)

	// Upstream #1: the initial connection the Proxy holds.
	up1 := newUpstreamRecorder()
	defer up1.Close()
	conn1, _, err := websocket.DefaultDialer.Dial(up1.WSURL(), nil)
	if err != nil {
		t.Fatalf("dial up1: %v", err)
	}

	p := &Proxy{
		serial:     "STUB",
		localPort:  port,
		remoteSock: "chrome_devtools_remote",
		upstream:   conn1,
		closed:     make(chan struct{}),
	}
	p.registerDefaultMethodHandlers()

	// Trigger a reconnect: this closes conn1 (old upstream) and dials a
	// fresh one against up2 via /json/version.
	if err := p.reconnect(); err != nil {
		t.Fatalf("reconnect: %v", err)
	}
	p.upstreamMu.RLock()
	newConn := p.upstream
	p.upstreamMu.RUnlock()
	if newConn == nil || newConn == conn1 {
		t.Fatalf("upstream was not swapped: new=%p old=%p", newConn, conn1)
	}
	// Sanity: the new conn is a real WS connection we can write to.
	if err := newConn.WriteMessage(websocket.TextMessage, []byte(`{"id":1,"method":"ping"}`)); err != nil {
		t.Fatalf("write on new upstream: %v", err)
	}
	_ = p.Close()
}
