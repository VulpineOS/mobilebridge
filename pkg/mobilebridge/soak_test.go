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

func TestReconnectSerialized_Soak(t *testing.T) {
	lockTestGlobals(t)
	swapReconnectBackoff(t, []time.Duration{1 * time.Millisecond})
	swapCommandRunner(t, func(ctx context.Context, name string, args ...string) ([]byte, error) {
		return nil, nil
	})

	for round := 0; round < 12; round++ {
		up := newUpstreamRecorder()
		var hits int32
		jsonSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			atomic.AddInt32(&hits, 1)
			w.Header().Set("Content-Type", "application/json")
			time.Sleep(5 * time.Millisecond)
			fmt.Fprintf(w, `{"Browser":"Chrome/stub","webSocketDebuggerUrl":%q}`, up.WSURL())
		}))

		_, portStr, _ := net.SplitHostPort(strings.TrimPrefix(jsonSrv.URL, "http://"))
		var port int
		fmt.Sscanf(portStr, "%d", &port)

		p := &Proxy{
			serial:     fmt.Sprintf("SERIAL-%d", round),
			localPort:  port,
			remoteSock: "chrome_devtools_remote",
			closed:     make(chan struct{}),
			done:       make(chan struct{}),
		}

		const callers = 6
		var wg sync.WaitGroup
		errs := make([]error, callers)
		wg.Add(callers)
		for i := 0; i < callers; i++ {
			i := i
			go func() {
				defer wg.Done()
				errs[i] = p.reconnect()
			}()
		}
		wg.Wait()

		for i, err := range errs {
			if err != nil {
				t.Fatalf("round %d reconnect[%d]: %v", round, i, err)
			}
		}
		if got := atomic.LoadInt32(&hits); got != 1 {
			t.Fatalf("round %d json/version hits=%d want 1", round, got)
		}
		select {
		case <-p.Done():
			t.Fatalf("round %d proxy marked done after successful reconnect", round)
		default:
		}

		_ = p.Close()
		jsonSrv.Close()
		up.Close()
	}
}

func TestProxyServeRejectsSecondClient_Soak(t *testing.T) {
	for round := 0; round < 20; round++ {
		rec := newUpstreamRecorder()
		upConn := dialUpstream(t, rec.WSURL())

		p := &Proxy{upstream: upConn, closed: make(chan struct{})}
		p.registerDefaultMethodHandlers()

		upg := websocket.Upgrader{CheckOrigin: func(*http.Request) bool { return true }}
		serveErr1 := make(chan error, 1)
		ds1 := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			ws, err := upg.Upgrade(w, r, nil)
			if err != nil {
				serveErr1 <- err
				return
			}
			serveErr1 <- p.Serve(context.Background(), ws)
		}))

		client1, _, err := websocket.DefaultDialer.Dial("ws"+strings.TrimPrefix(ds1.URL, "http"), nil)
		if err != nil {
			t.Fatalf("round %d dial client1: %v", round, err)
		}

		deadline := time.Now().Add(500 * time.Millisecond)
		for !p.Busy() && time.Now().Before(deadline) {
			time.Sleep(2 * time.Millisecond)
		}
		if !p.Busy() {
			t.Fatalf("round %d proxy never became busy", round)
		}

		rec2 := newUpstreamRecorder()
		ws2 := dialUpstream(t, rec2.WSURL())
		err = p.Serve(context.Background(), ws2)
		if err != ErrBusy {
			t.Fatalf("round %d second Serve err=%v want %v", round, err, ErrBusy)
		}

		_ = client1.Close()
		select {
		case <-serveErr1:
		case <-time.After(2 * time.Second):
			t.Fatalf("round %d first Serve did not return", round)
		}

		_ = ws2.Close()
		_ = upConn.Close()
		ds1.Close()
		rec2.Close()
		rec.Close()
	}
}

func TestJsonListCacheInvalidation_Soak(t *testing.T) {
	var hitsA int32
	upstreamA := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt32(&hitsA, 1)
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `[{"id":"A","title":"device-a","webSocketDebuggerUrl":"ws://127.0.0.1:9999/devtools/page/A"}]`)
	}))
	defer upstreamA.Close()

	var hitsB int32
	upstreamB := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt32(&hitsB, 1)
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `[{"id":"B","title":"device-b","webSocketDebuggerUrl":"ws://127.0.0.1:9999/devtools/page/B"}]`)
	}))
	defer upstreamB.Close()

	upPortA := 0
	if _, err := fmtSscanfPort(strings.TrimPrefix(upstreamA.URL, "http://"), &upPortA); err != nil {
		t.Fatalf("parse upstream A port: %v", err)
	}
	upPortB := 0
	if _, err := fmtSscanfPort(strings.TrimPrefix(upstreamB.URL, "http://"), &upPortB); err != nil {
		t.Fatalf("parse upstream B port: %v", err)
	}

	s := NewServer("fake-serial", "127.0.0.1:9324")
	if err := s.Start(); err != nil {
		t.Fatalf("start: %v", err)
	}
	defer s.Stop()

	type wantState struct {
		port int
		id   string
	}
	sequence := []wantState{
		{port: upPortA, id: "A"},
		{port: upPortB, id: "B"},
	}

	for round := 0; round < 12; round++ {
		want := sequence[round%len(sequence)]
		if err := s.RunWithProxy(&Proxy{localPort: want.port}); err != nil {
			t.Fatalf("round %d wire: %v", round, err)
		}

		resp, err := http.Get("http://127.0.0.1:9324/json/list")
		if err != nil {
			t.Fatalf("round %d get list: %v", round, err)
		}
		body, _ := io.ReadAll(resp.Body)
		resp.Body.Close()
		if !strings.Contains(string(body), fmt.Sprintf(`"id":"%s"`, want.id)) {
			t.Fatalf("round %d stale /json/list body: %s", round, body)
		}

		resp, err = http.Get("http://127.0.0.1:9324/json/list")
		if err != nil {
			t.Fatalf("round %d second get list: %v", round, err)
		}
		_, _ = io.ReadAll(resp.Body)
		resp.Body.Close()
	}

	if got := atomic.LoadInt32(&hitsA); got < 1 {
		t.Fatalf("upstream A was never hit")
	}
	if got := atomic.LoadInt32(&hitsB); got < 1 {
		t.Fatalf("upstream B was never hit")
	}
}
