# mobilebridge

A CDP (Chrome DevTools Protocol) bridge for Android Chrome. It lets any CDP client — Puppeteer, Playwright, browser-use, OpenClaw, or your own automation — drive a real Chrome instance running on a physical Android device over ADB, as if it were a local Chrome on port 9222.

On top of standard CDP, mobilebridge adds synthetic **touch gesture** commands (tap, swipe, pinch, long-press) that are translated to `Input.dispatchTouchEvent` calls so agents can interact with mobile-first web experiences properly.

## Release status

`mobilebridge` is the open-source Android device bridge in the VulpineOS
stack. The v0.1 line is intentionally narrow:

- Android only
- one active downstream CDP client per proxied page
- ADB-backed local forwarding
- gesture helpers, reconnect handling, device enrichment, and recording

It is designed to be useful both as:

1. a standalone command-line bridge for local or hosted Android devices
2. an embeddable Go package for VulpineOS or other CDP tooling

For release notes, see [CHANGELOG.md](CHANGELOG.md). For the release
checklist used for tags, see [RELEASING.md](RELEASING.md).
For host-process and Vulpine integration patterns, see
[docs/integration.md](docs/integration.md).
For the hosted Android device-farm and session-pool model, see
[docs/device-farm.md](docs/device-farm.md).

## What it does

```
  Your CDP client (Puppeteer, OpenClaw, etc.)
         │  ws://localhost:9222/devtools/page/<id>
         ▼
  ┌──────────────────────┐
  │   mobilebridge       │
  │   - /json/* HTTP API │
  │   - CDP WebSocket    │
  │   - Touch gestures   │
  └──────────────────────┘
         │  adb -s <serial> forward tcp:<adb-port> localabstract:chrome_devtools_remote
         ▼
  Android Chrome on device
```

1. Discovers connected Android devices via `adb devices -l`.
2. Locates the Chrome devtools abstract socket (`chrome_devtools_remote` or `webview_devtools_remote_<pid>`) via `/proc/net/unix`.
3. Sets up an ADB port forward to it.
4. Serves Chrome's `/json/version`, `/json/list`, `/json/new` endpoints and proxies `/devtools/page/<id>` WebSockets through to the device.
5. Intercepts synthetic `MobileBridge.*` gesture methods and dispatches real CDP touch events.

## Requirements

