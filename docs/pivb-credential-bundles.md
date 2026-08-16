# PIVB credential bundles

ZKA can route PIVB subject-token mints as a workspace-owned credential
capability. The sandbox contract does not change: `pivb agent-session` still
delegates one fixed alias through one ephemeral socket, while its trusted-host
side talks to a stable ZKA socket for the selected workspace.

```text
sandbox fixed-alias session.sock
  → trusted pivb agent-session relay
  → stable workspace PIVB socket
  → local ZKA route or authenticated ZKA credential stream
  → provider-side forward.sock
  → networkless pivbd
  → YubiKey slot 9c
```

The route is semantic. It accepts only `POST /v1/mint` with a complete PIVB
forwarding protocol v3 request. PIVB and ZKA strictly decode the same versioned
request and response contract; a mixed-version deployment fails closed and
requires upgrading both peers. Both sides read the `version` field before strict
decoding so skew is reported as skew rather than as a malformed request, and a
PIVB manifest persisted under v2 fails closed until its bundle is released and
re-claimed. The route does not carry raw digests, APDUs, PINs, unlock requests,
PC/SC, configuration, STS calls, IAM calls, or Google access tokens.

## Configure the bundle

Declare the same bundle name and PIVB alias allowlist on each ZKA peer:

```nix
services.zka.credentials = {
  defaultBundle = "work";
  pivb.routingMode = "environment";
  # Default grant window claims from this node request; empty requests none.
  pivb.grantWindow = "";
  bundles.work.pivb = {
    enable = true;
    aliases = [ "ro" "deploy" ];
  };
};
```

`credentials.pivb.forwardSocket` may override the provider-side PIVB socket.
The default is `$XDG_RUNTIME_DIR/pivb/forward.sock`. The socket must remain a
trusted-host capability and must never be mounted into the agent sandbox.

Run pivbd with its forwarding endpoint enabled:

```fish
pivb --forward-socket="$XDG_RUNTIME_DIR/pivb/forward.sock" serve
```

The installed PIVB systemd unit enables that endpoint and ZKA's cooperative
card lease by default. The equivalent global PIVB flag is:

```text
--card-lease-socket=%t/zka/card-lease.sock
```

The PIVB unit stays networkless: both integrations use same-UID Unix sockets.
The unit is ordered after `zkad.service` without requiring it. Workspace-forwarded describe
and mint operations require the lease and fail closed if ZKA is unavailable.
Direct-local unlock and mint operations retry the lease socket briefly during a
zkad restart, then retain the pre-ZKA local behavior if the socket is absent.
An explicit lease denial or malformed lease response always fails closed. Do
not weaken `PrivateNetwork` or add AF_INET/AF_INET6.

ZKA creates the lease under its runtime directory (by default,
`$XDG_RUNTIME_DIR/zka/card-lease.sock`). Filtered OpenPGP
`PKSIGN`/`PKDECRYPT` and PIVB describe, PIN verification, and signing operations
hold the same exclusive lease. The PIVB sharing-violation recovery remains a
one-retry fallback while it owns the lease. Other programs that access
scdaemon/PCSC without ZKA are outside this cooperative protocol.

## Select a workspace route

For every PIVB-enabled managed backend ZKA removes ambient PIVB attachment
variables and installs one complete versioned tuple:

```text
PIVB_ATTACHMENT_MODE=route-required
PIVB_ROUTE_SOCKET=/absolute/stable/workspace/socket
PIVB_ATTACHMENT_PROTOCOL=1
```

PIVB protocol 1 carries both the socket and `route_required: true` through the
trusted-host WIF API. It rejects partial, malformed, unknown, or conflicting
policy before any local signer, PIN, touch notification, or card lookup. Route
transport failures are `PIVB_UNAVAILABLE`; a responding endpoint with an
invalid route protocol is `PIVB_CONFIG`; policy failures are
`PIVB_ROUTE_REQUIRED`. There is no route-less retry.

Before creating a backend, ZKA requires schema-1 output from `pivb capabilities
--format=json` with protocol 1 and `route-required`. It checks both zkad's
configured executable and the `pivb` selected by the launching user's `PATH`.
Unknown fields are forward-compatible, but an unknown schema or missing
capability fails closed.

A local launcher first inspects the workspace-owned route:

```fish
set endpoint_json (zka workspace credentials endpoint --json example-project)
```

The endpoint state covers the whole selected bundle, not only the PIVB socket.
It is `ready` only while every enabled capability and the remote provider
transport are available, the bundle includes PIVB, every stable route is
published, and every managed pane has the current credential environment.
Launchers handle the result as follows:

- `ready`: use the returned `socket`.
- `unclaimed`: run `activate-local --if-unclaimed --bundle work` and query the
  endpoint again.
