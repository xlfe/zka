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

Version 0.5 implements the workspace-centric workflow:

- an external, keyboard-first Gio launcher for choosing local/remote workspaces
  or creating a named workspace before Kitty starts;
- one dedicated Kitty process and remote-control socket per attachment;
- one automatically managed `zmx` backend per pane;
- topology-only Kitty templates;
- watcher-driven topology capture with a two-second reconciliation fallback;
- canonical manifests that restore only `zka pane`, never a foreground command;
- primary and mirror attachments with idempotent attach and two-phase move;
- destination-initiated remote attach/move over a supervised SSH control channel;
- remote state mirroring, lease revocation, reconnect, and full-snapshot resync;
- explicit workspace rename and kill operations, locally and over SSH;
- view-owned zmx lifecycle cleanup with durable retry after partial failures;
- dead-backend placeholders for partial failures and automatic reclamation when
  a workspace has no surviving zmx sessions;
- same-directory tab/window creation through Kitty's last reported shell CWD;
- Codex lifecycle state with a streaming Waybar/Sway attention surface, an
  attention-only Gio popup, and configurable desktop/`ntfy-send` notifications;
- daemon-owned cancellation and deterministic worker shutdown.

State schemas v1 and v2 are intentionally reset on first v0.3 start because v3
changes process ownership. The reset removes zka state and generated Kitty
sessions, but does not kill old zmx sessions. Those sessions become unmanaged
orphans and can be inspected or removed directly with zmx. Existing Kitty
processes are not signalled by the reset either.

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
  attention.states = [ "blocked" "error" "done" ];
  kitty.extraArgs = [ "--class" "managed-kitty" ];
  ssh.options = [
    "-o" "ServerAliveInterval=5"
    "-o" "ServerAliveCountMax=3"
    "-o" "BatchMode=yes"
  ];
  notifications = {
    desktopEnabled = true;
    ntfyEnabled = true;
    ntfyCommand = "ntfy-send";
  };
};
```

`ntfy-send` authentication remains owned by the helper's existing
configuration. zka never reads or transports its token.

## Local workflow

Open the graphical workspace launcher from Sway or another desktop binding:

```sh
zka launch
```

The launcher runs as its own Gio/Wayland process, outside every managed Kitty
pane. It offers local workspaces directly, accepts an SSH alias before listing
remote workspaces, and creates a workspace with an optional name. Kitty is not
started until a choice is made. The launcher deliberately composes the same
commands available below: `zka kitty` and `zka workspace attach`.

The home list groups local and previously connected remote workspaces into
`ATTACHED` and `DETACHED` sections. Selecting an attached workspace switches to
its existing Sway window; selecting a detached workspace recreates its Kitty
view. Use the row's `Detach` button or press `D` to close that view while
leaving its zmx sessions alive. Each row also shows the workspace-wide agent
state, detected agent processes, and its pane/tab/window counts.

Create a workspace without choosing a name:

```sh
zka kitty
```

Or provide a display name, starting directory, and safe Kitty options:

```sh
zka kitty --name example-project --cwd ~/example-project -- --class managed-kitty
```

Every ordinary new tab or split in that dedicated Kitty instance uses its
forced managed shell and becomes a new hidden zmx-backed pane. Closing a Kitty
split or tab removes the corresponding pane and kills its zmx session. Closing
the final pane, the managed OS window, or confirming Kitty quit kills the whole
workspace. If Kitty crashes or its socket becomes unreachable without a
confirmed close event, zka preserves the zmx sessions and marks the attachment
unhealthy.

If one zmx backend dies while other panes remain alive, a later attach restores
that pane as a static `zmx backend is dead` placeholder so the surviving panes
stay accessible. Press Ctrl-C in the placeholder to remove only that pane. If
the last zmx backend dies, the daemon closes any remaining managed Kitty view
and removes the workspace automatically.

Workspace operations are intentionally workspace-level:

```sh
zka workspace list
zka workspace inspect example-project
zka workspace attach example-project
zka workspace move example-project
zka workspace focus example-project --pane PANE_ID
zka workspace seen example-project
zka workspace detach example-project
zka workspace rename example-project shell-work
zka workspace kill shell-work
```

`detach` is the intentional persistence boundary: it closes the local Kitty
attachment while leaving every zmx session alive for a later `attach`. `kill` is
immediate and non-interactive. Cleanup state is persisted before zmx is
signalled, and failed kills are retried by the daemon until absence is
confirmed.

Attaching an already attached workspace focuses and reuses it. Restoration
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
zka workspace attach devbox.example:example-project
zka workspace move devbox.example:example-project
zka workspace rename devbox.example:example-project shell-work
zka workspace kill devbox.example:shell-work
```

`attach` creates or focuses a mirror. `move` performs a two-phase handoff:

1. fetch origin revision R and register a preparing destination attachment;
2. create every destination Kitty pane;
3. require a fresh origin-side heartbeat from every SSH → zmx client;
4. confirm the logical topology and focus;
5. commit the primary lease at revision R;
6. only then revoke and close the old primary views.

If destination creation, SSH, revision validation, or pane readiness fails, the
new views are removed and the source remains untouched. Repeating attach or move
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

