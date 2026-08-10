# Sandboxed agent hook relays

`zka relay hooks` lets the existing managed Codex and Claude Code hooks report
lifecycle state from a filesystem-isolated sandbox without mounting the zkad
control socket or disclosing the real workspace and pane IDs. The Unix socket
path is an opaque capability for one supervised process and one fixed
workspace/pane pair.

## Launcher contract

Start the trusted host-side launcher under the relay. Workspace and pane
default to the managed pane's host environment, but may be explicit:

```fish
zka relay hooks --workspace "$ZKA_WORKSPACE_ID" --pane "$ZKA_PANE_ID" -- sbx codex
```

The child launcher inherits its host environment, with
`ZKA_HOOK_RELAY_SOCKET` set to the newly created host socket. This is
intentional: the trusted launcher may still need `ZKA_WORKSPACE_ID` for PIVB
routing, sandbox fingerprints, and other host-side mount decisions.

Before executing the untrusted process, the launcher must:

1. Bind-mount only `ZKA_HOOK_RELAY_SOCKET` at the fixed sandbox path
   `/run/zka-hook/capability.sock`.
2. Set `ZKA_HOOK_SOCKET=/run/zka-hook/capability.sock` inside the sandbox.
3. Construct the sandbox environment from an allowlist. Do not expose
   `ZKA_WORKSPACE_ID`, `ZKA_PANE_ID`, `ZKA_SOCKET`,
   `ZKA_HOOK_RELAY_SOCKET`, notification credentials, the session D-Bus, Kitty
   remote control, or any other host identity/control endpoint.

The bind mount and Unix permissions must preserve access by the relay owner's
UID. User-namespace mappings that present an unrelated or overflow UID need an
explicit ownership mapping. The relay does not use `SO_PEERCRED`: its authority
comes from the private `0700` host directory, the `0600` socket, and mounting
only that socket into the sandbox.

The existing managed hook configuration stays unchanged. `zka hook codex` and
`zka hook claude` give `ZKA_HOOK_SOCKET` exclusive precedence when it is set;
otherwise direct zkad delivery and unmanaged-shell no-op behavior remain as
before. Hooks always print `{}` and exit successfully, including when the relay
or zkad is unavailable.

If PIVB is also in use, compose the trusted supervisors and let the final
launcher mount both narrow capabilities:

```fish
set pivb_route (zka workspace credentials endpoint example-project)
zka relay hooks -- \
  pivb agent-session \
    --route-socket "$pivb_route" \
    --alias ro \
    --source-label codex:agentic/ro \
    -- sbx codex
```

Do not roll out the zka command as a sandbox feature until the launcher change
is deployed and verifies the fixed mount and environment allowlist.

The relay is process-owned rather than zkad-owned because its capability and
admission budget must end with this sandbox process. It is the component that
knows traffic crossed an untrusted boundary, so it can shed load before zkad's
global state lock and notification path. A daemon-owned stable route is useful
for PIVB, which must survive provider handoffs; retaining a hook route after its
sandbox exits would instead broaden its lifetime. Foreground supervision,
flock-based crash cleanup, stderr logging, and the doctor check provide the
corresponding lifecycle and observability here.

## Capability and protocol

Each invocation creates a random session below
`$ZKA_RUNTIME_DIR/hook-relays`, takes an exclusive `flock` for its lifetime,
and binds a `0600` socket without replacing an existing path. The relay starts
the launcher itself and keeps the Linux parent death signal tied to a locked OS
thread until the child is reaped. Targeted SIGTERM/SIGHUP are forwarded;
terminal SIGINT/SIGQUIT already reach the whole foreground process group and
are not forwarded a second time. Normal exit, SIGTERM, and SIGINT remove only
the socket inode created by that relay. A later relay startup removes stale
session metadata only after a 60-second grace and when the session lock is
unheld. It unlinks the socket only when the recorded inode still matches; if
the ownership record is missing or truncated, the unproven socket is left
untouched. `zka doctor` reports active, starting, or stale sessions without
connecting to them.

The private protocol is one EOF-terminated JSON object and no response:

```json
{
  "version": 1,
  "agent": "codex",
  "kind": "permission_request",
  "turn_id": "turn-123",
  "detail": "approval needed"
}
```

Only `codex` and `claude` are accepted. Event kinds are limited to
`session_start`, `user_prompt`, `permission_request`, `post_tool`, `stop`,
`agent_error`, and `session_end`. Unknown fields, trailing JSON values,
workspace/pane/source fields, daemon operations, process/backend fields,
unsupported values, malformed UTF-8, and messages over 4 KiB are rejected.
The host injects workspace and pane identity and derives the source from the
allowlisted agent. Turn IDs are at most 128 bytes and reject C0/C1 controls. The
managed hook shim omits a bad turn ID rather than losing its lifecycle event;
other clients are rejected. Details have controls removed, whitespace
normalized, and valid UTF-8 truncation at 180 bytes.

## Load and ordering bounds

The capability is still presented to a process assumed to be hostile, so the
relay bounds both parsing and daemon work:

- raw connections use a 100/second token bucket with burst 100, at most eight
  concurrent parsers, a 128-request reorder window, and a 100 ms read timeout;
- parsed valid noncritical events use a 10/second token bucket with burst 10;
- attention-critical events use an independent 10/second token bucket with
  burst 10, so noncritical floods cannot consume their allowance;
- exactly one zkad event is in flight, with a 500 ms upstream timeout;
- eight additional events may wait in an ordered FIFO. When full, the oldest
  noncritical event is evicted; a critical event is evicted only if every
  queued event is critical.

`permission_request`, `stop`, and `agent_error` use the reserved critical bucket
and are critical for queue admission; the remaining lifecycle kinds are
self-repairing state updates. Both classes still share exactly one upstream call
in flight and the same eight-slot FIFO bound, for a combined ceiling of 20
admitted events/second.

Connections are sequenced at `accept`. Parser results and event admission are
then handed off in that order, and the upstream FIFO preserves the order of
retained events. This is the strongest local guarantee available: an attacker
can still fill the kernel listen backlog before a legitimate connection.
Overload is dropped silently so agent execution remains best effort. Aggregate
drop counts and upstream availability/retired-pane transitions are written to
the relay's stderr, which the supervising journal should retain.
