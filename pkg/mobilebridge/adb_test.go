package mobilebridge

import (
	"context"
	"errors"
	"strings"
	"testing"
)

const sampleDevicesOutput = `List of devices attached
R58N12ABCDE            device usb:336592896X product:starqltesq model:SM_G960U device:starqltesq transport_id:1
emulator-5554          offline
0123456789ABCDEF       unauthorized usb:1-2 transport_id:4
* daemon not running; starting now at tcp:5037 *
`

func TestParseDevices(t *testing.T) {
	devs := parseDevices(sampleDevicesOutput)
	if len(devs) != 3 {
		t.Fatalf("want 3 devices, got %d: %#v", len(devs), devs)
	}

	if devs[0].Serial != "R58N12ABCDE" {
		t.Errorf("serial[0] = %q", devs[0].Serial)
	}
	if devs[0].State != "device" {
		t.Errorf("state[0] = %q", devs[0].State)
	}
	if devs[0].Model != "SM_G960U" {
		t.Errorf("model[0] = %q", devs[0].Model)
	}
	if devs[0].Product != "starqltesq" {
		t.Errorf("product[0] = %q", devs[0].Product)
	}

	if devs[1].Serial != "emulator-5554" || devs[1].State != "offline" {
		t.Errorf("emulator row wrong: %#v", devs[1])
	}
	if devs[2].State != "unauthorized" {
		t.Errorf("unauthorized row wrong: %#v", devs[2])
	}
}

func TestParseDevicesEmpty(t *testing.T) {
	if got := parseDevices("List of devices attached\n\n"); len(got) != 0 {
		t.Errorf("want empty, got %#v", got)
	}
}

const sampleProcNetUnixChrome = `Num       RefCount Protocol Flags    Type St Inode Path
0000000000000000: 00000002 00000000 00010000 0001 01 12345 @chrome_devtools_remote
0000000000000000: 00000002 00000000 00010000 0001 01 12346 @webview_devtools_remote_1234
`

const sampleProcNetUnixWebviewOnly = `Num       RefCount Protocol Flags    Type St Inode Path
0000000000000000: 00000002 00000000 00010000 0001 01 12346 @webview_devtools_remote_9876
`

const sampleProcNetUnixNone = `Num       RefCount Protocol Flags    Type St Inode Path
0000000000000000: 00000002 00000000 00010000 0001 01 1 @some_other_socket
`

func TestParseDevtoolsSocketChromePreferred(t *testing.T) {
	got, ok := parseDevtoolsSocket(sampleProcNetUnixChrome)
	if !ok {
		t.Fatal("expected a socket")
	}
	if got != "chrome_devtools_remote" {
		t.Errorf("got %q, want chrome_devtools_remote", got)
	}
}

func TestParseDevtoolsSocketWebviewFallback(t *testing.T) {
	got, ok := parseDevtoolsSocket(sampleProcNetUnixWebviewOnly)
	if !ok {
		t.Fatal("expected webview socket")
	}
	if got != "webview_devtools_remote_9876" {
		t.Errorf("got %q", got)
	}
}

func TestParseDevtoolsSocketNone(t *testing.T) {
	if _, ok := parseDevtoolsSocket(sampleProcNetUnixNone); ok {
		t.Error("expected no match")
	}
}

// stubRunner swaps commandRunner for the duration of a test.
type stubCall struct {
	name string
	args []string
}

func withStubRunner(t *testing.T, out string, err error) *[]stubCall {
	t.Helper()
	var calls []stubCall
	orig := commandRunner
	commandRunner = func(ctx context.Context, name string, args ...string) ([]byte, error) {
		calls = append(calls, stubCall{name: name, args: append([]string(nil), args...)})
		return []byte(out), err
	}
	t.Cleanup(func() { commandRunner = orig })
	return &calls
}

