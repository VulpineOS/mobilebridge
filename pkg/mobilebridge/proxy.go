package mobilebridge

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"time"

	"github.com/gorilla/websocket"
)

// Proxy owns one upstream CDP WebSocket connection to Chrome on the device
// plus, while Serve is running, exactly one downstream client WebSocket. It
// pumps frames bidirectionally and tears everything down on Close.
//
// Multi-client fan-out is intentionally NOT supported in this MVP. A CDP
// session is inherently stateful (target ids, enabled domains, outstanding
// request ids) so fan-out would require message multiplexing with per-client
// id remapping. Until that lands, Serve will refuse a second concurrent
// client by returning ErrBusy; callers wiring Serve into an HTTP handler
// should surface that as a 503 so clients get a clear failure instead of
// silent frame interleaving.
type Proxy struct {
	serial     string
	localPort  int
	remoteSock string

	upstream *websocket.Conn

	// writeMu serializes writes on the upstream connection. Gesture helpers
	// and the downstream->upstream pump both use it.
	writeMu sync.Mutex

	// serveMu guards single-client enforcement. busy is set while Serve is
	// actively pumping frames.
	serveMu sync.Mutex
	busy    bool

	// methodHandlers maps synthetic CDP method names (e.g. "MobileBridge.tap")
	// to handler functions. Handlers receive the raw "params" JSON and return
	// an arbitrary result value which the Proxy wraps into a CDP response.
	// The map is populated by registerDefaultMethodHandlers in NewProxy and
	// may also be extended via RegisterMethod for tests or callers embedding
	// the Proxy directly.
	methodMu       sync.RWMutex
	methodHandlers map[string]func(params json.RawMessage) (interface{}, error)

	closeOnce sync.Once
	closed    chan struct{}
}

// ErrBusy is returned by Proxy.Serve if a second client tries to attach
// while another is already connected. See the Proxy doc comment for the
// single-client limitation.
var ErrBusy = errors.New("mobilebridge: proxy is already serving a client")

// NewProxy sets up adb forwarding to the given device's Chrome devtools
// socket, queries Chrome's /json/version endpoint to find the browser-level
// WebSocket URL, and dials it. It returns a ready-to-Serve Proxy.
func NewProxy(serial string, localPort int) (*Proxy, error) {
	sock, err := ChromeDevtoolsSocket(serial)
	if err != nil {
		return nil, fmt.Errorf("find devtools socket: %w", err)
	}
	if err := Forward(serial, localPort, sock); err != nil {
		return nil, fmt.Errorf("adb forward: %w", err)
	}

	wsURL, err := fetchBrowserWebSocketURL(fmt.Sprintf("http://127.0.0.1:%d", localPort))
	if err != nil {
		_ = Unforward(serial, localPort)
		return nil, fmt.Errorf("fetch browser ws url: %w", err)
	}

	dialer := websocket.Dialer{HandshakeTimeout: 10 * time.Second}
	conn, _, err := dialer.Dial(wsURL, nil)
	if err != nil {
		_ = Unforward(serial, localPort)
		return nil, fmt.Errorf("dial upstream: %w", err)
	}

	p := &Proxy{
		serial:     serial,
		localPort:  localPort,
		remoteSock: sock,
		upstream:   conn,
		closed:     make(chan struct{}),
	}
	p.registerDefaultMethodHandlers()
	return p, nil
}

// RegisterMethod installs a synthetic CDP method handler. If a handler with
// the same name already exists it is replaced. Handlers are called from the
// downstream read goroutine, so they should return quickly; long-running work
// should be dispatched to its own goroutine.
func (p *Proxy) RegisterMethod(name string, fn func(params json.RawMessage) (interface{}, error)) {
	p.methodMu.Lock()
	defer p.methodMu.Unlock()
	if p.methodHandlers == nil {
		p.methodHandlers = make(map[string]func(params json.RawMessage) (interface{}, error))
	}
	p.methodHandlers[name] = fn
}