- `degraded` with `source=local` and the requested bundle already selected: run
  unflagged `activate-local --bundle work` to restore ephemeral provider sources,
  then query the endpoint again.
- `degraded` for a remote source or a different selected bundle: fail closed and
  report `detail`; do not take over the provider automatically.
- Any unknown state: fail closed.

Creating or refreshing a local claim requires exactly one ready attachment owned
by this node, unless `--attachment` names one explicitly. It reads the live
slot-9c public identity through pivbd and pins its serial, JWK key ID, and DER
SubjectPublicKeyInfo. Re-running unflagged activation with a different live card
is an explicit repin and advances the workspace generation.

Pass only the stable route path to the trusted `agent-session` supervisor:

```fish
pivb agent-session \
  --route-socket "$pivb_route" \
  --alias ro \
  --source-label codex:agentic/ro \
  -- sbx codex
```

The four sandbox mounts remain `session.sock`, `credential.json`,
`session.json`, and the static `pivb-agent-subject-token` helper. The ZKA route,
`forward.sock`, `wif.sock`, `control.sock`, card-lease socket, PIVB config,
PC/SC socket, and YubiKey devices stay outside the sandbox.

The launcher may call `activate-local --if-unclaimed` on every start. When a
claim already exists, ZKA returns it unchanged before selecting a local
attachment, refreshing provider sessions, building a capability manifest,
probing SSH, OpenPGP, or PIVB, clearing provider sources, or repinning a card.
If the workspace is still unclaimed, ordinary local-owner and provider checks
apply.

Without `--if-unclaimed`, activation is an explicit local selection. It may
replace a remote provider or change the selected bundle, and it advances the
claim generation when the binding changes. `workspace attach
--claim-credentials` is likewise an explicit transfer operation. Transfers are
blocked while route-unsafe panes require explicit backend recreation.

A launcher may inspect:

```fish
zka workspace credentials endpoint --json example-project
```

The bare `endpoint` form exits nonzero unless the whole bundle is healthy and an
available PIVB route is published. JSON form remains available for inspecting
`unclaimed` and `degraded` states plus deterministic capability and recreation
details.

## Remote claim lifecycle

On the provider/card host, the claim command builds the PIVB
manifest from its local pivbd and sends it over the authenticated ZKA control
session:

```fish
zka workspace credentials claim --bundle work devbox:example-project
zka workspace credentials claim --bundle work --window 30m devbox:example-project
```

The origin queries its card-free PIVB policy and checks the provider, issuer,
alias targets, and enrolled serial/key ID before changing route state. It then
atomically replaces any local route with the provider-node route. Each mint is constrained to
the claim's alias allowlist and pinned card. ZKA injects authenticated origin,
workspace, bundle, generation, provider node, and operation
IDs. Provider-side pivbd then revalidates alias, target, audience, enrollment,
and pinned card identity before signing.

### The grant window on the claim

`--window` records an operator authorisation window on the claim itself, and
`credentials.pivb.grantWindow` supplies the default when the flag is omitted; an
explicit `--window 0` always closes it. The window is claim-anchored: it opens at
the claim's `UpdatedAt` and runs for `min(requested, max_grant_window_s)`, using
the maximum published in the claimed manifest. A window is refused at claim time
on a bundle that does not enable PIVB and on a provider publishing
`max_grant_window_s = 0`, rather than being recorded as a window that could never
open.

The daemon, never the pane, stamps the window into each forwarded mint's trusted
context: `window_s` as the operator *requested* it, plus the absolute
`window_deadline` the claim anchored it to, both or neither. The requested value
travels because the provider clamps against its own live configuration, which can
move underneath a claim; a window that has already closed is stamped as no window
at all, and the provider treats an expired deadline as windowless rather than as a
refusal. Both the local route and the remote credential stream stamp the same
pair, so the forward context a pane reads back describes the mint that actually
happened. What the provider granted comes back top-level, outside the forwarded
context, because binding a response to the active route replaces that context
wholesale.

A windowed claim always advances the claim generation, even when it is otherwise
identical to the claim it replaces. Renewal is re-granting: a provider re-reads a
claim only when its generation changes, so refreshing in place would leave it
authorising against a stale anchor.

Four paths retire a claim, and between them they reach the provider's pivbd on
whichever node is holding the reuse state. Two run where the card is because the
claim was locally provided: explicit `credentials release`, and detaching the
owning attachment. Two run on a provider serving a remote origin: observing a
`credentials_claim` or `credentials_release` it just served (purging up to the
new generation, which retires superseded grants and leaves the claim just made
reusable), and refreshing a workspace mirror in which a claim this node provides
has vanished or changed generation. All four call `POST /v1/invalidate`
best-effort and asynchronously — the claim change is already durable, so an
absent, older, or failing pivbd costs one journal line and never fails the
operation. A node with no pivbd socket is skipped silently. Origin-side pivbd independently checks
the returned JWT claims, SPKI, signature, lifetime, local enrollment, and the
active route's pinned serial/key/SPKI. The origin relay, rather than the remote
provider, stamps that route pin into the response envelope.

