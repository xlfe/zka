"""Parse a zka-rendered session file with Kitty's own parser and print JSON.

Run headlessly: this only imports kitty.session, which needs no display and no
running Kitty. It exists so the pure-Go model in kittymodel_test.go can be
checked against the real implementation instead of drifting away from it.

Usage: python3 kitty_oracle.py <session-file>
"""

from __future__ import annotations

import json
import sys

from kitty.options.types import Options
from kitty.session import parse_session


def main() -> int:
    with open(sys.argv[1], encoding="utf-8") as handle:
        raw = handle.read()
    windows = []
    # An empty environment with no OS fallback matches how zka escapes values:
    # every "$" is doubled, so nothing should ever be substituted.
    for session in parse_session(raw, Options(), environ={}):
        tabs = []
        for tab in session.tabs:
            entries = []
            for spec in tab.windows:
                window = getattr(spec, "window", spec)
                cmd = getattr(window, "cmd", None) or []
                entries.append(
                    {
                        "title": getattr(window, "window_title", None) or "",
                        "cwd": getattr(window, "cwd", None) or "",
                        "args": list(cmd),
                        "vars": dict(getattr(window, "watchers", {}) or {})
                        or dict(getattr(window, "user_vars", {}) or {}),
                    }
                )
            tabs.append(
                {
                    "name": tab.name,
                    "layout": tab.layout,
                    "enabled_layouts": list(tab.enabled_layouts or []),
                    "layout_state": tab.layout_state,
                    "windows": entries,
                }
            )
        windows.append(
            {
                "class": session.os_window_class,
                "name": session.os_window_name,
                "state": session.os_window_state,
                "focus_tab": session.focus_tab_spec,
                "focus_os_window": session.focus_os_window,
                "tabs": tabs,
            }
        )
    json.dump(windows, sys.stdout)
    return 0


if __name__ == "__main__":
    raise SystemExit(main())
