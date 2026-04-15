package mobilebridge

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"
)

type fakeWorkerAttachedSession struct {
	browserURL string
	done       chan struct{}
	closed     bool
}

func (f *fakeWorkerAttachedSession) BrowserURL() string { return f.browserURL }

func (f *fakeWorkerAttachedSession) Close() error {
	if !f.closed {
		f.closed = true
		close(f.done)
	}
	return nil
}

func (f *fakeWorkerAttachedSession) Done() <-chan struct{} { return f.done }

func TestWorkerControlServer_AttachTargetRelease(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/json/new" {
			http.NotFound(w, r)
			return
		}
		_ = json.NewEncoder(w).Encode(WorkerCreateTargetResponse{
			ID:    "page-1",
			Title: "Docs",
			URL:   r.URL.Query().Get("url"),
			Type:  "page",
		})
	}))
	defer upstream.Close()

	addr := "127.0.0.1:0"
	server := NewWorkerControlServer(addr)
	server.startAttached = func(context.Context, string, string) (workerAttachedSession, error) {
		return &fakeWorkerAttachedSession{
			browserURL: upstream.URL,
			done:       make(chan struct{}),
		}, nil
	}
	server.newSessionID = func() string { return "mbw_test" }
	if err := server.Start(); err != nil {
		t.Fatalf("start worker control: %v", err)
	}
	defer server.Stop()

	controlAddr := server.listenAddr
	if controlAddr == "" {
		t.Fatalf("control addr = %q", controlAddr)
	}

	body := bytes.NewBufferString(`{"device_id":"android-1"}`)
	resp, err := http.Post("http://"+controlAddr+"/sessions", "application/json", body)
	if err != nil {
		t.Fatalf("attach session: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("attach status = %d", resp.StatusCode)
	}
	var attached WorkerAttachResponse
	if err := json.NewDecoder(resp.Body).Decode(&attached); err != nil {
		t.Fatalf("decode attach: %v", err)
	}
	if attached.SessionID != "mbw_test" {
		t.Fatalf("attach = %#v", attached)
	}

	targetBody := bytes.NewBufferString(`{"url":"https://example.com/docs"}`)
	resp, err = http.Post("http://"+controlAddr+"/sessions/mbw_test/targets", "application/json", targetBody)
	if err != nil {
		t.Fatalf("create target: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("target status = %d", resp.StatusCode)
	}
	var target WorkerCreateTargetResponse
	if err := json.NewDecoder(resp.Body).Decode(&target); err != nil {
		t.Fatalf("decode target: %v", err)
	}
	if target.ID != "page-1" || target.URL != "https://example.com/docs" {
		t.Fatalf("target = %#v", target)
	}

	req, err := http.NewRequest(http.MethodDelete, "http://"+controlAddr+"/sessions/mbw_test", nil)
	if err != nil {
		t.Fatalf("delete request: %v", err)
	}
	resp, err = http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("release session: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("release status = %d", resp.StatusCode)
	}
}

func TestWorkerControlServer_RemovesClosedSessions(t *testing.T) {
	session := &fakeWorkerAttachedSession{
		browserURL: "http://127.0.0.1:9222",
		done:       make(chan struct{}),
	}
	server := NewWorkerControlServer("127.0.0.1:0")
	server.startAttached = func(context.Context, string, string) (workerAttachedSession, error) {
		return session, nil
	}
	server.newSessionID = func() string { return "mbw_test" }
	if err := server.Start(); err != nil {
		t.Fatalf("start worker control: %v", err)
	}
	defer server.Stop()

	resp, err := http.Post("http://"+server.listenAddr+"/sessions", "application/json", bytes.NewBufferString(`{"device_id":"android-1"}`))
	if err != nil {
		t.Fatalf("attach session: %v", err)
	}
	resp.Body.Close()

	if err := session.Close(); err != nil {
		t.Fatalf("close session: %v", err)
	}
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if server.getSession("mbw_test") == nil {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatal("session was not cleaned up after Done closed")
}

func TestWorkerControlServer_EnforcesMaxSessions(t *testing.T) {
	server := NewWorkerControlServer("127.0.0.1:0")
	server.SetMaxSessions(1)
	server.startAttached = func(context.Context, string, string) (workerAttachedSession, error) {
		return &fakeWorkerAttachedSession{
			browserURL: "http://127.0.0.1:9222",
			done:       make(chan struct{}),
		}, nil
	}
	server.newSessionID = func() string { return "mbw_test" }
	if err := server.Start(); err != nil {
		t.Fatalf("start worker control: %v", err)
	}
	defer server.Stop()

	resp, err := http.Post("http://"+server.ListenAddr()+"/sessions", "application/json", bytes.NewBufferString(`{"device_id":"android-1"}`))
	if err != nil {
		t.Fatalf("attach session: %v", err)
	}
	resp.Body.Close()

	resp, err = http.Post("http://"+server.ListenAddr()+"/sessions", "application/json", bytes.NewBufferString(`{"device_id":"android-2"}`))
	if err != nil {
		t.Fatalf("attach second session: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusServiceUnavailable {
		t.Fatalf("status = %d", resp.StatusCode)
	}
}

func TestWorkerControlServer_Snapshot(t *testing.T) {
	server := NewWorkerControlServer("127.0.0.1:0")
	server.SetMaxSessions(3)
	server.startAttached = func(context.Context, string, string) (workerAttachedSession, error) {
		return &fakeWorkerAttachedSession{
			browserURL: "http://127.0.0.1:9222",
			done:       make(chan struct{}),
		}, nil
	}
	server.newSessionID = func() string { return "mbw_test" }
	server.listDevices = func(context.Context) ([]Device, error) {
		return []Device{{Serial: "android-1", State: "device", Model: "Pixel 9", AndroidVersion: "14"}}, nil
	}
	server.enrichDevice = func(context.Context, *Device) error { return nil }
	server.socketInfo = func(context.Context, string) (DevtoolsSocket, error) {
		return DevtoolsSocket{Name: "chrome_devtools_remote", Kind: SocketKindChrome}, nil
	}
	if err := server.Start(); err != nil {
		t.Fatalf("start worker control: %v", err)
	}
	defer server.Stop()

	resp, err := http.Post("http://"+server.ListenAddr()+"/sessions", "application/json", bytes.NewBufferString(`{"device_id":"android-1"}`))
	if err != nil {
		t.Fatalf("attach session: %v", err)
	}
	resp.Body.Close()

	snapshot := server.Snapshot(context.Background(), "worker-1", "farm-a", "http://worker-a.internal")
	if !snapshot.Healthy || snapshot.ActiveSessions != 1 || snapshot.MaxSessions != 3 {
		t.Fatalf("snapshot = %#v", snapshot)
	}
	if len(snapshot.Devices) != 1 || !snapshot.Devices[0].Inspectable {
		t.Fatalf("snapshot devices = %#v", snapshot.Devices)
	}
}
