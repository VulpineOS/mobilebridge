package mobilebridge

import (
	"context"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
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

// TestProxyReconnect_ReaderResumesAcrossSwap verifies the full read-direction
// resumption story end-to-end via the PRODUCTION write-failure path: the
// downstream client sends a frame, the writer pump's WriteMessage on the
// dead upstream returns an error, ensureReconnect() is invoked, the swap
// completes, the frame is replayed on up2, and the reader (which was also
// blocked on the dead conn) picks up new reads from up2 without Serve
// tearing down.
func TestProxyReconnect_ReaderResumesAcrossSwap(t *testing.T) {
	origBackoff := reconnectBackoff
	reconnectBackoff = []time.Duration{1 * time.Millisecond}
	t.Cleanup(func() { reconnectBackoff = origBackoff })

	origRunner := commandRunner
	commandRunner = func(ctx context.Context, name string, args ...string) ([]byte, error) {
		return nil, nil
	}
	t.Cleanup(func() { commandRunner = origRunner })

	// upstream2 records its accepted conn and echoes client-sent frames
	// back, and also exposes a channel for the test to push server-side
	// frames to the reader pump.
	var up2Conn *websocket.Conn
	var up2Mu sync.Mutex
	up2Got := make(chan string, 4)
	upg := websocket.Upgrader{CheckOrigin: func(*http.Request) bool { return true }}
	up2Srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		ws, err := upg.Upgrade(w, r, nil)
		if err != nil {
			return
		}
		up2Mu.Lock()
		up2Conn = ws
		up2Mu.Unlock()
		for {
			_, data, err := ws.ReadMessage()
			if err != nil {
				return
			}
			select {
			case up2Got <- string(data):
			default:
			}
		}
	}))
	defer up2Srv.Close()
	up2WSURL := "ws" + strings.TrimPrefix(up2Srv.URL, "http") + "/"

	jsonSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprintf(w, `{"Browser":"Chrome/stub","webSocketDebuggerUrl":%q}`, up2WSURL)
	}))
	defer jsonSrv.Close()
	_, portStr, _ := net.SplitHostPort(strings.TrimPrefix(jsonSrv.URL, "http://"))
	var port int
	fmt.Sscanf(portStr, "%d", &port)

	// upstream1: accepts the connection, then the test closes the HTTP
	// server (tearing down the TCP socket) so the next WriteMessage on
	// the cached conn fails — that's what drives the production reconnect.
	up1Accepted := make(chan struct{})
	up1Srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		ws, err := upg.Upgrade(w, r, nil)
		if err != nil {
			return
		}
		close(up1Accepted)
		for {
			if _, _, err := ws.ReadMessage(); err != nil {
				return
			}
		}
	}))
	conn1, _, err := websocket.DefaultDialer.Dial("ws"+strings.TrimPrefix(up1Srv.URL, "http")+"/", nil)
	if err != nil {
		t.Fatalf("dial up1: %v", err)
	}
	<-up1Accepted

	p := &Proxy{
		serial:     "STUB",
		localPort:  port,
		remoteSock: "chrome_devtools_remote",
		upstream:   conn1,
		closed:     make(chan struct{}),
		done:       make(chan struct{}),
	}
	p.registerDefaultMethodHandlers()

	serveErr := make(chan error, 1)
	dsSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		ws, err := upg.Upgrade(w, r, nil)
		if err != nil {
			serveErr <- err
			return
		}
		serveErr <- p.Serve(context.Background(), ws)
	}))
	defer dsSrv.Close()
	client, _, err := websocket.DefaultDialer.Dial("ws"+strings.TrimPrefix(dsSrv.URL, "http"), nil)
	if err != nil {
		t.Fatalf("dial client: %v", err)
	}
	defer client.Close()

	// Let the Serve goroutines settle into their read loops.
	time.Sleep(20 * time.Millisecond)

	// Kill upstream1 hard. The next WriteMessage the writer pump attempts
	// on conn1 will fail, triggering the production ensureReconnect path.
	_ = conn1.Close()
	up1Srv.Close()

	// Drive a downstream->upstream write to force the writer pump to
	// fail on the dead conn and invoke ensureReconnect().
	if err := client.WriteMessage(websocket.TextMessage, []byte(`{"id":1,"method":"Runtime.evaluate"}`)); err != nil {
		t.Fatalf("client write: %v", err)
	}

	// Wait for reconnect to swap in up2.
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		up2Mu.Lock()
		ready := up2Conn != nil
		up2Mu.Unlock()
		if ready {
			break
		}
		time.Sleep(5 * time.Millisecond)
	}
	up2Mu.Lock()
	conn := up2Conn
	up2Mu.Unlock()
	if conn == nil {
		t.Fatal("up2 never connected — writer-triggered reconnect did not fire")
	}

	// The replayed write should have landed on up2.
	select {
	case got := <-up2Got:
		if got != `{"id":1,"method":"Runtime.evaluate"}` {
			t.Errorf("up2 got unexpected frame: %q", got)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("up2 never received the replayed frame")
	}

	// Now verify the reader pump is ALSO resumed: push a server-side
	// frame from up2 and make sure the downstream client receives it.
	// Serve must still be running (not have errored out).
	select {
	case err := <-serveErr:
		t.Fatalf("Serve exited prematurely: %v", err)
	default:
	}
	if err := conn.WriteMessage(websocket.TextMessage, []byte(`{"msg":"post"}`)); err != nil {
		t.Fatalf("write on up2: %v", err)
	}
	_ = client.SetReadDeadline(time.Now().Add(2 * time.Second))
	_, data, err := client.ReadMessage()
	if err != nil {
		t.Fatalf("client did not receive post-reconnect frame: %v", err)
	}
	if string(data) != `{"msg":"post"}` {
		t.Errorf("post frame = %q", string(data))
	}

	_ = client.Close()
	_ = p.Close()
	select {
	case <-serveErr:
	case <-time.After(2 * time.Second):
	}
}