// lookupMethod returns the handler for name if one is registered.
func (p *Proxy) lookupMethod(name string) (func(params json.RawMessage) (interface{}, error), bool) {
	p.methodMu.RLock()
	defer p.methodMu.RUnlock()
	fn, ok := p.methodHandlers[name]
	return fn, ok
}

// registerDefaultMethodHandlers wires the built-in MobileBridge.* gesture
// methods into methodHandlers. Called from NewProxy and also from tests that
// construct a bare Proxy{}.
func (p *Proxy) registerDefaultMethodHandlers() {
	p.RegisterMethod("MobileBridge.tap", func(params json.RawMessage) (interface{}, error) {
		var args struct{ X, Y int }
		if len(params) > 0 {
			if err := json.Unmarshal(params, &args); err != nil {
				return nil, err
			}
		}
		if err := Tap(p, args.X, args.Y); err != nil {
			return nil, err
		}
		return map[string]interface{}{}, nil
	})
	p.RegisterMethod("MobileBridge.longPress", func(params json.RawMessage) (interface{}, error) {
		var args struct {
			X, Y, DurationMs int
		}
		if len(params) > 0 {
			if err := json.Unmarshal(params, &args); err != nil {
				return nil, err
			}
		}
		if err := LongPress(p, args.X, args.Y, args.DurationMs); err != nil {
			return nil, err
		}
		return map[string]interface{}{}, nil
	})
	p.RegisterMethod("MobileBridge.swipe", func(params json.RawMessage) (interface{}, error) {
		var args struct {
			FromX, FromY, ToX, ToY, DurationMs int
		}
		if len(params) > 0 {
			if err := json.Unmarshal(params, &args); err != nil {
				return nil, err
			}
		}
		if err := Swipe(p, args.FromX, args.FromY, args.ToX, args.ToY, args.DurationMs); err != nil {
			return nil, err
		}
		return map[string]interface{}{}, nil
	})
	p.RegisterMethod("MobileBridge.pinch", func(params json.RawMessage) (interface{}, error) {
		var args struct {
			CenterX, CenterY int
			Scale            float64
		}
		if len(params) > 0 {
			if err := json.Unmarshal(params, &args); err != nil {
				return nil, err
			}
		}
		if err := Pinch(p, args.CenterX, args.CenterY, args.Scale); err != nil {
			return nil, err
		}
		return map[string]interface{}{}, nil
	})
}

// browserVersionInfo is the subset of /json/version we care about.
type browserVersionInfo struct {
	Browser              string `json:"Browser"`
	ProtocolVersion      string `json:"Protocol-Version"`
	UserAgent            string `json:"User-Agent"`
	V8Version            string `json:"V8-Version"`
	WebKitVersion        string `json:"WebKit-Version"`
	WebSocketDebuggerURL string `json:"webSocketDebuggerUrl"`
}

func fetchBrowserWebSocketURL(base string) (string, error) {
	u, err := url.Parse(base)
	if err != nil {
		return "", err
	}
	u.Path = "/json/version"
	client := &http.Client{Timeout: 5 * time.Second}
	resp, err := client.Get(u.String())
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return "", err
	}
	if resp.StatusCode != 200 {
		return "", fmt.Errorf("json/version status %d: %s", resp.StatusCode, string(body))
	}
	var v browserVersionInfo
	if err := json.Unmarshal(body, &v); err != nil {
		return "", fmt.Errorf("decode json/version: %w", err)
	}
	if v.WebSocketDebuggerURL == "" {
		return "", errors.New("json/version: no webSocketDebuggerUrl")
	}
	return v.WebSocketDebuggerURL, nil
}