OpenSSH server-alive checks detect dead TCP sessions. After a control connection
has completed its protocol handshake, zka retries SSH exit status 255 with
exponential backoff from 250 ms to 30 seconds, then reattaches to the same zmx
session. Exit 255 before the first handshake is treated as an authentication or
configuration failure instead. zkad logs the exit status and the last 8 KiB of
SSH stderr, and returns both to the caller. It never restarts a missing
foreground process; a missing backend becomes the same removable static
placeholder used locally.

### SSH agents and hardware-backed keys

Control SSH runs inside `zkad`, so it inherits the systemd user manager's
environment rather than the environment of the shell that opened the launcher.
Check the value visible to user services with:

```sh
systemctl --user show-environment | rg '^SSH_AUTH_SOCK='
```

If the interactive shell has the intended agent socket, import it before
restarting the daemon:

```sh
systemctl --user import-environment SSH_AUTH_SOCK DISPLAY WAYLAND_DISPLAY
systemctl --user restart zkad
```

The default `BatchMode=yes` prevents SSH from opening password, host-key, or
private-key passphrase prompts. Hardware-backed agent keys can still work when
the agent can complete signing from zkad's environment, including hardware token touch
or a separately configured graphical pinentry. zka has no controlling terminal
and does not relay PIN or touch prompts through the launcher. Unlock the key or
configure graphical pinentry first; an unavailable identity or refused signing
request is then reported from SSH stderr instead of appearing as a local socket
timeout. Use `journalctl --user-unit zkad` for the same bounded diagnostic.

## Attention and notifications

Each pane records explainable agent evidence and one of `unknown`, `idle`,
`working`, `blocked`, `done`, or `error`. A workspace exposes the highest-priority
aggregate. Codex hooks identify the hidden pane through `ZKA_WORKSPACE_ID` and
`ZKA_PANE_ID`.

Kitty titles and local notifications reflect pane state. By default, zka
invokes `ntfy-send` for every `blocked` or `error` transition and for `done`
when the pane has no attached view. Delivery is deduplicated and failures are
retained in `zka workspace inspect`.

`zka attention` exposes the same state as a live, attention-only queue. It is a
projection of what needs you now, not a notification history: resolved items
disappear automatically, and a finished pane disappears while that exact pane
is focused. The queue orders waiting panes first, then failures, then finished
work, oldest first within each state.

```sh
zka attention show             # Gio popup with only actionable panes
zka attention status           # one human-readable snapshot
zka attention status --json    # versioned machine-readable snapshot
zka attention focus-next       # attach/switch and focus the next exact pane
zka attention pause            # defer interruptions; agents keep running
zka attention resume
zka attention toggle
```

Pause is persistent across daemon restarts. It suppresses locally generated
desktop and ntfy notifications while agents and remote synchronization keep
running. The live pending count remains visible, and resuming delivers only
items that still need attention and were not delivered previously. Each remote
origin keeps its own pause and ntfy policy; a local pause is not propagated over
SSH.

### Waybar and Sway attention surface

Waybar can keep one streaming subscription to zkad; no polling interval or
per-update process is involved. Add `custom/zka` to the desired Waybar module
list and configure it like this:

```jsonc
"custom/zka": {
  "exec": "zka attention watch --waybar",
  "return-type": "json",
  "restart-interval": 2,
  "format": "zka {text}",
  "tooltip": true,
  "on-click": "zka attention show",
  "on-click-middle": "zka attention focus-next",
  "on-click-right": "zka attention toggle"
}
```

The module always prints a count, including `0`, so it remains a clickable
ambient entry point. It emits the CSS classes `clear`, `blocked`, `error`,
`done`, `paused`, and `unavailable`; for example:

```css
#custom-zka { color: #99a8b8; }
#custom-zka.blocked, #custom-zka.error { color: #ff8f91; }
#custom-zka.done { color: #6ed5c0; }
#custom-zka.paused { color: #7b8794; }
#custom-zka.unavailable { color: #d2a8ff; }
```

Every mouse action has a stable command for Sway or scripts:

```text
bindsym $mod+a exec zka attention show
bindsym $mod+Shift+a exec zka attention focus-next
bindsym $mod+Ctrl+a exec zka attention toggle
```

The attention popup updates from the same daemon stream. Up/Down selects an
item, Enter or a row click restores and focuses its exact local or remote pane,
`P` pauses or resumes interruptions, and Escape closes it. Its Wayland app ID
is the same stable `zka-launch` ID as the workspace launcher; its window title
is `zka attention` if a compositor rule needs to distinguish the two modes.

## Suggested example-project integration

After enabling the module, make the normal Sway terminal binding create a
workspace through the external launcher:

```text
bindsym $mod+Return exec zka launch
for_window [app_id="^zka-launch$"] floating enable, resize set width 680 px height 560 px, move position center
```

The Gio launcher exposes the stable Wayland app ID `zka-launch`, so the
`for_window` rule floats and centers only the launcher window.

Because Sway starts the launcher directly, attaching does not first create a
temporary managed terminal. Running `zka launch` from an existing pane is still
supported, but the compositor binding is the intended entry point.

Keep mappings such as `new_window_with_cwd` and `new_tab_with_cwd` (including a
custom Ctrl-T mapping). The managed instance redirects those action names
through Kitty's last OSC-7-reported directory before its forced shell routes the
new pane through zka. Point a quad-terminal binding at a topology template
instead of launching four independent terminals.

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