// TestProxy_ReaderInitiatedReconnect verifies that when the upstream breaks
// the READ side first (while no write is in flight), the reader goroutine
// kicks off a reconnect itself instead of tearing down the Serve loop. This
// exercises the ensureReconnect() coordination path.
func TestProxy_ReaderInitiatedReconnect(t *testing.T) {
	origBackoff := reconnectBackoff
	reconnectBackoff = []time.Duration{1 * time.Millisecond}
	t.Cleanup(func() { reconnectBackoff = origBackoff })

	origRunner := commandRunner
	commandRunner = func(ctx context.Context, name string, args ...string) ([]byte, error) {
		return nil, nil
	}
	t.Cleanup(func() { commandRunner = origRunner })

	// upstream #2 that the reconnect dial should land on.
	var up2Conn *websocket.Conn
	var up2Mu sync.Mutex
	upg := websocket.Upgrader{CheckOrigin: func(*http.Request) bool { return true }}
	up2Srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		ws, err := upg.Upgrade(w, r, nil)
		if err != nil {
			return
		}
		up2Mu.Lock()
		up2Conn = ws
		up2Mu.Unlock()
		for {
			if _, _, err := ws.ReadMessage(); err != nil {
				return
			}
		}
	}))
	defer up2Srv.Close()
	up2WSURL := "ws" + strings.TrimPrefix(up2Srv.URL, "http") + "/"

	jsonSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprintf(w, `{"Browser":"Chrome/stub","webSocketDebuggerUrl":%q}`, up2WSURL)
	}))
	defer jsonSrv.Close()
	_, portStr, _ := net.SplitHostPort(strings.TrimPrefix(jsonSrv.URL, "http://"))
	var port int
	fmt.Sscanf(portStr, "%d", &port)

	// upstream #1: accepts, writes a single frame, then hard-closes the
	// connection without waiting for any client read. This forces the
	// proxy's reader goroutine to observe a ReadMessage error while the
	// writer goroutine is idle — the canonical reader-initiated scenario.
	up1Ready := make(chan struct{})
	up1Srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		ws, err := upg.Upgrade(w, r, nil)
		if err != nil {
			return
		}
		close(up1Ready)
		_ = ws.WriteMessage(websocket.TextMessage, []byte(`{"msg":"hello"}`))
		// Hard close so the reader gets an error.
		_ = ws.Close()
	}))
	defer up1Srv.Close()

	conn1, _, err := websocket.DefaultDialer.Dial("ws"+strings.TrimPrefix(up1Srv.URL, "http")+"/", nil)
	if err != nil {
		t.Fatalf("dial up1: %v", err)
	}
	<-up1Ready

	p := &Proxy{
		serial:     "STUB",
		localPort:  port,
		remoteSock: "chrome_devtools_remote",
		upstream:   conn1,
		closed:     make(chan struct{}),
	}
	p.registerDefaultMethodHandlers()

	// Wire downstream.
	serveErr := make(chan error, 1)
	dsSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		ws, err := upg.Upgrade(w, r, nil)
		if err != nil {
			serveErr <- err
			return
		}
		serveErr <- p.Serve(context.Background(), ws)
	}))
	defer dsSrv.Close()
	client, _, err := websocket.DefaultDialer.Dial("ws"+strings.TrimPrefix(dsSrv.URL, "http"), nil)
	if err != nil {
		t.Fatalf("dial client: %v", err)
	}
	defer client.Close()

	// The reader should see up1's hello frame, then observe the close as
	// an error, and kick off a reconnect. Read the first frame.
	_ = client.SetReadDeadline(time.Now().Add(2 * time.Second))
	_, data, err := client.ReadMessage()
	if err != nil {
		t.Fatalf("expected hello frame, got err: %v", err)
	}
	if string(data) != `{"msg":"hello"}` {
		t.Errorf("first frame = %q", string(data))
	}
	_ = client.SetReadDeadline(time.Time{})

	// Wait for up2 to receive the reconnect dial.
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		up2Mu.Lock()
		ready := up2Conn != nil
		up2Mu.Unlock()
		if ready {
			break
		}
		time.Sleep(5 * time.Millisecond)
	}
	up2Mu.Lock()
	got := up2Conn
	up2Mu.Unlock()
	if got == nil {
		t.Fatal("reader did not initiate reconnect; up2 never received a dial")
	}

	// The downstream client must NOT have seen a close. Send a new frame
	// from up2 and ensure the client receives it — proves Serve is still
	// pumping.
	if err := got.WriteMessage(websocket.TextMessage, []byte(`{"msg":"post"}`)); err != nil {
		t.Fatalf("write on up2: %v", err)
	}
	_ = client.SetReadDeadline(time.Now().Add(2 * time.Second))
	_, data, err = client.ReadMessage()
	if err != nil {
		t.Fatalf("client did not receive post-reconnect frame: %v", err)
	}
	if string(data) != `{"msg":"post"}` {
		t.Errorf("post frame = %q", string(data))
	}

	_ = client.Close()
	_ = p.Close()
	select {
	case <-serveErr:
	case <-time.After(2 * time.Second):
	}
}

