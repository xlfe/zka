"""Global Kitty watcher for managed zka workspace instances.

The watcher never performs remote control itself. It emits best-effort,
non-blocking hints; zkad debounces them and pulls Kitty's authoritative tree.
"""

from __future__ import annotations

import datetime
import json
import os
import socket
from typing import Any


def _window(args: tuple[Any, ...]) -> Any | None:
    for value in args:
        if hasattr(value, "id") and (
            hasattr(value, "user_vars") or hasattr(value, "child")
        ):
            return value
    return None


def _workspace(window: Any | None) -> str:
    if window is None:
        return os.environ.get("ZKA_WORKSPACE_ID", "")
    values = getattr(window, "user_vars", None) or {}
    return str(values.get("zka_workspace", ""))


def _pane(window: Any | None) -> str:
    if window is None:
        return os.environ.get("ZKA_PANE_ID", "")
    values = getattr(window, "user_vars", None) or {}
    return str(values.get("zka_pane", ""))


def _event_data(args: tuple[Any, ...]) -> dict[str, Any]:
    for value in reversed(args):
        if isinstance(value, dict):
            return value
    return {}


def _emit(kind: str, *args: Any) -> None:
    path = os.environ.get("ZKA_WATCHER_SOCKET", "")
    endpoint = os.environ.get("KITTY_LISTEN_ON", "")
    if not path or not endpoint:
        return
    window = _window(args)
    data = _event_data(args)
    payload = {
        "version": 1,
        "endpoint": endpoint,
        "workspace": _workspace(window),
        "kind": kind,
        "window_id": int(getattr(window, "id", 0) or 0),
        "pane_id": _pane(window),
        "confirmed": bool(data.get("confirmed", False)),
        "aborted": bool(data.get("aborted", False)),
        "timestamp": datetime.datetime.now(datetime.timezone.utc).isoformat(),
    }
    encoded = json.dumps(payload, separators=(",", ":")).encode("utf-8")
    sock = socket.socket(socket.AF_UNIX, socket.SOCK_DGRAM)
    try:
        sock.setblocking(False)
        sock.sendto(encoded, path)
    except (BlockingIOError, FileNotFoundError, ConnectionRefusedError, OSError):
        pass
    finally:
        sock.close()


def on_load(*args: Any) -> None:
    _emit("load", *args)


def on_resize(*args: Any) -> None:
    _emit("resize", *args)


def on_close(*args: Any) -> None:
    _emit("close", *args)


def on_focus_change(*args: Any) -> None:
    _emit("focus", *args)


def on_title_change(*args: Any) -> None:
    _emit("title", *args)


def on_set_user_var(*args: Any) -> None:
    _emit("user-var", *args)


def on_tab_bar_dirty(*args: Any) -> None:
    _emit("tab-bar", *args)


def on_quit(*args: Any) -> None:
    _emit("quit", *args)
