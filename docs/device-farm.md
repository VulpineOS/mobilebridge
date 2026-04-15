# MobileBridge Device Farm and Session Pool Spec

This document scopes the hosted Android-device model for `mobilebridge`
when it is used behind a larger control plane such as VulpineOS or the
paid Vulpine API.

It is a planning and integration spec, not a promise that every
described control-plane feature already exists inside this repo.

## Goals

- host a pool of real Android Chrome devices behind a stable service
- allocate short-lived CDP sessions safely to workers or API jobs
- expose health and capacity state without leaking device internals to
  downstream clients
- make failure handling explicit enough for reliable hosted operation

## Non-goals

- multi-tenant concurrent use of one attached browser target
- cross-worker reuse of a live local loopback endpoint
- iOS orchestration in this public repo
- billing, auth, or tenant storage logic inside `mobilebridge`

## Current primitive

`mobilebridge` already provides the low-level primitive the hosted model
needs:

```go
session, err := mobilebridge.StartAttachedServer(ctx, serial, "127.0.0.1:9222")
```

That call creates a local attached session for one Android device and
returns:

- a public `Endpoint`
- a `Done()` channel for permanent upstream loss
- a `Close()` path that tears down the server and ADB forward cleanly

The hosted design should treat that attached server as an ephemeral
worker-local lease, not as a globally shared network service.

## Control-plane model

The recommended control plane has three record types.

### Device

Represents a physical Android phone or emulator.

Suggested fields:

- `device_id`
- `serial`
- `state`: `discovered`, `ready`, `reserved`, `attached`, `draining`, `offline`
- `model`
- `android_version`
- `sdk_level`
- `last_seen_at`
- `last_healthy_at`
- `capabilities`: browser socket, webview socket, screen recording
- `worker_id`
- `labels`: region, rack, usb-hub, reliability tier

### Session lease

Represents one allocated client session against one device.

Suggested fields:

- `session_id`
- `device_id`
- `tenant_id`
- `worker_id`
- `status`: `allocating`, `attached`, `releasing`, `expired`, `failed`
- `endpoint`
- `created_at`
- `expires_at`
- `released_at`
- `failure_reason`

### Worker

Represents the host process that can see a set of USB-attached devices.

Suggested fields:

- `worker_id`
- `hostname`
- `advertise_addr`
- `device_count`
- `active_sessions`
- `queue_depth`
- `max_sessions`
- `failure_rate`
- `last_error`
- `healthy`
- `last_heartbeat_at`

## Allocation model

Use a lease-based allocator.

Recommended rules:

1. one active lease per device by default
2. device must be `ready` before allocation
3. allocator reserves the device before starting `StartAttachedServer`
4. lease is owned by the worker that created the local loopback endpoint
5. downstream callers receive a worker-routable endpoint or a worker-owned
   action surface, never a raw ADB concept

Selection order:

1. required capability filters
2. explicit device id, if requested
3. sticky reuse for the same tenant or workflow when safe
4. lowest recent failure rate
5. oldest idle device

## Session lifecycle

The normal lifecycle is:

1. `discovered` device appears from ADB
2. health probe promotes it to `ready`
3. allocator marks it `reserved`
4. worker starts `StartAttachedServer`
5. successful attach promotes device to `attached` and lease to `attached`
6. client uses the returned CDP endpoint
7. release path calls `Close()`
8. janitor clears the lease and returns device to `ready`

If attach fails:

1. lease becomes `failed`
2. device returns to `ready` or `offline` depending on health result
3. the failure is recorded with the attempted socket type and error

If the proxy `Done()` channel closes:

1. lease becomes `failed`
2. device becomes `offline` pending fresh health probes
3. allocator should not hand the device out again until a probe passes

## Health model

Hosted mobile work fails when the control plane cannot distinguish
"attached but flaky" from "gone". Use a three-layer health model.

### Device discovery health

Cheap checks:

- `adb devices -l` reports the device as `device`
- the target serial still exists

### Browser socket health

Medium checks:

- devtools socket classification succeeds
- `chrome_devtools_remote` or a valid webview socket is present
- `/json/version` responds after `StartAttachedServer`

### Session readiness health

Expensive checks, run sparingly:

- `/json/list` returns at least one target
- optional `CreateTarget` probe in a canary workflow

The allocator should use cheap and medium checks in the steady state and
reserve expensive checks for bootstrapping, canaries, or devices with a
recent failure streak.

## Operational limits

Use these limits unless real measurements justify widening them.

- one attached lease per device
- one worker-local HTTP server per lease
- short session TTLs with explicit renewals at the control-plane layer
- bounded reconnect attempts before marking the lease failed
- no assumption that a local `127.0.0.1` endpoint is portable across
  workers

For a first hosted rollout, the simplest safe model is:

- one job or operator session per device
- no oversubscription
- release immediately after the job completes

## Failure handling

Expected failure classes:

- ADB disappears
- Chrome devtools socket is gone
- device is connected but unauthorized
- attached server starts but `/json/version` never becomes healthy
- proxy reconnect exhausts retries

Recommended control-plane behavior:

- mark the lease failed
- mark the device unavailable until the next successful health probe
- increment a device failure counter
- move repeated offenders to `draining`
- keep release idempotent

## Integration with Vulpine API

The current API work already matches the intended shape:

- inventory endpoint for device listing
- attach endpoint for session allocation
- release endpoint for explicit teardown
- target creation and recording actions on active Android sessions

The missing hosted pieces are control-plane concerns around those
primitives:

- allocator policy
- worker heartbeats
- lease TTL and renewal
- janitor cleanup for stale leases
- readiness scoring and draining

The public repo should document those behaviors here, while product-level
auth, billing, and tenant persistence stay in the API service.

## Recommended implementation order

1. worker heartbeat and device registry
2. persistent lease records with TTL and stale-session cleanup
3. allocator filters and sticky reuse
4. health score plus draining state
5. operator metrics and audit surfaces

## Metrics worth tracking

- devices discovered
- devices ready
- allocation latency
- attach success rate
- reconnect recoveries
- reconnect exhaustion count
- average session duration
- release cleanup failures
- device failure streak

## Public boundary

This repo should stay Android-only and public.

Safe to document here:

- Android pooling model
- ADB/Chrome socket assumptions
- worker-local attached server lifecycle
- hosted allocation and health concepts

Not for this repo:

- private mobile device implementations
- private product internals
- tenant secrets or credential flows
