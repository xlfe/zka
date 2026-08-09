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
forwarding protocol v2 request. PIVB and ZKA strictly decode the same versioned
request and response contract; a mixed-version deployment fails closed and
requires upgrading both peers. The route does not carry raw digests, APDUs,
PINs, unlock requests, PC/SC, configuration, STS calls, IAM calls, or Google
access tokens.

## Configure the bundle

Declare the same bundle name and PIVB alias allowlist on each ZKA peer:

```nix
services.zka.credentials = {
  defaultBundle = "work";
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

A local launcher explicitly activates the local card. The operation is
workspace-scoped and idempotent when the bundle and live card are unchanged:

```fish
zka workspace credentials activate-local --if-unclaimed --bundle work example-project
set pivb_route (zka workspace credentials endpoint example-project)
```

Activation reads the live slot-9c public identity through pivbd and pins its
serial, JWK key ID, and DER SubjectPublicKeyInfo. It refuses to replace a route
owned by a remote attachment. Re-running it with a different live card is an
explicit repin and advances the workspace generation.

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

The launcher may call `activate-local --if-unclaimed` on every start. An existing
local or remote route is returned unchanged without probing or repinning a card.
Without `--if-unclaimed`, the command is an explicit local repin and refuses to
replace a remote attachment. A launcher may also inspect:

```fish
zka workspace credentials endpoint --json example-project
```

The bare `endpoint` form exits nonzero unless the route is observed ready and
its listener is published. JSON form remains available for inspecting
`unclaimed`, `starting`, and `degraded` state plus the last listener error.

## Remote claim lifecycle

On the attachment/card host, the existing claim command builds the PIVB
manifest from its local pivbd and sends it over the authenticated ZKA control
session:

```fish
zka workspace credentials claim --bundle work devbox:example-project
```

The origin queries its card-free PIVB policy and checks the provider, issuer,
alias targets, and enrolled serial/key ID before changing route state. It then
atomically replaces any local route with the attachment route. Each mint is constrained to
the claim's alias allowlist and pinned card. ZKA injects authenticated origin,
workspace, bundle, generation, provider node, provider attachment, and operation
IDs. Provider-side pivbd then revalidates alias, target, audience, enrollment,
and pinned card identity before signing. Origin-side pivbd independently checks
the returned JWT claims, SPKI, signature, lifetime, local enrollment, and the
active route's pinned serial/key/SPKI. The origin relay, rather than the remote
provider, stamps that route pin into the response envelope.

The stable workspace socket path does not change during takeover, so a live
fixed-alias agent session can use the new provider on its next subject-token
request. Existing Google access tokens are unaffected and remain valid until
their own expiry.

Explicit release or owner-attachment detach removes the remote route:

```fish
zka workspace credentials release devbox:example-project
```

There is no implicit local or alternate-card fallback. The route remains
unavailable until another remote claim or an explicit local activation. A route
generation change closes active relay connections, so an in-flight mint fails
closed and is not retried automatically on another provider.

Remote response read, size, and transport failures return `503
PIVB_UNAVAILABLE`. A response that arrives successfully but fails the origin's
route/card/context binding returns `403 PIVB_CONFIG`; provider errors such as
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