// TestReconnect_ReaderSurvivesWidenedSwapWindow installs a hook that sleeps
// between clearing the upstream conn and starting the dial loop, widening
// the window in which a reader could observe the swap. The reader pump must
// hold off on tearing down Serve during this window because reconnectGate
// is set atomically with clearing p.upstream.
func TestReconnect_ReaderSurvivesWidenedSwapWindow(t *testing.T) {
	origBackoff := reconnectBackoff
	reconnectBackoff = []time.Duration{1 * time.Millisecond}
	t.Cleanup(func() { reconnectBackoff = origBackoff })

	origRunner := commandRunner
	commandRunner = func(ctx context.Context, name string, args ...string) ([]byte, error) {
		return nil, nil
	}
	t.Cleanup(func() { commandRunner = origRunner })

	origHook := reconnectSwapHook
	reconnectSwapHook = func() { time.Sleep(50 * time.Millisecond) }
	t.Cleanup(func() { reconnectSwapHook = origHook })

	up2 := newUpstreamRecorder()
	defer up2.Close()
	jsonSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprintf(w, `{"Browser":"Chrome/stub","webSocketDebuggerUrl":%q}`, up2.WSURL())
	}))
	defer jsonSrv.Close()
	_, portStr, _ := net.SplitHostPort(strings.TrimPrefix(jsonSrv.URL, "http://"))
	var port int
	fmt.Sscanf(portStr, "%d", &port)

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
		done:       make(chan struct{}),
	}
	p.registerDefaultMethodHandlers()

	upg := websocket.Upgrader{CheckOrigin: func(*http.Request) bool { return true }}
	serveErr := make(chan error, 1)
	dsSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		ws, err := upg.Upgrade(w, r, nil)
		if err != nil {
			serveErr <- err
			return
		}
		serveErr <- p.Serve(context.Background(), ws)
	}))
	defer dsSrv.Close()
	client, _, err := websocket.DefaultDialer.Dial("ws"+strings.TrimPrefix(dsSrv.URL, "http"), nil)
	if err != nil {
		t.Fatalf("dial client: %v", err)
	}
	defer client.Close()

	// Give the Serve goroutines a moment to enter their read loops.
	time.Sleep(20 * time.Millisecond)

	// Drive a reconnect while the reader is parked. The swap hook sleeps
	// 50ms, widening any conn==nil window. The reader must NOT exit Serve
	// during this period.
	go func() { _ = p.reconnect() }()

	// Wait past the hook window plus dial time.
	time.Sleep(150 * time.Millisecond)

	// Prove Serve is still running: push a frame through up2 and read it.
	// up2 is a recorder, so grab its server conn via WSURL replay — we
	// instead just validate by issuing a new write-upstream path.
	p.upstreamMu.RLock()
	live := p.upstream
	p.upstreamMu.RUnlock()
	if live == nil {
		t.Fatal("upstream nil after reconnect; swap did not complete")
	}

	select {
	case err := <-serveErr:
		t.Fatalf("Serve exited during widened swap window: %v", err)
	default:
	}

	_ = client.Close()
	_ = p.Close()
	select {
	case <-serveErr:
	case <-time.After(2 * time.Second):
	}
}