// sendUpstream serializes params to JSON and writes one CDP message. This
// satisfies the messageSender interface so gesture helpers can drive it.
func (p *Proxy) sendUpstream(method string, params any) error {
	var raw json.RawMessage
	if params != nil {
		b, err := json.Marshal(params)
		if err != nil {
			return err
		}
		raw = b
	}
	msg := cdpMessage{ID: nextID(), Method: method, Params: raw}
	b, err := json.Marshal(msg)
	if err != nil {
		return err
	}
	p.writeMu.Lock()
	defer p.writeMu.Unlock()
	if p.upstream == nil {
		return errors.New("mobilebridge: proxy not connected")
	}
	return p.upstream.WriteMessage(websocket.TextMessage, b)
}

// Serve pumps frames in both directions until either side hangs up. Only
// one client may be attached at a time; a second concurrent call returns
// ErrBusy without touching the connection.
func (p *Proxy) Serve(downstream *websocket.Conn) error {
	if downstream == nil {
		return errors.New("mobilebridge: nil downstream")
	}
	p.serveMu.Lock()
	if p.busy {
		p.serveMu.Unlock()
		return ErrBusy
	}
	p.busy = true
	p.serveMu.Unlock()
	defer func() {
		p.serveMu.Lock()
		p.busy = false
		p.serveMu.Unlock()
	}()
	errCh := make(chan error, 2)

	// downstreamWriteMu serializes writes on the downstream connection.
	// Synthetic responses and the upstream->downstream pump both use it.
	var downstreamWriteMu sync.Mutex
	writeDownstream := func(mt int, b []byte) error {
		downstreamWriteMu.Lock()
		defer downstreamWriteMu.Unlock()
		return downstream.WriteMessage(mt, b)
	}

	// Downstream -> upstream. Intercept synthetic MobileBridge.* methods
	// and translate them into real CDP calls; forward everything else.
	go func() {
		for {
			mt, data, err := downstream.ReadMessage()
			if err != nil {
				errCh <- err
				return
			}
			if mt != websocket.TextMessage {
				continue
			}
			if handled, resp := p.maybeHandleSynthetic(data); handled {
				if resp != nil {
					if werr := writeDownstream(websocket.TextMessage, resp); werr != nil {
						errCh <- werr
						return
					}
				}
				continue
			}
			p.writeMu.Lock()
			werr := p.upstream.WriteMessage(websocket.TextMessage, data)
			p.writeMu.Unlock()
			if werr != nil {
				// Upstream died mid-write — most commonly because the
				// adb forward dropped. Try to re-establish; if that
				// succeeds, replay this frame on the new connection.
				if rerr := p.reconnect(); rerr != nil {
					errCh <- werr
					return
				}
				p.writeMu.Lock()
				werr = p.upstream.WriteMessage(websocket.TextMessage, data)
				p.writeMu.Unlock()
				if werr != nil {
					errCh <- werr
					return
				}
			}
		}
	}()

	// Upstream -> downstream.
	go func() {
		for {
			mt, data, err := p.upstream.ReadMessage()
			if err != nil {
				errCh <- err
				return
			}
			if err := writeDownstream(mt, data); err != nil {
				errCh <- err
				return
			}
		}
	}()

	select {
	case err := <-errCh:
		return err
	case <-p.closed:
		return nil
	}
}

// cdpResponse is the JSON wire format for a CDP method reply. Either Result
// or Error is set, matching the shape real Chrome uses.
type cdpResponse struct {
	ID     int64           `json:"id"`
	Result interface{}     `json:"result,omitempty"`
	Error  *cdpErrorObject `json:"error,omitempty"`
}

type cdpErrorObject struct {
	Code    int    `json:"code"`
	Message string `json:"message"`
}

