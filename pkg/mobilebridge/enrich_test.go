package mobilebridge

import (
	"context"
	"strings"
	"testing"
	"time"
)

const sampleMemInfo = `MemTotal:        5879072 kB
MemFree:         1234567 kB
MemAvailable:    2345678 kB
Buffers:            1234 kB
`

const sampleDumpsysBattery = `Current Battery Service state:
  AC powered: false
  USB powered: true
  Wireless powered: false
  Max charging current: 0
  Max charging voltage: 0
  Charge counter: 2500000
  status: 2
  health: 2
  present: true
  level: 87
  scale: 100
  voltage: 4200
  temperature: 300
  technology: Li-ion
`

func TestParseMemTotalMB(t *testing.T) {
	if got := parseMemTotalMB(sampleMemInfo); got != 5741 {
		t.Errorf("parseMemTotalMB = %d, want 5741", got)
	}
	if got := parseMemTotalMB(""); got != 0 {
		t.Errorf("parseMemTotalMB(empty) = %d, want 0", got)
	}
	if got := parseMemTotalMB("MemTotal:\n"); got != 0 {
		t.Errorf("parseMemTotalMB(malformed) = %d, want 0", got)
	}
}

func TestParseBatteryLevel(t *testing.T) {
	n, ok := parseBatteryLevel(sampleDumpsysBattery)
	if !ok {
		t.Fatal("want ok=true")
	}
	if n != 87 {
		t.Errorf("level = %d, want 87", n)
	}
	if _, ok := parseBatteryLevel("no level here"); ok {
		t.Error("want ok=false for missing level")
	}
}

// TestDeviceEnrich_ParsesGetprop walks Device.Enrich through a fake
// commandRunner that returns canned adb output for each of the four calls.
func TestDeviceEnrich_ParsesGetprop(t *testing.T) {
	lockTestGlobals(t)
	swapCommandRunner(t, func(ctx context.Context, name string, args ...string) ([]byte, error) {
		// Every call is "adb -s <serial> shell <...>". Match on the trailing
		// shell command to decide what to return.
		full := strings.Join(args, " ")
		switch {
		case strings.Contains(full, "getprop ro.build.version.release"):
			return []byte("14\n"), nil
		case strings.Contains(full, "getprop ro.build.version.sdk"):
			return []byte("34\n"), nil
		case strings.Contains(full, "cat /proc/meminfo"):
			return []byte(sampleMemInfo), nil
		case strings.Contains(full, "dumpsys battery"):
			return []byte(sampleDumpsysBattery), nil
		}
		return []byte(""), nil
	})

	d := &Device{Serial: "R58N12ABCDE", State: "device", Model: "SM_G960U"}
	if err := d.Enrich(context.Background()); err != nil {
		t.Fatalf("enrich: %v", err)
	}
	if d.AndroidVersion != "14" {
		t.Errorf("AndroidVersion = %q", d.AndroidVersion)
	}
	if d.SDKLevel != 34 {
		t.Errorf("SDKLevel = %d", d.SDKLevel)
	}
	if d.RAM_MB != 5741 {
		t.Errorf("RAM_MB = %d", d.RAM_MB)
	}
	if d.BatteryPercent != 87 {
		t.Errorf("BatteryPercent = %d", d.BatteryPercent)
	}
}

// TestEnrich_ParseFailureCounted verifies that when every adb shell call
// succeeds at the process level but its output is unparseable, Enrich treats
// the run as a failure (okAny stays false) and surfaces a parse error instead
// of silently returning nil.
func TestEnrich_ParseFailureCounted(t *testing.T) {
	lockTestGlobals(t)
	swapCommandRunner(t, func(ctx context.Context, name string, args ...string) ([]byte, error) {
		full := strings.Join(args, " ")
		switch {
		case strings.Contains(full, "getprop ro.build.version.release"):
			return []byte("   \n"), nil // empty after trim
		case strings.Contains(full, "getprop ro.build.version.sdk"):
			return []byte("notanumber\n"), nil
		case strings.Contains(full, "cat /proc/meminfo"):
			return []byte("nothing useful here\n"), nil
		case strings.Contains(full, "dumpsys battery"):
			return []byte("no level line here\n"), nil
		}
		return nil, nil
	})

	d := &Device{Serial: "R58N12ABCDE"}
	err := d.Enrich(context.Background())
	if err == nil {
		t.Fatal("expected error from Enrich when every parse fails, got nil")
	}
	// Must mention at least one parse failure, not just a command error.
	msg := err.Error()
	if !strings.Contains(msg, "parse") {
		t.Errorf("error %q does not mention a parse failure", msg)
	}
	if d.AndroidVersion != "" || d.SDKLevel != 0 || d.RAM_MB != 0 || d.BatteryPercent != 0 {
		t.Errorf("no fields should be populated: %+v", d)
	}
}

// TestEnrich_HungCmdDoesntBlockOthers verifies that if one of the four adb
// shell lookups blocks, the per-call timeout fires and the other three still
// populate their fields. The whole Enrich must complete well under the sum
// of (all four individually hung) timeouts.
func TestEnrich_HungCmdDoesntBlockOthers(t *testing.T) {
	origTimeout := enrichPerCallTimeout
	lockTestGlobals(t)
	t.Cleanup(func() {
		enrichPerCallTimeout = origTimeout
	})
	enrichPerCallTimeout = 200 * time.Millisecond

	swapCommandRunner(t, func(ctx context.Context, name string, args ...string) ([]byte, error) {
		full := strings.Join(args, " ")
		switch {
		case strings.Contains(full, "getprop ro.build.version.release"):
			return []byte("14\n"), nil
		case strings.Contains(full, "getprop ro.build.version.sdk"):
			// Block until the per-call ctx is cancelled.
			<-ctx.Done()
			return nil, ctx.Err()
		case strings.Contains(full, "cat /proc/meminfo"):
			return []byte(sampleMemInfo), nil
		case strings.Contains(full, "dumpsys battery"):
			return []byte(sampleDumpsysBattery), nil
		}
		return nil, nil
	})

	d := &Device{Serial: "R58N12ABCDE"}
	start := time.Now()
	err := d.Enrich(context.Background())
	elapsed := time.Since(start)

	// Because three of four lookups succeed, Enrich should return nil (okAny
	// is true) even though the fourth timed out.
	if err != nil {
		t.Fatalf("Enrich returned %v; expected nil since three fields succeeded", err)
	}
	// Must finish within ~6s (well under the 5s default even with scheduling
	// jitter — we lowered the per-call timeout to 200ms for this test).
	if elapsed > 6*time.Second {
		t.Errorf("Enrich took %v; per-call timeout must unblock siblings faster", elapsed)
	}
	if d.AndroidVersion != "14" {
		t.Errorf("AndroidVersion = %q, want 14", d.AndroidVersion)
	}
	if d.RAM_MB != 5741 {
		t.Errorf("RAM_MB = %d, want 5741", d.RAM_MB)
	}
	if d.BatteryPercent != 87 {
		t.Errorf("BatteryPercent = %d, want 87", d.BatteryPercent)
	}
	if d.SDKLevel != 0 {
		t.Errorf("SDKLevel = %d, want 0 (hung call must not fill it)", d.SDKLevel)
	}
}

func TestDeviceEnrich_EmptySerial(t *testing.T) {
	d := &Device{}
	if err := d.Enrich(context.Background()); err == nil {
		t.Error("expected error for empty serial")
	}
}
