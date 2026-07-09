# zka

`zka` is a planned companion layer for `zmx`, `zmosh`, kitty, and coding-agent
workflows.

The core idea: kitty windows should be disposable views onto persistent
`zmx`/`zmosh` sessions. `zka` owns the plumbing around those views: restoring
kitty layout, moving views between local and remote clients, tracking which
agent needs attention, and routing notifications back to the right kitty tab or
split.

Status: design draft. There is no implementation yet.

## Why

Coding agents are easiest to run in real terminals, but hard to supervise once
there are many of them. A useful system should answer questions like:

- Which agents are still working?
- Which agent is blocked on approval or input?
- Which one finished while I was in another tab?
- Can I click a notification and land on the exact kitty split that needs me?
- Can I detach from one machine and reattach the same work somewhere else?

`zmx` and `zmosh` solve the hard process/session side: the shell or agent lives
outside the terminal view and can survive local detach, remote reconnect, and
roaming. Kitty already solves the terminal UI side: OS windows, tabs, splits,
layouts, titles, bells, notifications, and remote control.

`zka` is the missing glue between those pieces.

## Why Not Herdr

Herdr is valuable prior art: it combines a terminal multiplexer, persistent
sessions, agent-state detection, notifications, themes, plugins, and a socket
API for agent workflows.

It is not the shape `zka` wants.

The motivating failure was practical: Herdr sat between the PTY and the terminal
and broke shell rendering/layout in my setup. For example, process compose output
became garbled. That points at the deeper design problem: a terminal-aware
multiplexer is an extra rendering layer in the path between the program and
kitty.

`zka` takes the opposite bet:

- Do not sit between the PTY and the terminal.
- Do not reimplement kitty's tabs, windows, splits, or rendering.
- Do not replace `zmx` or `zmosh` as the persistent session owner.
- Use kitty as the terminal UI.
- Use `zmx`/`zmosh` as the process/session layer.
- Add only the orchestration that neither side owns.

The useful Herdr ideas to borrow are agent awareness, a small state model,
explainable detection rules, lifecycle hooks where available, and "done" as a
seen/unseen layer over an idle transition.

## Why Not tmux or Zellij

`tmux` and `zellij` are also the wrong center of gravity for this project.

Kitty already has the windowing model I want to use: OS windows, tabs, splits,
layouts, per-window titles, tab templates, bells, desktop notifications, and a
remote-control API. Adding another terminal multiplexer inside kitty duplicates
that model and brings another rendering layer into the path.

`zka` should be a kitty-native companion, not a replacement terminal UI.

## Design

### Views, Not Migrated PTYs

`zka` does not migrate PTYs.

Each kitty split launches an attach command:

```sh
zmx attach "$session_id"
```

or, for a remote session:

```sh
zmosh attach "$remote_session_ref"
```

The process belongs to the `zmx`/`zmosh` daemon. Kitty owns only the view. Moving
work from one kitty instance to another means:

1. Snapshot the current kitty view layout.
2. Close or detach the old views if desired.
3. Recreate new kitty windows/tabs/splits.
4. Run `zmx attach` or `zmosh attach` in each recreated split.

The processes never move. The views are rebuilt around them.

### Stable Window Mapping

Every kitty window created by `zka` should be tagged with a user variable that
records the persistent session it displays.

Example shape:

```sh
kitten @ launch \
  --var zka_session="$session_id" \
  --title "$title" \
  -- zmx attach "$session_id"
```

That user variable becomes the join key when `zka` snapshots kitty state. Titles,
process names, and current directories are useful hints, but they are not stable
identity.

### Kitty Snapshot And Restore

Kitty's remote-control API can report the current OS-window/tab/window tree:

```sh
kitten @ --to "$KITTY_LISTEN_ON" ls
```

`zka` should persist the pieces needed to recreate the view:

- OS windows
- tabs
- split windows
- active/focused window and tab
- tab layout
- layout state and split bias
- titles
- kitty user variables, especially `zka_session`
- cosmetic hints such as cwd, command line, and dimensions

Restore should prefer generating a kitty session file for the bulk layout, then
use remote-control commands for final focus, resize, and state adjustments.

### Agent Awareness

Agent state should come from the most authoritative source available. Screen
scraping is useful, but it should be the fallback, not the foundation.

Priority order:

1. Native server/API events, such as OpenCode's HTTP/SSE server.
2. Structured agent protocols, especially ACP, when `zka` controls the launch.
3. Official SDK or app-server surfaces, such as Codex app-server or SDK.
4. Agent lifecycle hooks, such as Codex hooks or Claude Code hooks.
5. Wrapper-reported state from a `zka run -- <agent>` launcher.
6. Process and TTY inspection through `/proc`.
7. Bottom-buffer screen rules when nothing better exists.

The initial state model should stay small:

- `unknown`: not enough evidence
- `idle`: agent is present and waiting
- `working`: agent is actively processing a turn or running tools
- `blocked`: agent needs user input, approval, or intervention
- `done`: agent transitioned from working to idle and has not been seen
- `error`: agent failed or needs attention because of an error

`done` is not usually an agent-native state. It is a `zka` attention state:
working became idle, and the user has not focused or acknowledged that window
yet.

### Agent Integrations

Planned integration strategy:

- OpenCode: use the server and event stream directly.
- Codex: prefer app-server or SDK for sessions `zka` launches; use JSON output
  from `codex exec --json` for noninteractive jobs; use hooks for native
  interactive CLI sessions.
