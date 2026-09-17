from __future__ import annotations

import sys
import unittest
from pathlib import Path
from unittest.mock import patch

sys.path.insert(0, str(Path(__file__).parent))
import watcher


class WatcherPresentationFilterTest(unittest.TestCase):
    def setUp(self) -> None:
        watcher._presentation_write_until = 0.0

    def test_control_title_is_ignored(self) -> None:
        with patch.object(watcher, "_emit") as emit:
            watcher.on_title_change(object(), object(), {"title": "[!] shell", "from_child": False})
        emit.assert_not_called()

    def test_child_title_is_emitted(self) -> None:
        with patch.object(watcher, "_emit") as emit:
            watcher.on_title_change(object(), object(), {"title": "nvim", "from_child": True})
        emit.assert_called_once()

    def test_zka_state_is_ignored_but_readiness_is_emitted(self) -> None:
        with patch.object(watcher, "_emit") as emit:
            watcher.on_set_user_var(object(), object(), {"key": "zka_state", "value": "blocked"})
            emit.assert_not_called()
            watcher.on_set_user_var(object(), object(), {"key": "zka_ready", "value": "1"})
        emit.assert_called_once()

    def test_control_title_suppresses_its_tab_bar_burst_only(self) -> None:
        with patch.object(watcher, "_emit") as emit:
            watcher.on_title_change(object(), object(), {"title": "[!] shell", "from_child": False})
            watcher.on_tab_bar_dirty(object(), object(), {})
            emit.assert_not_called()
            watcher._presentation_write_until = 0.0
            watcher.on_tab_bar_dirty(object(), object(), {})
        emit.assert_called_once()


if __name__ == "__main__":
    unittest.main()
