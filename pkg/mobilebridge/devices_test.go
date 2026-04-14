package mobilebridge

import (
	"context"
	"os/exec"
	"sync"
	"testing"
	"time"
)

// scriptedRunner returns a commandRunner that yields consecutive stdout
// strings from a script on each invocation. After the script is exhausted
// the last entry is repeated. The returned *int is the call counter,
// guarded by the same mutex as the runner itself.
func scriptedRunner(script []string) (func(ctx context.Context, name string, args ...string) ([]byte, error), *int) {
	var mu sync.Mutex
	idx := 0
	runner := func(ctx context.Context, name string, args ...string) ([]byte, error) {
		mu.Lock()
		defer mu.Unlock()
		i := idx
		if i >= len(script) {
			i = len(script) - 1
		}
		idx++
		return []byte(script[i]), nil
	}
	return runner, &idx
}

func withStubbedADB(t *testing.T, runner func(ctx context.Context, name string, args ...string) ([]byte, error)) {
	t.Helper()
	origInterval := watchInterval
	swapCommandRunner(t, runner)
	swapADBLookupFn(t, func(string) (string, error) { return "/fake/adb", nil })
	watchInterval = 5 * time.Millisecond
	t.Cleanup(func() {
		watchInterval = origInterval
		_ = exec.ErrNotFound
	})
}

func collectEvents(t *testing.T, ch <-chan DeviceEvent, want int, timeout time.Duration) []DeviceEvent {
	t.Helper()
	var got []DeviceEvent
	deadline := time.After(timeout)
	for len(got) < want {
		select {
		case ev, ok := <-ch:
			if !ok {
				return got
			}
			got = append(got, ev)
		case <-deadline:
			return got
		}
	}
	return got
}

// drainUntilClosed reads from ch until it is closed or the timeout fires,
// so the WatchDevices goroutine (which closes the channel on ctx cancel)
// has definitely exited before the test's t.Cleanup swaps commandRunner
// back. Returns all observed events.
func drainUntilClosed(t *testing.T, ch <-chan DeviceEvent, timeout time.Duration) []DeviceEvent {
	t.Helper()
	var out []DeviceEvent
	deadline := time.After(timeout)
	for {
		select {
		case ev, ok := <-ch:
			if !ok {
				return out
			}
			out = append(out, ev)
		case <-deadline:
			t.Error("drainUntilClosed: channel not closed within timeout")
			return out
		}
	}
}

// TestWatchDevices_NoDuplicateAdds feeds the same device across multiple
// ticks and asserts we emit exactly one Added event, not one per tick.
func TestWatchDevices_NoDuplicateAdds(t *testing.T) {
	lockTestGlobals(t)
	const line = "List of devices attached\nSERIAL_A    device usb:1 product:p model:M transport_id:1\n"
	runner, _ := scriptedRunner([]string{line, line, line, line, line})
	withStubbedADB(t, runner)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	ch, err := WatchDevices(ctx)
	if err != nil {
		t.Fatalf("watch: %v", err)
	}

	// Collect for a few ticks then cancel and drain until the producer
	// goroutine closes the channel.
	time.Sleep(40 * time.Millisecond)
	cancel()

	events := drainUntilClosed(t, ch, 500*time.Millisecond)
	var addedCount int
	for _, ev := range events {
		if ev.Type == DeviceAdded && ev.Device.Serial == "SERIAL_A" {
			addedCount++
		}
	}
	if addedCount != 1 {
		t.Errorf("want 1 Added event for SERIAL_A, got %d", addedCount)
	}
}

// TestWatchDevices_ProperlyHandlesRemoves feeds [A], [], [A] and expects
// the full add/remove/add flicker to be reported in order.
func TestWatchDevices_ProperlyHandlesRemoves(t *testing.T) {
	lockTestGlobals(t)
	const withA = "List of devices attached\nSERIAL_A    device usb:1 product:p model:M transport_id:1\n"
	const empty = "List of devices attached\n"
	runner, _ := scriptedRunner([]string{withA, empty, withA, withA, withA})
	withStubbedADB(t, runner)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	ch, err := WatchDevices(ctx)
	if err != nil {
		t.Fatalf("watch: %v", err)
	}

	events := collectEvents(t, ch, 3, 500*time.Millisecond)
	if len(events) < 3 {
		t.Fatalf("want 3 events, got %d: %+v", len(events), events)
	}
	if events[0].Type != DeviceAdded || events[0].Device.Serial != "SERIAL_A" {
		t.Errorf("events[0] = %+v, want Added SERIAL_A", events[0])
	}
	if events[1].Type != DeviceRemoved || events[1].Device.Serial != "SERIAL_A" {
		t.Errorf("events[1] = %+v, want Removed SERIAL_A", events[1])
	}
	if events[2].Type != DeviceAdded || events[2].Device.Serial != "SERIAL_A" {
		t.Errorf("events[2] = %+v, want Added SERIAL_A", events[2])
	}
	cancel()
	drainUntilClosed(t, ch, 500*time.Millisecond)
}

// TestWatchDevices_StateChange feeds a device that transitions from
// "unauthorized" to "device" and expects a remove+add pair.
func TestWatchDevices_StateChange(t *testing.T) {
	lockTestGlobals(t)
	const unauth = "List of devices attached\nSERIAL_A    unauthorized usb:1 transport_id:1\n"
	const ready = "List of devices attached\nSERIAL_A    device usb:1 product:p model:M transport_id:1\n"
	runner, _ := scriptedRunner([]string{unauth, ready, ready, ready})
	withStubbedADB(t, runner)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	ch, err := WatchDevices(ctx)
	if err != nil {
		t.Fatalf("watch: %v", err)
	}

	events := collectEvents(t, ch, 3, 500*time.Millisecond)
	if len(events) < 3 {
		t.Fatalf("want >=3 events, got %d: %+v", len(events), events)
	}
	// events[0]: Added (unauthorized)
	// events[1]: Removed (unauthorized, state changed)
	// events[2]: Added (device)
	if events[0].Type != DeviceAdded || events[0].Device.State != "unauthorized" {
		t.Errorf("events[0] = %+v", events[0])
	}
	if events[1].Type != DeviceRemoved {
		t.Errorf("events[1] = %+v want Removed", events[1])
	}
	if events[2].Type != DeviceAdded || events[2].Device.State != "device" {
		t.Errorf("events[2] = %+v want Added/device", events[2])
	}
	cancel()
	drainUntilClosed(t, ch, 500*time.Millisecond)
}

// TestWatchDevices_CtxCancellation asserts the output channel is closed
// promptly after the context is canceled.
func TestWatchDevices_CtxCancellation(t *testing.T) {
	lockTestGlobals(t)
	const line = "List of devices attached\nSERIAL_A    device usb:1 product:p model:M transport_id:1\n"
	runner, _ := scriptedRunner([]string{line})
	withStubbedADB(t, runner)

	ctx, cancel := context.WithCancel(context.Background())
	ch, err := WatchDevices(ctx)
	if err != nil {
		t.Fatalf("watch: %v", err)
	}
	// Drain the initial Added so the sender isn't blocked.
	select {
	case <-ch:
	case <-time.After(200 * time.Millisecond):
	}
	cancel()
	drainUntilClosed(t, ch, 500*time.Millisecond)
}
