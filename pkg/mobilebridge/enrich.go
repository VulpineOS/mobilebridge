package mobilebridge

import (
	"context"
	"errors"
	"fmt"
	"strconv"
	"strings"
)

// Enrich fills in AndroidVersion, SDKLevel, RAM_MB and BatteryPercent by
// shelling out to the device via adb. Individual lookups that fail are left
// at their zero values — the caller can rely on the Serial/State/Model/
// Product fields that were already parsed by ListDevices.
//
// This is best-effort: a single unreachable device or a locked screen can
// cause any of the four reads to return garbage. Enrich returns the last
// error encountered only if *every* read failed; a partial success returns
// nil.
func (d *Device) Enrich(ctx context.Context) error {
	if d == nil {
		return errors.New("mobilebridge: nil device")
	}
	if d.Serial == "" {
		return errors.New("mobilebridge: empty serial")
	}

	var firstErr error
	var okAny bool
	record := func(err error) {
		if err == nil {
			okAny = true
			return
		}
		if firstErr == nil {
			firstErr = err
		}
	}

	// Android version (e.g. "14").
	if out, err := commandRunner(ctx, adbPath, "-s", d.Serial, "shell",
		"getprop", "ro.build.version.release"); err == nil {
		raw := strings.TrimSpace(string(out))
		if raw != "" {
			d.AndroidVersion = raw
			okAny = true
		} else {
			record(errors.New("parse android version: empty getprop output"))
		}
	} else {
		record(err)
	}

	// SDK level (e.g. "34").
	if out, err := commandRunner(ctx, adbPath, "-s", d.Serial, "shell",
		"getprop", "ro.build.version.sdk"); err == nil {
		raw := strings.TrimSpace(string(out))
		if n, perr := strconv.Atoi(raw); perr == nil {
			d.SDKLevel = n
			okAny = true
		} else {
			record(fmt.Errorf("parse sdk level %q: %w", raw, perr))
		}
	} else {
		record(err)
	}

	// RAM via /proc/meminfo.
	if out, err := commandRunner(ctx, adbPath, "-s", d.Serial, "shell",
		"cat", "/proc/meminfo"); err == nil {
		d.RAM_MB = parseMemTotalMB(string(out))
		if d.RAM_MB > 0 {
			okAny = true
		} else {
			record(errors.New("parse meminfo: no MemTotal line"))
		}
	} else {
		record(err)
	}

	// Battery level via dumpsys battery.
	if out, err := commandRunner(ctx, adbPath, "-s", d.Serial, "shell",
		"dumpsys", "battery"); err == nil {
		if pct, ok := parseBatteryLevel(string(out)); ok {
			d.BatteryPercent = pct
			okAny = true
		} else {
			record(errors.New("parse dumpsys battery: no level line"))
		}
	} else {
		record(err)
	}

	if !okAny {
		if firstErr != nil {
			return firstErr
		}
		return errors.New("mobilebridge: enrich: no fields could be read")
	}
	return nil
}

// parseMemTotalMB extracts the MemTotal line from /proc/meminfo output,
// e.g. "MemTotal:        5879072 kB" → 5741 (MB, rounded down).
func parseMemTotalMB(meminfo string) int {
	for _, line := range strings.Split(meminfo, "\n") {
		line = strings.TrimSpace(line)
		if !strings.HasPrefix(line, "MemTotal:") {
			continue
		}
		fields := strings.Fields(line)
		if len(fields) < 2 {
			return 0
		}
		kb, err := strconv.Atoi(fields[1])
		if err != nil {
			return 0
		}
		return kb / 1024
	}
	return 0
}

// parseBatteryLevel extracts the "level:" line from `dumpsys battery` output.
// Typical shape:
//
//	Current Battery Service state:
//	  AC powered: false
//	  USB powered: true
//	  level: 87
//	  scale: 100
//
// Returns the int percentage and ok=true if a level line was found.
func parseBatteryLevel(dumpsys string) (int, bool) {
	for _, line := range strings.Split(dumpsys, "\n") {
		line = strings.TrimSpace(line)
		if !strings.HasPrefix(line, "level:") {
			continue
		}
		rest := strings.TrimSpace(strings.TrimPrefix(line, "level:"))
		n, err := strconv.Atoi(rest)
		if err != nil {
			return 0, false
		}
		return n, true
	}
	return 0, false
}