- `adb` on `$PATH` with the device authorized (`adb devices` shows `device`, not `unauthorized`).
- Chrome for Android with USB debugging enabled and at least one tab open (or use Chrome's remote debugging flag on rooted builds).
- Go 1.22+ to build from source.

## Install

```
go install github.com/VulpineOS/mobilebridge/cmd/mobilebridge@latest
```

Or clone and build:

```
git clone https://github.com/VulpineOS/mobilebridge.git
cd mobilebridge
go build ./cmd/mobilebridge
```

## CLI usage

List attached devices:

```
mobilebridge --list
```

Run the bridge for a specific device on a specific local port:

```
mobilebridge --device R58N12ABCDE --port 9222
```

If only one device is attached you can omit `--device`:

```
mobilebridge --port 9222
```

Point any CDP client at `http://localhost:9222` exactly as you would for a local desktop Chrome:

```
const browser = await puppeteer.connect({ browserURL: 'http://localhost:9222' });
```

Check device/socket readiness without starting the bridge:

```
mobilebridge --health --device R58N12ABCDE
```

Print enriched device information:

```
mobilebridge --devices
```

## CLI flags

| Flag             | Description                                                       |
| ---------------- | ----------------------------------------------------------------- |
| `--list`         | List attached devices with Chrome/WebView labeling and exit.      |
| `--device`       | Device serial to bind (auto-pick when only one device is ready).  |
| `--port`         | Local TCP port to serve CDP on (default `9222`).                  |
| `--watch`        | Continuously log device hotplug add/remove events.                |
| `--health`       | Print the resolved device + devtools socket state and exit `0`.   |
| `--auto-restart` | If the upstream Chrome drops, relaunch instead of exiting.        |
| `--devices`      | Print an enriched device list (Android version, SDK, RAM, battery) and exit. |
| `--screenrecord` | Start `adb screenrecord` on server start and pull the MP4 to this path on shutdown. |
| `--logcat`       | After bridge start, dump `adb logcat -d` filtered to Chrome/WebView tags. |

## Touch gestures

On top of raw CDP, mobilebridge exposes synthetic `MobileBridge.*` methods
over the same WebSocket. Any CDP client can call them directly:

```js
// Puppeteer / chrome-remote-interface
const client = await page.target().createCDPSession();
await client.send('MobileBridge.tap',       { x: 200, y: 400 });
await client.send('MobileBridge.swipe',     { fromX: 500, fromY: 1200, toX: 500, toY: 300, durationMs: 300 });
await client.send('MobileBridge.pinch',     { centerX: 540, centerY: 960, scale: 0.5 });
await client.send('MobileBridge.longPress', { x: 200, y: 400, durationMs: 800 });
```

Internally each call expands into a sequence of `Input.dispatchTouchEvent`
frames: `touchStart` -> interpolated `touchMove`s -> `touchEnd`.

## Touch gesture extensions

Standard CDP has `Input.dispatchTouchEvent`, but it's fiddly to drive interactive gestures by hand. mobilebridge exposes higher-level helpers as Go functions in `pkg/mobilebridge`:

```go
ctx := context.Background()
session, _ := mobilebridge.StartAttachedServer(ctx, "R58N12ABCDE", "127.0.0.1:9222")
defer session.Close()

p := session.Proxy
p.Tap(ctx, 200, 400)
p.Swipe(ctx, 500, 1200, 500, 300, 300)         // scroll up
p.Pinch(ctx, 540, 960, 0.5)                    // pinch out
p.LongPress(ctx, 200, 400, 800)
```

Each helper builds the correct sequence of `Input.dispatchTouchEvent` payloads (`touchStart` → `touchMove`s → `touchEnd`) and sends them over the proxied CDP connection.

## Network emulation

mobilebridge exposes `Network.emulateNetworkConditions` as a Go helper that
handles the "enable the Network domain first, then apply the throttle"
dance and converts user-friendly kilobits-per-second into Chrome's
bytes-per-second format:

```go
p.EmulateNetworkConditions(false, 200, 1600, 750) // ~3G: 200ms latency, 1.6 Mbps down, 750 kbps up
p.EmulateNetworkConditions(true,  0,   0,    0)   // offline
```

## Device enrichment

Beyond `adb devices -l`, you can enrich a `Device` with Android version,
SDK level, total RAM, and current battery percent. Enrich runs four cheap
`getprop`/`dumpsys`/`/proc/meminfo` reads and is best-effort per field:

```go
d := devices[0]
_ = d.Enrich(ctx)
fmt.Printf("android %s sdk %d ram %dMB battery %d%%\n",
    d.AndroidVersion, d.SDKLevel, d.RAM_MB, d.BatteryPercent)
```

Or from the CLI: `mobilebridge --devices`.

## Screen recording

The bridge can drive `adb shell screenrecord` in the background while
automation runs, then pull the MP4 back to your host on shutdown. It uses
a 3-minute cap (Android's own hard limit) and a 4 Mbps bitrate by default.
Start it either programmatically:

```go
_ = proxy.StartScreenRecording(ctx, "/tmp/run.mp4")
// ... automation ...
_ = proxy.StopScreenRecording(ctx)
```

or via the CLI: `mobilebridge --port 9222 --screenrecord /tmp/run.mp4`.

## Proxy lifecycle

An `*AttachedServer` owns the public HTTP server plus the `*Proxy` that
connects to the Android devtools socket. The usual shape is:

```go
ctx := context.Background()
session, err := mobilebridge.StartAttachedServer(ctx, serial, "127.0.0.1:9222")
if err != nil { log.Fatal(err) }
defer session.Close()

select {
case <-ctx.Done():
    // shut down
case <-session.Done():
    // upstream permanently lost (reconnect exhausted backoff) — rebuild
}
```

Lower-level callers can still compose `NewProxy`, `NewServer`, and
`Server.RunWithProxy` directly. Use a different ADB-forward port from the
public server port when doing that manually.

`Done()` returns a channel closed either by `Close()` or when `reconnect`
gives up after its escalating backoff. Before that, transient ADB forward
drops are recovered internally without tearing the Serve loop down.

## Embedding in host processes

If you are integrating mobilebridge into another Go service, the normal
entrypoint is `StartAttachedServer`:

```go
ctx := context.Background()
session, err := mobilebridge.StartAttachedServer(ctx, "R58N12ABCDE", "127.0.0.1:9222")
if err != nil {
	log.Fatal(err)
}
defer session.Close()

resp, err := http.Get("http://127.0.0.1:9222/json/version")
if err != nil {
	log.Fatal(err)
}
defer resp.Body.Close()
```

If your host already owns ADB port assignment, use
`StartAttachedServerWithADBPort`:

```go
session, err := mobilebridge.StartAttachedServerWithADBPort(
	ctx,
	"R58N12ABCDE",
	4567,
	"127.0.0.1:9222",
)
if err != nil {
	log.Fatal(err)
}
defer session.Close()
```

This keeps the package usable both as a local CLI and as a bridge
component inside larger device-farm or agent systems.

For the VulpineOS extension-adapter pattern and the recommended hosted
API worker shape, see [docs/integration.md](docs/integration.md).

## Sentinel errors

Callers can match specific failure classes with `errors.Is`:

| Error                           | Meaning                                                               |
| ------------------------------- | ---------------------------------------------------------------------- |
| `mobilebridge.ErrBusy`          | `Proxy.Serve` refused a second concurrent client (single-client MVP).  |
| `mobilebridge.ErrDeviceNotFound`| Operation targeted a serial that isn't attached.                      |
| `mobilebridge.ErrADBMissing`    | `adb` is not on `$PATH` — install platform-tools.                     |
| `mobilebridge.ErrNoDevtoolsSocket` | `/proc/net/unix` on the device has no `chrome_devtools_remote` or `webview_devtools_remote_<pid>` — Chrome isn't running or USB debugging isn't granted. |

## Design notes

- **No magic.** mobilebridge is a thin proxy. Everything it does could be scripted with `adb forward` + a raw WebSocket; the point is that it handles device discovery, reconnection, multi-device selection, and gesture ergonomics so you don't have to.
- **Stateless per connection.** The proxy keeps one upstream WebSocket to Chrome and pumps frames bidirectionally. Closing the client closes the upstream and tears down the forward.
- **Hotplug.** `WatchDevices` polls `adb devices` so tools built on top can react to devices appearing and disappearing.

## Limitations

- **Single client per page.** Each proxied `/devtools/page/<id>` WebSocket
  accepts exactly one downstream client at a time. A second connection gets
  a `503 Service Unavailable`. CDP sessions carry per-client state
  (outstanding request ids, enabled domains, target attachment), so honest
  multiplexing would need id remapping that isn't implemented yet. Run one
  automation client per device for now.
- **Android only.** iOS is not supported in this repo — see below.
- **Single upstream Chrome.** mobilebridge attaches to the first devtools
  abstract socket it finds (`chrome_devtools_remote` preferred, WebView
  fallback). If you need to target a specific WebView host on a device with
  several, use `adb forward` manually and point mobilebridge at the local
  port.

## Troubleshooting

Common ADB issues and how to unstick them:

- **`adb devices` shows `unauthorized`.** Unplug, replug, and accept the
  RSA fingerprint prompt on the device. On some OEMs you must accept the
  dialog every time the cable is reseated.
- **`adb devices` shows `offline`.** Run `adb kill-server && adb start-server`
  and replug. Android Studio / Scrcpy sometimes grabs the ADB server in a
  bad state.
- **`no chrome devtools socket found on device`.** Chrome for Android only
  exposes the socket when at least one tab is open in the foreground and
  USB debugging for Chrome is enabled (`chrome://inspect` -> Discover USB
  devices from a desktop Chrome once to prime it).
- **`adb forward` succeeds but `/json/version` 502s.** The forward is
  racing Chrome's socket becoming writable — retry after a second, or use
  `--auto-restart` to have mobilebridge handle it for you.
- **Stale forwards surviving a crash.** `adb -s <serial> forward --list`
  shows all current forwards; `adb -s <serial> forward --remove-all`
  nukes them.
- **Permission denied reading `/proc/net/unix`.** The device is in
  hardened mode. Rooted builds expose the socket directly; production
  phones rely on Chrome's `localabstract:chrome_devtools_remote` which
  does not need root.

## Testing

mobilebridge's unit tests stub `adb` via an overridable `commandRunner` so
the whole suite runs without a phone attached. Run it with the race
detector enabled — several tests exercise the reader/writer reconnect
goroutines and will catch regressions there only under `-race`:

```
go test ./... -race
```

The repo also includes repeated soak coverage for reconnect serialization,
busy-session enforcement, and `/json/list` cache invalidation to catch
edge-case regressions that single-shot tests can miss.

For a real-device smoke test, plug in an authorized Android phone with
Chrome open on any tab, then run the CLI against it end-to-end:

```
mobilebridge --list                                  # confirms adb sees the device
mobilebridge --serial <SERIAL> --port 9222 &          # starts the bridge
curl http://127.0.0.1:9222/json/version | jq .Browser # expect "Chrome/..."
```

Point a Puppeteer or OpenClaw instance at `ws://127.0.0.1:9222/...` and
drive a navigation to verify the CDP pump is alive. Ctrl-C the bridge to
tear down the adb forward when you're done.

## Compatibility

| Component                    | Versions known to work                      |
| ---------------------------- | -------------------------------------------- |
| Android Chrome (stable)      | 100+ — anything with USB debugging support.  |
| Android WebView              | System WebView 96+ with `setWebContentsDebuggingEnabled(true)`. |
| Android OS                   | 8.0+ (API 26). Older devices lack the abstract socket layout mobilebridge probes for. |
| `adb`                        | 1.0.41+ (Platform-Tools r28+). Older ADBs parse `devices -l` differently. |
| Go (build)                   | 1.22+.                                       |

Not supported: rooted-only devtools pipes, `chrome_devtools_remote_<uid>` fallback on hardened AOSP forks, Fire OS builds that strip the socket. PRs welcome.

## iOS

mobilebridge is Android-only. iOS Safari support is provided as part of the broader VulpineOS commercial offering; Apple's WebKit Remote Inspector Protocol is undocumented and version-fragile, so it lives behind that ecosystem rather than in this repo.

## License

MIT. See [LICENSE](LICENSE).