The stable workspace socket path does not change during takeover, so a live
fixed-alias agent session can use the new provider on its next subject-token
request. Existing Google access tokens are unaffected and remain valid until
their own expiry.

Every generation is owned by the ready attachment that made the claim.
Detaching that attachment atomically clears the whole bundle and advances the
generation; a transient control disconnect only degrades it, and detaching a
non-owner does nothing. Explicit release also removes the route:

```fish
zka workspace credentials release devbox:example-project
```

There is no implicit local or alternate-card fallback. The route remains
unavailable until another remote claim or an explicit local activation. A route
generation change closes active relay connections, so an in-flight mint fails
closed and is not retried automatically on another provider.

## Stage 2 blocker: enforced pane provenance

Protocol 1 authenticates the route response and fails closed when the inherited
policy is present, but the policy itself is cooperative. Cgroup membership
cannot upgrade it into adversarial pane identity. On the tested target host, a
same-UID process can write its PID into another user scope's `cgroup.procs`, so
matching the route peer's cgroup device/inode would let one workspace
impersonate another.

An inherited descriptor could act as an unforgeable per-pane second factor,
but it cannot cross the current backend chain. The target-host probes ran
[zmx 0.6.0](https://github.com/neurosnap/zmx/blob/v0.6.0/src/main.zig#L817-L825),
whose tagged source closes every inherited descriptor from 3 through 63 except
its own internal descriptors before it forks the PTY child. ZKA therefore
exposes no `cgroup-bound` or protocol-2 mode. Stage 2 is blocked on one of these
architectural changes:

- zmx adds an explicit, versioned pass-descriptor contract that ZKA can probe;
- ZKA starts panes inside process isolation that hides the user bus, Kitty,
  zkad, local PIVB, and other zmx session sockets while exposing mediated
  per-pane endpoints.

Until then, PIVB routing enforcement is PIVB-only and cooperative. SSH-agent
and OpenPGP workspace sockets are also routing endpoints, not same-UID security
boundaries.

Remote response read, size, and transport failures return `503
PIVB_UNAVAILABLE`. A response that arrives successfully but fails the origin's
route/card/context binding returns `502 PIVB_CONFIG`; provider errors such as
`PIVB_LOCKED` otherwise retain their original status and code.

## Timeout and audit contract

The executable credential-source budget remains 30 seconds. ZKA allows at most
25 seconds for provider forwarding, inside PIVB's existing signing and helper
deadlines. Remote transport, touch, and one PIVB signing retry must fit that
budget; timeout does not trigger provider or card fallback.

PIVB logs the bounded alias, target, card identity, agent-session source, and
ZKA routing context. The touch notification includes the authenticated origin
node, workspace, bundle, generation, and operation ID so the operator can
distinguish a remote workspace mint from a direct-local mint. It never logs the subject token, PIN, STS result, Google
access token, ID token, or Authorization header. Source labels and operation IDs
are audit correlation only; authorization comes from the fixed alias, bundle
policy, authenticated ZKA session, claim generation, and pinned card.

Every mint response carries what the provider actually granted, in top-level
`granted_window_s` and `granted_window_deadline`. They are absent when no window
applies: a mint that asked for none, and a claim whose window had already closed
when the mint arrived. A mint that asks for a window the provider does not grant
is refused with `403 PIVB_WINDOW_NOT_ALLOWED` rather than downgraded silently.

The origin announces a closing grant window twice: about two minutes before the
deadline, and again at expiry. The pre-notice is skipped for granted windows
shorter than four minutes, which would otherwise announce their own opening, and
a window first seen already closed is never announced at all, so a restarted
daemon does not report grants that ended while it was down. Both notices are
per workspace and per generation — a renewed grant is a new generation and earns
its own pair — and both honour the ordinary notification policy on each channel,
because missing one costs a touch and nothing else. They are journalled either
way as `PIVB grant window closing|expired workspace=… bundle=…`, which is the
only record a headless origin has.

A pane whose claimed PIVB manifest no longer matches the provider's bundle policy
or forwarding protocol — the shape a v2-era manifest has after a lockstep upgrade
— now receives `403 PIVB_CONFIG` telling it to release and re-claim the bundle.
Only the operator can repair that, so dropping the connection left the pane with a
transport error and no remedy to act on.