- Claude Code: use hooks for foreground interactive sessions; use documented CLI
  JSON surfaces for background sessions where available.
- ACP agents: use ACP as the structured control plane when the agent can be
  launched through an ACP adapter.
- Unknown agents: fall back to wrapper events, process detection, and manifest
  rules over recent terminal text.

Agent APIs move. Integrations should prefer schemas, event names, and capability
checks from the installed tool version where available. For example, Codex
app-server can generate version-specific TypeScript or JSON Schema artifacts for
the local Codex binary.

Every detector should be explainable. A debug command should be able to say:

```text
state=blocked
source=codex-hook
evidence=PermissionRequest
session=abc123
kitty_window=42
```

or, for a fallback rule:

```text
state=blocked
source=screen-rule
agent=claude
region=bottom_lines(5)
rule=permission_prompt
```

### Notifications And Focus

Kitty already has the important primitives:

- `KITTY_WINDOW_ID` identifies the originating kitty window.
- `KITTY_LISTEN_ON` points at the remote-control socket.
- `kitten notify` can show desktop notifications, wait for activation, and print
  which action was taken.
- `kitten @ focus-window --match "id:$KITTY_WINDOW_ID"` can focus the originating
  split.

A wrapper can turn an agent state change into an actionable notification:

```sh
choice="$(
  kitten notify \
    --urgency critical \
    --app-name "Agent" \
    --identifier "agent-$KITTY_WINDOW_ID" \
    --button "Approve" \
    --button "Deny" \
    --wait-for-completion \
    "Agent needs input" \
    "Waiting on your call in this kitty tab"
)"

case "$choice" in
  0|1|2)
    kitten @ --to "$KITTY_LISTEN_ON" \
      focus-window --match "id:$KITTY_WINDOW_ID"
    ;;
esac
```

For attached sessions, the agent can also emit kitty desktop notifications
through the terminal stream. For detached sessions, there is no terminal to render
that escape sequence, so `zka` also needs a direct notification path through the
local notification daemon or a small watcher process.

### Kitty UI State

`zka` should use kitty's native attention surfaces before building custom UI:

- per-window titles for split-level state
- tab titles for project/session-level state
- `bell_symbol` and activity indicators for cheap attention
- `bell_border_color` and window urgency for unfocused windows
- kitty user variables as machine-readable state
- optional custom `tab_bar.py` or `window_title_bar.py` once basic state is solid

The first useful UI can be simple: title prefixes, bell/urgency, and desktop
notifications. Custom coloring can come later.

## Possible Components

Names are placeholders.

- `zkad`: background daemon that stores view/session/agent state.
- `zka`: CLI for launch, attach, snapshot, restore, status, and debug commands.
- `zka-kitty`: kitty adapter for snapshot/restore/focus/title/user-var changes.
- `zka-agent`: agent-state collector and detector engine.
- `zka-notify`: notification bridge for kitty, Sway/Wayland notification daemons,
  and detached-session fallback alerts.
- `zka explain`: debug command that shows why a session has its current state.

Example command shape:

```sh
zka launch --name reviewer -- codex
zka launch --name impl -- claude
zka snapshot > zka-session.json
zka restore zka-session.json --to "$KITTY_LISTEN_ON"
zka status --json
zka seen "$session_id"
zka explain "$session_id"
```

## Target Environment

Initial target:

- Linux
- NixOS packaging
- Wayland/Sway
- kitty
- `zmx`
- `zmosh`
- desktop notification daemon such as `swaync` or `mako`

The design should keep the core daemon mostly independent from Sway. Sway mainly
matters for focus behavior, urgency styling, and notification review UX.

## Non-Goals

- No terminal emulator.
- No terminal multiplexer UI.
- No PTY/process migration.
- No CRIU-style checkpoint/restore.
- No dependency on tmux or zellij.
- No screen scraping when a structured state source exists.
- No attempt to restore terminal graphics protocol images in a new emulator.
- No promise to recover arbitrary in-terminal visual state after a detached
  session with no active client.

## Open Questions

- Implementation language: Rust, Go, or a small shell/Python prototype first.
- State store format and location.
- Whether ACP should be a first-class launch path from day one.
- How much of kitty restore should be session-file generation versus imperative
  remote-control replay.
- Whether custom kitty tab/window title rendering belongs in this repo or should
  stay as user config examples.
- Which agent gets the first complete native integration.

## References

- Kitty remote control: https://sw.kovidgoyal.net/kitty/remote-control/
- Kitty notify kitten: https://sw.kovidgoyal.net/kitty/kittens/notify/
- Kitty desktop notifications: https://sw.kovidgoyal.net/kitty/desktop-notifications/
- Agent Client Protocol: https://agentclientprotocol.com/
- OpenCode server: https://opencode.ai/docs/server/
- Codex app-server: https://developers.openai.com/codex/app-server
- Codex SDK: https://developers.openai.com/codex/sdk
- Codex hooks: https://developers.openai.com/codex/hooks
- Codex noninteractive mode: https://developers.openai.com/codex/noninteractive
- Claude Code hooks: https://code.claude.com/docs/en/hooks
- tmux-agent-status: https://github.com/samleeney/tmux-agent-status
- tmux-agent-indicator: https://github.com/accessd/tmux-agent-indicator
- tmux-agent-sidebar: https://github.com/hiroppy/tmux-agent-sidebar
- tmux-agent-state: https://github.com/Gentleman-Programming/tmux-agent-state
