# zka

`zka` makes one complete Kitty workspace the user-visible unit while keeping
every terminal pane alive in its own hidden `zmx` session.

```text
workspace "example-project"
├── Kitty topology: OS windows → tabs → splits
├── pane A → zmx session on devbox.example
├── pane B → zmx session on devbox.example
└── pane C → zmx session on devbox.example

attachments
├── devbox.example: dedicated Kitty instance (primary)
└── laptop.example: dedicated Kitty instance over SSH (mirror)
```

The remote path is deliberately composed from existing tools:

```text
local:  Kitty → zka → zmx
remote: Kitty → zka → OpenSSH over private network → zmx on the origin
```

There is no `zmosh`, `zosh`, custom network protocol, PTY migration, or outer
local `zmx` around an SSH connection. private network supplies reachability and stable
host names, OpenSSH supplies authentication and transport, and `zmx` remains the
only persistent PTY owner.

## Status

Version 0.2 implements the workspace-centric workflow:

- one dedicated Kitty process and remote-control socket per attachment;
- one automatically managed `zmx` backend per pane;
- topology-only Kitty templates;
- watcher-driven topology capture with a two-second reconciliation fallback;
- canonical manifests that restore only `zka pane`, never a foreground command;
- primary and mirror attachments with idempotent open and two-phase move;
- destination-initiated remote open/move over a supervised SSH control channel;
- remote state mirroring, lease revocation, reconnect, and full-snapshot resync;
- Codex lifecycle attention state, Kitty notifications, and important
  `ntfy-send` notifications;
- daemon-owned cancellation and deterministic worker shutdown.

Schema v1 is intentionally reset on first v0.2 start. It represented individual
process sessions and was never deployed; translating it into whole workspaces
would invent topology that did not exist.

## NixOS setup

```nix
{
  imports = [ inputs.zka.nixosModules.default ];

  services.zka = {
    enable = true;
    shell.command = [ "fish" ];
    zmx.package = pkgs.zmx;

    # Add whichever package provides your already-configured ntfy-send helper.
    # extraPackages = [ inputs.ntfy-send.packages.${pkgs.system}.default ];
  };
}
```

The module installs `zka`, Kitty, OpenSSH, the global Kitty watcher, a systemd
user service, and a shared JSON runtime configuration. It also owns
`/etc/codex/requirements.toml` when managed Codex hooks are enabled. Put other
system Codex requirements in `services.zka.codex.extraRequirements`; zka does
not disable user or project hooks.

Useful options include:

```nix
services.zka = {
  kitty.extraArgs = [ "--class" "managed-kitty" ];
  ssh.options = [
    "-o" "ServerAliveInterval=5"
    "-o" "ServerAliveCountMax=3"
    "-o" "BatchMode=yes"
  ];
  notifications.ntfyCommand = "ntfy-send";
};
```

`ntfy-send` authentication remains owned by the helper's existing
configuration. zka never reads or transports its token.

## Local workflow

Create a workspace without choosing a name:

```sh
zka kitty
```

Or provide a display name, starting directory, and safe Kitty options:

```sh
zka kitty --name example-project --cwd ~/example-project -- --class managed-kitty
```

Every ordinary new tab or split in that dedicated Kitty instance uses its
forced managed shell and becomes a new hidden zmx-backed pane. Closing the Kitty
views leaves the pane processes in zmx.

Workspace operations are intentionally workspace-level:

```sh
zka workspace list
zka workspace inspect example-project
zka workspace open example-project
zka workspace move example-project
zka workspace focus example-project --pane PANE_ID
zka workspace seen example-project
zka workspace detach example-project
```

Opening an already attached workspace focuses and reuses it. Restoration
recreates the logical OS-window/tab/split hierarchy, layout state, titles,
working directories, and active focus. It never saves or reruns `codex`, `nvim`,
or another foreground program; those processes are already alive in zmx.

## Topology templates

`--template` accepts a Kitty session file containing topology directives and
bare `launch` directives only. A four-pane example:

```text
new_tab work
layout splits
launch --location default
launch --location vsplit
launch --location hsplit
launch --location hsplit
focus
```

```sh
zka kitty --name quad --template ./quad.kitty-session
```

Program-bearing launches, unknown directives, and the reserved
`zka_workspace`, `zka_pane`, or `ZKA_*` variables are rejected. zka adds the
stable pane IDs and canonical attach commands itself.

## Remote workflow

Configure an ordinary OpenSSH host alias whose address resolves through your
network. The same zka version and a running `zkad` must exist on the origin.
Then, from the destination machine:

```sh
zka workspace list --origin devbox.example
zka workspace inspect devbox.example:example-project
zka workspace open devbox.example:example-project
zka workspace move devbox.example:example-project
```

`open` creates or focuses a mirror. `move` performs a two-phase handoff:

1. fetch origin revision R and register a preparing destination attachment;
2. create every destination Kitty pane;
3. require a fresh origin-side heartbeat from every SSH → zmx client;
4. confirm the logical topology and focus;
5. commit the primary lease at revision R;
6. only then revoke and close the old primary views.

If destination creation, SSH, revision validation, or pane readiness fails, the
new views are removed and the source remains untouched. Repeating open or move
reuses the deterministic node/workspace attachment instead of creating
duplicates.

The daemon keeps one long-lived control connection per origin:

```sh
ssh -T -- devbox.example exec zka remote-control
```

It uses a versioned, one-MiB-limited JSON-lines protocol for snapshots, state
events, readiness, lease commits, and revocations. Mutating handoff requests
are replay-safe if SSH drops after the origin acts but before its response
arrives. Pane terminal traffic is separate and attaches directly on the origin:

```sh
ssh -tt -- devbox.example exec zka remote-attach --workspace W --pane P --attachment A
```

OpenSSH server-alive checks detect dead TCP sessions. zka retries only SSH exit
status 255, with exponential backoff from 250 ms to 30 seconds, then reattaches
to the same zmx session. It never restarts a missing foreground process.

## Attention and notifications

Each pane records explainable agent evidence and one of `unknown`, `idle`,
`working`, `blocked`, `done`, or `error`. A workspace exposes the highest-priority
aggregate. Codex hooks identify the hidden pane through `ZKA_WORKSPACE_ID` and
`ZKA_PANE_ID`.

Kitty titles and local notifications reflect pane state. zka invokes
`ntfy-send` for every `blocked` or `error` transition and for `done` when the
pane has no attached view. Delivery is deduplicated and failures are retained
in `zka workspace inspect`.

## Suggested example-project integration

After enabling the module, make the normal Sway terminal binding create a
managed workspace:

```text
bindsym $mod+Return exec zka kitty
```

Keep Kitty's `new_window_with_cwd` and `new_tab_with_cwd` mappings; the dedicated
instance's forced shell routes each new pane through zka automatically. Point a
quad-terminal binding at a topology template instead of launching four
independent terminals.

Leave utility terminals such as `audio-mixer`, `work-log`, and `system-monitor` on plain Kitty
unless their processes should also persist. zka does not try to adopt an
already-running PTY after the fact.

## Boundaries

“Exact restore” means logical Kitty topology plus live terminal state. zka does
not promise pixel-perfect Sway placement, migration of terminal graphics, or
capture of an ordinary already-running PTY. Shared unmanaged Kitty processes
are outside the model; managed workspaces use dedicated Kitty instances so
every window can be identified and restored safely.

Run local or remote diagnostics with:

```sh
zka doctor
zka doctor --origin devbox.example
```