func TestListDevicesUsesRunner(t *testing.T) {
	calls := withStubRunner(t, sampleDevicesOutput, nil)
	devs, err := ListDevices(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if len(devs) != 3 {
		t.Errorf("want 3 devices, got %d", len(devs))
	}
	if len(*calls) != 1 || (*calls)[0].name != "adb" {
		t.Errorf("runner calls = %#v", *calls)
	}
	if strings.Join((*calls)[0].args, " ") != "devices -l" {
		t.Errorf("args = %v", (*calls)[0].args)
	}
}

func TestForwardArgs(t *testing.T) {
	calls := withStubRunner(t, "", nil)
	if err := Forward(context.Background(), "R58N", 9222, "chrome_devtools_remote"); err != nil {
		t.Fatal(err)
	}
	if len(*calls) != 1 {
		t.Fatalf("want 1 call, got %d", len(*calls))
	}
	want := "-s R58N forward tcp:9222 localabstract:chrome_devtools_remote"
	if got := strings.Join((*calls)[0].args, " "); got != want {
		t.Errorf("args = %q, want %q", got, want)
	}
}

func TestForwardEmptySerial(t *testing.T) {
	if err := Forward(context.Background(), "", 9222, "x"); err == nil {
		t.Error("expected error for empty serial")
	}
}

func TestUnforwardArgs(t *testing.T) {
	calls := withStubRunner(t, "", nil)
	if err := Unforward(context.Background(), "R58N", 9222); err != nil {
		t.Fatal(err)
	}
	want := "-s R58N forward --remove tcp:9222"
	if got := strings.Join((*calls)[0].args, " "); got != want {
		t.Errorf("args = %q, want %q", got, want)
	}
}

func TestChromeDevtoolsSocket(t *testing.T) {
	withStubRunner(t, sampleProcNetUnixChrome, nil)
	name, err := ChromeDevtoolsSocket(context.Background(), "R58N")
	if err != nil {
		t.Fatal(err)
	}
	if name != "chrome_devtools_remote" {
		t.Errorf("got %q", name)
	}
}

func TestChromeDevtoolsSocketError(t *testing.T) {
	withStubRunner(t, "boom", errors.New("exit 1"))
	if _, err := ChromeDevtoolsSocket(context.Background(), "R58N"); err == nil {
		t.Error("expected error")
	}
}

func TestChromeDevtoolsSocketNone(t *testing.T) {
	withStubRunner(t, sampleProcNetUnixNone, nil)
	if _, err := ChromeDevtoolsSocket(context.Background(), "R58N"); err == nil {
		t.Error("expected error when no socket present")
	}
}

func TestChromeDevtoolsSocketInfoChrome(t *testing.T) {
	withStubRunner(t, sampleProcNetUnixChrome, nil)
	info, err := ChromeDevtoolsSocketInfo(context.Background(), "R58N")
	if err != nil {
		t.Fatal(err)
	}
	if info.Name != "chrome_devtools_remote" {
		t.Errorf("name = %q", info.Name)
	}
	if info.Kind != SocketKindChrome {
		t.Errorf("kind = %v", info.Kind)
	}
}

func TestChromeDevtoolsSocketInfoWebView(t *testing.T) {
	withStubRunner(t, sampleProcNetUnixWebviewOnly, nil)
	info, err := ChromeDevtoolsSocketInfo(context.Background(), "R58N")
	if err != nil {
		t.Fatal(err)
	}
	if info.Kind != SocketKindWebView {
		t.Errorf("kind = %v, want webview", info.Kind)
	}
	if info.Name != "webview_devtools_remote_9876" {
		t.Errorf("name = %q", info.Name)
	}
}

// TestSentinelErrors exercises the exported error sentinels so callers can
// use errors.Is to match failure modes without parsing error strings.
func TestSentinelErrors(t *testing.T) {
	// ErrADBMissing: stub adbLookupFn so we don't need a real missing adb.
	origLookup := adbLookupFn
	adbLookupFn = func(string) (string, error) { return "", errors.New("exec: not found") }
	t.Cleanup(func() { adbLookupFn = origLookup })

	_, err := ListDevices(context.Background())
	if err == nil {
		t.Fatal("want error, got nil")
	}
	if !errors.Is(err, ErrADBMissing) {
		t.Errorf("errors.Is(err, ErrADBMissing) = false; err = %v", err)
	}

	// Restore lookup for the devtools socket test.
	adbLookupFn = origLookup

	// ErrNoDevtoolsSocket: /proc/net/unix with no matching line.
	withStubRunner(t, sampleProcNetUnixNone, nil)
	_, err = ChromeDevtoolsSocketInfo(context.Background(), "R58N")
	if err == nil {
		t.Fatal("want error, got nil")
	}
	if !errors.Is(err, ErrNoDevtoolsSocket) {
		t.Errorf("errors.Is(err, ErrNoDevtoolsSocket) = false; err = %v", err)
	}

	// ErrDeviceNotFound: sanity-check the sentinel is unique and non-nil so
	// downstream callers can rely on it existing in this package.
	if ErrDeviceNotFound == nil {
		t.Error("ErrDeviceNotFound is nil")
	}
	if errors.Is(ErrDeviceNotFound, ErrADBMissing) || errors.Is(ErrADBMissing, ErrNoDevtoolsSocket) {
		t.Error("sentinel errors must be distinct")
	}
}
