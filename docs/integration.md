# Integrating mobilebridge

This document shows the intended host-process integration patterns for
`mobilebridge`.

For the hosted Android fleet model built on top of these primitives, see
[device-farm.md](device-farm.md).

## What the package returns

The core helper is `StartAttachedServer`:

```go
session, err := mobilebridge.StartAttachedServer(ctx, serial, "127.0.0.1:9222")
if err != nil {
	return err
}
defer session.Close()
```

It gives you an `*AttachedServer` with:

- `Serial`: the Android device serial
- `Addr`: the local server listen address
- `Endpoint`: the normalized public HTTP endpoint, e.g.
  `http://127.0.0.1:9222`
- `Proxy`: the underlying proxy instance
- `Done()`: a channel closed when reconnect attempts are exhausted

For most host processes, `session.Endpoint` is the only value that
matters: it behaves like a local desktop Chrome `browserURL`.

## Device listing

List visible Android devices before allocating a session:

```go
devices, err := mobilebridge.ListDevices(ctx)
if err != nil {
	return err
}
for _, d := range devices {
	fmt.Printf("%s %s %s\n", d.Serial, d.State, d.Model)
}
```

If you need richer metadata, call `Enrich` on selected devices:

```go
if len(devices) > 0 {
	_ = devices[0].Enrich(ctx)
}
```

## Host-process pattern

The recommended host-process shape is:

1. resolve the target Android device
2. start an attached server on a local loopback port
3. hand `session.Endpoint` to your CDP client
4. close the session when the client is done

Example:

```go
ctx := context.Background()

session, err := mobilebridge.StartAttachedServer(ctx, "R58N12ABCDE", "127.0.0.1:9222")
if err != nil {
	log.Fatal(err)
}
defer session.Close()

browser, err := puppeteer.Connect(puppeteer.ConnectOptions{
	BrowserURL: session.Endpoint,
})
if err != nil {
	log.Fatal(err)
}
defer browser.Close()
```

If your host already owns the ADB forward port allocation, use
`StartAttachedServerWithADBPort`.

## VulpineOS integration pattern

`VulpineOS` integrates `mobilebridge` through its public
`internal/extensions.MobileBridge` interface.

The current adapter does two things:

1. `ListDevices(ctx)` maps `mobilebridge.Device` into the generic
   `extensions.MobileDevice` shape
2. `Connect(ctx, udid)` starts `StartAttachedServer`, stores a cleanup
   callback, and returns a generic `MobileSession{CDPEndpoint: ...}`

That keeps `mobilebridge` public and Android-specific, while letting
`VulpineOS` treat Android and iOS bridges through one generic surface.

Conceptually:

```go
devices, _ := extensions.Registry.Mobile().ListDevices(ctx)
session, _ := extensions.Registry.Mobile().Connect(ctx, devices[0].UDID)
defer extensions.Registry.Mobile().Disconnect(ctx, session.ID)

fmt.Println(session.CDPEndpoint)
```

The returned `CDPEndpoint` can then be consumed by the same automation
layer that drives desktop browsers.

## Vulpine API integration pattern

The paid API should treat `mobilebridge` as a session provider, not as
an endpoint implementation detail.

Recommended service shape:

1. allocate or choose an Android device
2. call `StartAttachedServer` in the worker process, or expose the hosted
   worker-control API when the worker is managed remotely
3. store the resulting `session.Endpoint` with the job/session record
4. hand that endpoint to the browser automation worker
5. close the attached server on job completion or worker shutdown

In other words, the API owns:

- job/session lifecycle
- worker placement
- billing and auth
- persistence

while `mobilebridge` owns only:

- ADB device discovery
- devtools socket resolution
- local CDP proxying
- reconnect behavior
- gesture extensions

## Hosted worker-control pattern

When the worker process is not the same process as the control plane,
`mobilebridge` can expose a narrow HTTP control API instead:

```bash
mobilebridge --worker-control 127.0.0.1:7788
```

Current endpoints:

- `POST /sessions` with `{ "device_id": "..." }`
- `DELETE /sessions/{id}`
- `POST /sessions/{id}/targets`
- `POST /sessions/{id}/recording/start`
- `POST /sessions/{id}/recording/stop`
- `GET /recordings/{id}/content`
- `DELETE /recordings/{id}`
- `GET /health`

This lets a control plane such as `vulpine-api` keep session leases,
tenant auth, and placement logic in one place while the worker host owns
the actual ADB attach and local CDP bridge lifecycle.

When the worker also receives:

- `--worker-heartbeat-url`
- `--worker-id`
- `--worker-token`
- `--worker-control-token`
- `--worker-advertise-url`

it can self-register directly with the control plane. The published
heartbeat includes current device inventory plus worker load fields such
as `active_sessions`, `queue_depth`, `max_sessions`, `failure_rate`, and
`last_error`.

When `--worker-control-token` is set, the worker-control mutation routes,
recording downloads, and recording deletes require `Authorization:
Bearer ...` from the control plane. That keeps attach, release, target
creation, recording control, and artifact cleanup off unauthenticated
private-network surfaces.

## Failure handling

When embedding `mobilebridge`, check these cases explicitly:

- `ErrADBMissing`
- `ErrDeviceNotFound`
- `ErrNoDevtoolsSocket`
- `ErrBusy`

Operationally:

- treat `Done()` closing as permanent session loss
- for transient drops, let the built-in reconnect path recover first
- always `Close()` the attached server to remove forwards and stop the
  local HTTP server

## When to use lower-level primitives

Use `NewProxy` + `NewServer` directly only when you need custom process
ownership or a nonstandard networking shape.

For almost all integrations:

- use `StartAttachedServer`
- consume `session.Endpoint`
- close the session when done
