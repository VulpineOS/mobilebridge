package mobilebridge

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestWorkerHeartbeatPublisher_Send(t *testing.T) {
	var received WorkerHeartbeat
	api := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			t.Fatalf("method = %s", r.Method)
		}
		if got := r.Header.Get("Authorization"); got != "Bearer token-123" {
			t.Fatalf("authorization = %q", got)
		}
		if err := json.NewDecoder(r.Body).Decode(&received); err != nil {
			t.Fatalf("decode heartbeat: %v", err)
		}
		w.WriteHeader(http.StatusOK)
	}))
	defer api.Close()

	server := NewWorkerControlServer("127.0.0.1:0")
	server.listDevices = func(context.Context) ([]Device, error) {
		return []Device{{Serial: "android-1", State: "device", Model: "Pixel 9", AndroidVersion: "14"}}, nil
	}
	server.enrichDevice = func(context.Context, *Device) error { return nil }
	server.socketInfo = func(context.Context, string) (DevtoolsSocket, error) {
		return DevtoolsSocket{Name: "chrome_devtools_remote", Kind: SocketKindChrome}, nil
	}
	publisher := NewWorkerHeartbeatPublisher(server, api.URL, "token-123", "worker-1", "farm-a", "http://worker-a.internal", 0)
	if err := publisher.Send(context.Background()); err != nil {
		t.Fatalf("Send: %v", err)
	}
	if received.WorkerID != "worker-1" || received.AdvertiseAddr != "http://worker-a.internal" {
		t.Fatalf("received = %#v", received)
	}
	if len(received.Devices) != 1 || received.Devices[0].DeviceID != "android-1" {
		t.Fatalf("received devices = %#v", received.Devices)
	}
}
