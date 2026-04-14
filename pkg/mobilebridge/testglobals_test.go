package mobilebridge

import (
	"context"
	"sync"
	"testing"
	"time"
)

var testSerialMu sync.Mutex

func lockTestGlobals(t *testing.T) {
	t.Helper()
	testSerialMu.Lock()
	t.Cleanup(testSerialMu.Unlock)
}

func swapCommandRunner(t *testing.T, runner func(context.Context, string, ...string) ([]byte, error)) {
	t.Helper()
	testOverrideMu.Lock()
	orig := commandRunner
	commandRunner = runner
	testOverrideMu.Unlock()
	t.Cleanup(func() {
		testOverrideMu.Lock()
		commandRunner = orig
		testOverrideMu.Unlock()
	})
}

func swapADBLookupFn(t *testing.T, lookup func(string) (string, error)) {
	t.Helper()
	testOverrideMu.Lock()
	orig := adbLookupFn
	adbLookupFn = lookup
	testOverrideMu.Unlock()
	t.Cleanup(func() {
		testOverrideMu.Lock()
		adbLookupFn = orig
		testOverrideMu.Unlock()
	})
}

func swapReconnectBackoff(t *testing.T, backoff []time.Duration) {
	t.Helper()
	testOverrideMu.Lock()
	orig := reconnectBackoff
	reconnectBackoff = append([]time.Duration(nil), backoff...)
	testOverrideMu.Unlock()
	t.Cleanup(func() {
		testOverrideMu.Lock()
		reconnectBackoff = orig
		testOverrideMu.Unlock()
	})
}

func swapReconnectSwapHook(t *testing.T, hook func()) {
	t.Helper()
	testOverrideMu.Lock()
	orig := reconnectSwapHook
	reconnectSwapHook = hook
	testOverrideMu.Unlock()
	t.Cleanup(func() {
		testOverrideMu.Lock()
		reconnectSwapHook = orig
		testOverrideMu.Unlock()
	})
}