// maybeHandleSynthetic peeks at an incoming CDP message. If the method name
// is registered in methodHandlers the handler is invoked and a CDP response
// is encoded for the caller to write back to the downstream client. Returns
// handled=true whenever the message was consumed; the upstream is never told
// about synthetic methods.
func (p *Proxy) maybeHandleSynthetic(raw []byte) (bool, []byte) {
	var probe struct {
		ID     int64           `json:"id"`
		Method string          `json:"method"`
		Params json.RawMessage `json:"params"`
	}
	if err := json.Unmarshal(raw, &probe); err != nil {
		return false, nil
	}
	if probe.Method == "" {
		return false, nil
	}
	handler, ok := p.lookupMethod(probe.Method)
	if !ok {
		// Not a synthetic method — forward to upstream as usual. We still
		// treat MobileBridge.* without a handler as handled-with-error so
		// the caller gets a proper CDP error instead of a real Chrome
		// "method not found" for a method Chrome doesn't know either.
		if strings.HasPrefix(probe.Method, "MobileBridge.") {
			resp := cdpResponse{
				ID: probe.ID,
				Error: &cdpErrorObject{
					Code:    -32601,
					Message: fmt.Sprintf("mobilebridge: unknown synthetic method %q", probe.Method),
				},
			}
			b, _ := json.Marshal(resp)
			return true, b
		}
		return false, nil
	}

	result, err := handler(probe.Params)
	if err != nil {
		resp := cdpResponse{
			ID: probe.ID,
			Error: &cdpErrorObject{
				Code:    -32000,
				Message: err.Error(),
			},
		}
		b, _ := json.Marshal(resp)
		return true, b
	}
	if result == nil {
		result = map[string]interface{}{}
	}
	resp := cdpResponse{ID: probe.ID, Result: result}
	b, _ := json.Marshal(resp)
	return true, b
}

// Close tears down the upstream WebSocket and removes the adb forward.
func (p *Proxy) Close() error {
	var err error
	p.closeOnce.Do(func() {
		close(p.closed)
		if p.upstream != nil {
			err = p.upstream.Close()
		}
		if p.serial != "" && p.localPort != 0 {
			if uerr := Unforward(p.serial, p.localPort); uerr != nil && err == nil {
				err = uerr
			}
		}
	})
	return err
}

// reconnectBackoff is the escalating delay sequence for reconnect attempts.
// Overridable from tests.
var reconnectBackoff = []time.Duration{
	100 * time.Millisecond,
	300 * time.Millisecond,
	1 * time.Second,
	3 * time.Second,
}

// reconnect tears down the current upstream connection and tries to
// re-establish the adb forward + Chrome WebSocket with an escalating
// backoff. Used by Serve() when a write to upstream fails — the most
// common cause is the adb forward dropping (cable jiggle, adb server
// restart, device sleep). Returns the final error if all attempts fail.
func (p *Proxy) reconnect() error {
	if p.upstream != nil {
		_ = p.upstream.Close()
		p.upstream = nil
	}
	var lastErr error
	for _, delay := range reconnectBackoff {
		time.Sleep(delay)
		// Best-effort: re-run adb forward in case the forward was torn
		// down underneath us.
		if err := Forward(p.serial, p.localPort, p.remoteSock); err != nil {
			lastErr = fmt.Errorf("adb forward: %w", err)
			continue
		}
		wsURL, err := fetchBrowserWebSocketURL(fmt.Sprintf("http://127.0.0.1:%d", p.localPort))
		if err != nil {
			lastErr = fmt.Errorf("fetch browser ws url: %w", err)
			continue
		}
		dialer := websocket.Dialer{HandshakeTimeout: 10 * time.Second}
		conn, _, err := dialer.Dial(wsURL, nil)
		if err != nil {
			lastErr = fmt.Errorf("dial upstream: %w", err)
			continue
		}
		p.writeMu.Lock()
		p.upstream = conn
		p.writeMu.Unlock()
		return nil
	}
	if lastErr == nil {
		lastErr = errors.New("mobilebridge: reconnect gave up")
	}
	return lastErr
}

// Upstream exposes the underlying connection for advanced callers. It is not
// safe for concurrent writes; use sendUpstream instead.
func (p *Proxy) Upstream() *websocket.Conn { return p.upstream }

// Busy reports whether a client is currently attached via Serve. Used by
// HTTP handlers to reject a second connection with 503 before upgrading.
func (p *Proxy) Busy() bool {
	p.serveMu.Lock()
	defer p.serveMu.Unlock()
	return p.busy
}