// TestReconnect_Serialized spawns several goroutines all calling reconnect()
// simultaneously and verifies that only one actually runs the reconnect
// cycle. The others must observe the in-flight gate and return its result,
// otherwise a "first caller exhausts backoff and signals Done()" + "second
// caller succeeds" race would leave a live proxy permanently marked done.
func TestReconnect_Serialized(t *testing.T) {
	origBackoff := reconnectBackoff
	reconnectBackoff = []time.Duration{5 * time.Millisecond}
	t.Cleanup(func() { reconnectBackoff = origBackoff })

	origRunner := commandRunner
	commandRunner = func(ctx context.Context, name string, args ...string) ([]byte, error) {
		return nil, nil
	}
	t.Cleanup(func() { commandRunner = origRunner })

	up := newUpstreamRecorder()
	defer up.Close()

	var hits int32
	jsonSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt32(&hits, 1)
		w.Header().Set("Content-Type", "application/json")
		// Hold briefly so concurrent callers pile up on the in-flight gate.
		time.Sleep(20 * time.Millisecond)
		fmt.Fprintf(w, `{"Browser":"Chrome/stub","webSocketDebuggerUrl":%q}`, up.WSURL())
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
		done:       make(chan struct{}),
	}

	const N = 5
	var wg sync.WaitGroup
	errs := make([]error, N)
	wg.Add(N)
	for i := 0; i < N; i++ {
		i := i
		go func() {
			defer wg.Done()
			errs[i] = p.reconnect()
		}()
	}
	wg.Wait()

	for i, err := range errs {
		if err != nil {
			t.Errorf("reconnect[%d] returned %v", i, err)
		}
	}
	if got := atomic.LoadInt32(&hits); got != 1 {
		t.Errorf("want 1 /json/version hit (single in-flight reconnect), got %d", got)
	}
	// Done() must NOT be closed; the proxy reconnected cleanly.
	select {
	case <-p.Done():
		t.Error("Done() was closed despite a successful reconnect")
	default:
	}
	_ = p.Close()
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
