# Remote control authentication

The daemon's remote-control supervisor and credential-provider recovery share
one authentication policy per SSH alias. Internal `RemoteManager.Call` calls
can reuse a control session or an authenticated OpenSSH master. Local API calls
must explicitly carry `allow_authentication` to permit a fresh authentication
attempt. `API.RemoteCall` is for user actions; polling, cleanup and recovery use
`API.RemoteCallBackground`.

A user request grants one fresh attempt to a newly created client. Subsequent
supervisor attempts use `ControlMaster=no` and `ProxyCommand=false`, placed
before configured SSH options. OpenSSH tries its control socket before opening
the proxy, so a missing, stale or disappearing master cannot turn background
recovery into an agent signing request. This also blocks a configured jump-host
or proxy from starting authentication as a fallback. `BatchMode=yes` alone does
not prevent agent or hardware-token interaction.

Authentication failure, or failure to reuse a master, exposes
`authentication_required` with an explicit reconnect command. An initial caller
deadline or cancellation records the same state before removing the client.
The state survives client replacement attempts by background callers. A daemon
restart clears in-memory diagnostics, but its recovery calls still cannot
initiate fresh authentication. Host aliases are independent.

## Configuration limits

Background recovery requires a reusable `ControlPath`. With multiplexing
disabled, an explicit user connection is required after a dropped control
session. The suppression of `ProxyJump` can change a `ControlPath` containing
`%j` or `%C` (which includes the jump host), preventing reuse of that master.
An explicit `-J` in `ssh.options` conflicts with the background `ProxyCommand`
guard and also fails closed. Such configurations still permit explicit
connections; background recovery may require an alias-specific control path
without jump-host tokens and a `ProxyJump` directive in SSH configuration.

The policy applies to daemon remote-control connections. Pane SSH channels
have a separate lifecycle.

## Evidence and verification

The September 2026 incident established that `zka daemon` spawned the SSH
process asking the GPG agent to sign. It did not establish the initiating
recovery path or map the live executable to a source revision.

At repository revision `2988f3c`, an initial timeout removed its client without
recording a failure. Credential-provider recovery could therefore observe
`idle` and create another client. Separately, creating a client unconditionally
cleared a recorded terminal failure, allowing reconciliation callers to restart
authentication. These are verified source paths, not proof of which path
produced the incident's observed cadence.

The regression helper in `internal/zka/remote_auth_test.go` models SSH signing
attempts, refused signing, unanswered authentication, caller cancellation,
master availability and connection loss. Tests exercise the real manager,
local API, supervisor processes and credential recovery, and check that an
explicit reconnect works after the state has been latched. The helper does not
exercise a physical token or validate OpenSSH's option parsing.

OpenSSH's [control-socket selection](https://github.com/openssh/openssh-portable/blob/V_10_2_P1/ssh.c#L1596),
[proxy/jump option precedence](https://github.com/openssh/openssh-portable/blob/V_10_2_P1/readconf.c#L1467)
and [connection hash](https://github.com/openssh/openssh-portable/blob/V_10_2_P1/ssh.c#L1425)
provide the source basis for the reuse guard and its configuration limits.
